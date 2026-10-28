package failover

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/util"
)

// subPathRule is the rule the API server applies to spec.subPath of a
// SubVolume. The executor applies it first, so a dry run finds what a real run
// would be refused for.
var subPathRule = regexp.MustCompile(v1alpha1.SubPathPattern)

const (
	// FailoverSourceLabel marks SubVolumes created by the failover CLI.
	FailoverSourceLabel = "storage.ibmcloud.io/failover-source"
)

// ExecuteOptions configures the failover execution.
type ExecuteOptions struct {
	// DRPoolName is the FileSharePool name on the DR cluster.
	DRPoolName string
	// DRShareIP is the NFS mount target IP on the DR cluster.
	DRShareIP string
	// DRExportPath is the NFS export path (optional, for VPC access mode).
	DRExportPath string
	// NamespaceOverride overrides the PVC namespace from metadata.
	NamespaceOverride string
	// DryRun prints what would happen without creating resources.
	DryRun bool
}

// Executor creates K8s resources on the DR cluster from a FailoverPlan.
type Executor struct {
	client client.Client
	out    io.Writer
}

// NewExecutor creates a new failover executor.
func NewExecutor(c client.Client, out io.Writer) *Executor {
	return &Executor{client: c, out: out}
}

// printf writes progress output. A failed write to the progress writer must not
// abort a failover that is already creating resources, so it is logged only.
func (e *Executor) printf(format string, args ...any) {
	if _, err := fmt.Fprintf(e.out, format, args...); err != nil {
		klog.V(4).InfoS("Failed to write progress output", "err", err)
	}
}

// Execute creates SubVolume CRs, PVs, and PVCs for each SubVolume in the plan.
func (e *Executor) Execute(ctx context.Context, plan *FailoverPlan, opts ExecuteOptions) error {
	for i := range plan.SubVolumes {
		sv := &plan.SubVolumes[i]
		if sv.Rewind {
			sv.SubPath = rewoundSubPath(sv.SubPath, sv.VMMoment)
		}
		if !subPathRule.MatchString(sv.SubPath) {
			return fmt.Errorf("failover SubVolume %s: folder %q is not accepted (it must match %s)", sv.SubVolumeName, sv.SubPath, v1alpha1.SubPathPattern)
		}
	}
	mountOptions, err := e.mountOptions(ctx, opts.DRPoolName)
	if err != nil {
		return err
	}
	for _, sv := range plan.SubVolumes {
		ns := sv.PVCNamespace
		if opts.NamespaceOverride != "" {
			ns = opts.NamespaceOverride
		}

		if err := e.executeOne(ctx, sv, ns, mountOptions, opts); err != nil {
			return fmt.Errorf("failover SubVolume %s: %w", sv.SubVolumeName, err)
		}
	}
	return nil
}

// mountOptions are the NFS options the pool's own PersistentVolumes are mounted
// with. The PVs made here point at the pool's share but are not made by the
// driver, so kubelet mounts them with exactly the options given to it. Without
// "sec=sys" every file on the share belongs to UID 99 and a VM cannot open its
// disk. A pool that cannot be read (no client, or not found) gives the defaults.
func (e *Executor) mountOptions(ctx context.Context, poolName string) ([]string, error) {
	if e.client == nil {
		return util.PoolMountOptions(nil), nil
	}
	pool := &v1alpha1.FileSharePool{}
	if err := e.client.Get(ctx, client.ObjectKey{Name: poolName}, pool); err != nil {
		if !errors.IsNotFound(err) {
			return nil, fmt.Errorf("read pool %s: %w", poolName, err)
		}
		e.printf("Pool %s not found; using the default mount options\n", poolName)
		return util.PoolMountOptions(nil), nil
	}
	return util.PoolMountOptions(pool.Spec.MountOptions), nil
}

func (e *Executor) executeOne(ctx context.Context, sv FailoverSubVolumePlan, namespace string, mountOptions []string, opts ExecuteOptions) error {
	svName := sv.SubVolumeName
	pvName := "dr-" + svName
	pvcName := sv.PVCName

	// 1. Create SubVolume CR.
	svCR := &v1alpha1.SubVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: svName,
			Labels: map[string]string{
				FailoverSourceLabel: sv.PolicyName,
			},
		},
		Spec: v1alpha1.SubVolumeSpec{
			PoolName:           opts.DRPoolName,
			ShareID:            "dr-share", // placeholder — DR share ID
			ShareMountTargetIP: opts.DRShareIP,
			ShareExportPath:    opts.DRExportPath,
			SubPath:            sv.SubPath,
			RequestedGB:        sv.RequestedGB,
			PVName:             pvName,
			PVCName:            pvcName,
			PVCNamespace:       namespace,
			UID:                sv.UID,
			GID:                sv.GID,
			Permissions:        sv.Permissions,
			ReclaimPolicy:      "Retain",
		},
	}

	// 2. Build PV spec.
	nfsPath := sv.SubPath
	if opts.DRExportPath != "" {
		nfsPath = opts.DRExportPath + sv.SubPath
	}

	pvObj := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: pvName,
		},
		Spec: corev1.PersistentVolumeSpec{
			Capacity: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse(fmt.Sprintf("%dGi", sv.RequestedGB)),
			},
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				NFS: &corev1.NFSVolumeSource{
					Server: opts.DRShareIP,
					Path:   nfsPath,
				},
			},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			MountOptions:                  mountOptions,
			ClaimRef: &corev1.ObjectReference{
				Name:      pvcName,
				Namespace: namespace,
			},
		},
	}

	// 3. Build PVC spec.
	storageRequest := resource.MustParse(fmt.Sprintf("%dGi", sv.RequestedGB))
	// An empty class, not a missing one: a missing class takes the cluster's
	// default StorageClass, and the claim then binds to nothing.
	noClass := ""
	pvcObj := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: namespace,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: storageRequest,
				},
			},
			VolumeName:       pvName,
			StorageClassName: &noClass,
		},
	}

	if opts.DryRun {
		e.printf("[dry-run] Would create SubVolume %s\n", svName)
		e.printf("[dry-run] Would create PV %s (nfs://%s%s, mount options %s)\n", pvName, opts.DRShareIP, nfsPath, strings.Join(mountOptions, ","))
		e.printf("[dry-run] Would create PVC %s/%s (bound to %s)\n", namespace, pvcName, pvName)
		return nil
	}

	// Create SubVolume CR (idempotent — skip if exists).
	if err := e.client.Create(ctx, svCR); err != nil {
		if !errors.IsAlreadyExists(err) {
			return fmt.Errorf("create SubVolume CR: %w", err)
		}
		e.printf("SubVolume %s already exists, skipping\n", svName)
	} else {
		e.printf("Created SubVolume %s\n", svName)
	}

	// Create PV (idempotent).
	if err := e.client.Create(ctx, pvObj); err != nil {
		if !errors.IsAlreadyExists(err) {
			return fmt.Errorf("create PV: %w", err)
		}
		e.printf("PV %s already exists, skipping\n", pvName)
	} else {
		e.printf("Created PV %s\n", pvName)
	}

	// Create PVC (idempotent).
	if err := e.client.Create(ctx, pvcObj); err != nil {
		if !errors.IsAlreadyExists(err) {
			return fmt.Errorf("create PVC: %w", err)
		}
		e.printf("PVC %s/%s already exists, skipping\n", namespace, pvcName)
	} else {
		e.printf("Created PVC %s/%s\n", namespace, pvcName)
	}

	return nil
}
