package failover

import (
	"context"
	"fmt"
	"io"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

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

// Execute creates SubVolume CRs, PVs, and PVCs for each SubVolume in the plan.
func (e *Executor) Execute(ctx context.Context, plan *FailoverPlan, opts ExecuteOptions) error {
	for _, sv := range plan.SubVolumes {
		ns := sv.PVCNamespace
		if opts.NamespaceOverride != "" {
			ns = opts.NamespaceOverride
		}

		if err := e.executeOne(ctx, sv, ns, opts); err != nil {
			return fmt.Errorf("failover SubVolume %s: %w", sv.SubVolumeName, err)
		}
	}
	return nil
}

func (e *Executor) executeOne(ctx context.Context, sv FailoverSubVolumePlan, namespace string, opts ExecuteOptions) error {
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
			ClaimRef: &corev1.ObjectReference{
				Name:      pvcName,
				Namespace: namespace,
			},
		},
	}

	// 3. Build PVC spec.
	storageRequest := resource.MustParse(fmt.Sprintf("%dGi", sv.RequestedGB))
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
			VolumeName: pvName,
		},
	}

	if opts.DryRun {
		fmt.Fprintf(e.out, "[dry-run] Would create SubVolume %s\n", svName)
		fmt.Fprintf(e.out, "[dry-run] Would create PV %s (nfs://%s%s)\n", pvName, opts.DRShareIP, nfsPath)
		fmt.Fprintf(e.out, "[dry-run] Would create PVC %s/%s (bound to %s)\n", namespace, pvcName, pvName)
		return nil
	}

	// Create SubVolume CR (idempotent — skip if exists).
	if err := e.client.Create(ctx, svCR); err != nil {
		if !errors.IsAlreadyExists(err) {
			return fmt.Errorf("create SubVolume CR: %w", err)
		}
		fmt.Fprintf(e.out, "SubVolume %s already exists, skipping\n", svName)
	} else {
		fmt.Fprintf(e.out, "Created SubVolume %s\n", svName)
	}

	// Create PV (idempotent).
	if err := e.client.Create(ctx, pvObj); err != nil {
		if !errors.IsAlreadyExists(err) {
			return fmt.Errorf("create PV: %w", err)
		}
		fmt.Fprintf(e.out, "PV %s already exists, skipping\n", pvName)
	} else {
		fmt.Fprintf(e.out, "Created PV %s\n", pvName)
	}

	// Create PVC (idempotent).
	if err := e.client.Create(ctx, pvcObj); err != nil {
		if !errors.IsAlreadyExists(err) {
			return fmt.Errorf("create PVC: %w", err)
		}
		fmt.Fprintf(e.out, "PVC %s/%s already exists, skipping\n", namespace, pvcName)
	} else {
		fmt.Fprintf(e.out, "Created PVC %s/%s\n", namespace, pvcName)
	}

	return nil
}
