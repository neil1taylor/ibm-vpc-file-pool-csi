package pool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/util"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/vmcopy"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// DefaultDriverNamespace is where the Helm chart installs the driver, and
	// where share data Jobs run.
	DefaultDriverNamespace = "kube-system"

	// shareJobLabel marks temporary resources created for share data Jobs.
	shareJobLabel = "storage.ibmcloud.io/share-data-job"

	// shareJobNameMax leaves room for the "-s0" suffix of PV and claim names.
	shareJobNameMax = 56

	shareJobBackoffLimit = int32(3)
)

// ErrShareJobFailed means a share data Job used up its retries. The Job has
// been removed, so the next call starts a new one.
var ErrShareJobFailed = errors.New("share data job failed")

var validPermissionsPattern = regexp.MustCompile(`^0?[0-7]{3}$`)

// ShareMount identifies the NFS export of one file share.
type ShareMount struct {
	MountTargetIP string
	ExportPath    string
}

// Ownership is applied to the top folder of a copy, or to every file in it
// when Recursive is set. Nil fields are left as copied.
type Ownership struct {
	UID         *int64
	GID         *int64
	Permissions string
	Recursive   bool
}

// ShareData removes and copies folders on file shares. Each call is
// idempotent for a given key: it starts the work if needed and reports
// done=true once the work has finished.
type ShareData interface {
	RemoveDir(ctx context.Context, key string, share ShareMount, path string) (bool, error)
	CopyDir(ctx context.Context, key string, src ShareMount, srcPath string, dst ShareMount, dstPath string, owner *Ownership) (bool, error)
	// ApplyVMDisk turns the staged QEMU copy volPath/stagingFile into the raw
	// disk dstPath/disk.img on the same share, then deletes the staging file.
	ApplyVMDisk(ctx context.Context, key string, share ShareMount, volPath, stagingFile, dstPath string, moment time.Time) (bool, error)
	// RestoreBackup writes the backup of one volume, as it was at the newest
	// moment at or before moment (the zero time asks for the latest), into the
	// volume folder dstPath on dst. It reports which moment it wrote.
	RestoreBackup(ctx context.Context, key string, backup ShareMount, volPath, momentsPath string, dst ShareMount, dstPath string, moment time.Time, owner *Ownership) (bool, RestoreOutcome, error)
	// Stop ends any work running for key. It reports done=true once
	// nothing runs for key any more.
	Stop(ctx context.Context, key string) (bool, error)
}

// ShareJobRunner implements ShareData with Kubernetes Jobs in the driver's
// namespace, because the controller pod has no NFS access.
type ShareJobRunner struct {
	client      client.Client
	namespace   string
	image       string
	driverImage string
}

// SetDriverImage sets the image of Jobs that run the driver itself. Without
// it, those Jobs are refused.
func (r *ShareJobRunner) SetDriverImage(image string) {
	r.driverImage = image
}

// jobShape is how a share data Job's pod differs from the usual root shell
// script: an optional init container that runs as root, and a main container
// that runs a command as a given user.
type jobShape struct {
	image      string
	command    []string
	runAsUser  int64
	initScript string
}

// NewShareJobRunner creates a runner that starts Jobs in namespace with image.
func NewShareJobRunner(c client.Client, namespace, image string) *ShareJobRunner {
	if image == "" {
		image = DefaultCloneImage
	}
	return &ShareJobRunner{client: c, namespace: namespace, image: image}
}

// RemoveDir removes path (a volume or snapshot folder) from share.
func (r *ShareJobRunner) RemoveDir(ctx context.Context, key string, share ShareMount, path string) (bool, error) {
	if err := validateSharePath(path); err != nil {
		return false, err
	}
	script := fmt.Sprintf("set -eu\nrm -r -f -- \"/share0%s\"\n", path)
	return r.run(ctx, key, []ShareMount{share}, r.scriptShape(script))
}

// CopyDir replaces dstPath on dst with a copy of srcPath on src, keeping
// owners and modes, then applies owner to the top folder.
func (r *ShareJobRunner) CopyDir(ctx context.Context, key string, src ShareMount, srcPath string, dst ShareMount, dstPath string, owner *Ownership) (bool, error) {
	if err := validateSharePath(srcPath); err != nil {
		return false, err
	}
	if err := validateSharePath(dstPath); err != nil {
		return false, err
	}
	if owner != nil && owner.Permissions != "" && !validPermissionsPattern.MatchString(owner.Permissions) {
		return false, fmt.Errorf("invalid permissions %q", owner.Permissions)
	}

	shares := []ShareMount{src}
	dstRoot := "/share0"
	if dst != src {
		shares = append(shares, dst)
		dstRoot = "/share1"
	}
	from := "/share0" + srcPath
	to := dstRoot + dstPath

	// If any step fails, the trap removes the partial copy.
	var b strings.Builder
	b.WriteString("set -eu\n")
	fmt.Fprintf(&b, "trap 'rm -r -f -- \"%s\"' EXIT\n", to)
	fmt.Fprintf(&b, "rm -r -f -- \"%s\"\n", to)
	fmt.Fprintf(&b, "mkdir -p \"%s\"\n", to)
	fmt.Fprintf(&b, "cp -a \"%s/.\" \"%s/\"\n", from, to)
	writeOwnerScript(&b, to, owner)
	b.WriteString("trap - EXIT\n")
	return r.run(ctx, key, shares, r.scriptShape(b.String()))
}

// writeOwnerScript adds the commands that give the folder to its owner.
func writeOwnerScript(b *strings.Builder, to string, owner *Ownership) {
	if owner == nil {
		return
	}
	if owner.UID != nil && owner.GID != nil {
		recursive := ""
		if owner.Recursive {
			recursive = "-R "
		}
		fmt.Fprintf(b, "chown %s%d:%d \"%s\"\n", recursive, *owner.UID, *owner.GID, to)
	}
	if owner.Permissions != "" {
		fmt.Fprintf(b, "chmod %s \"%s\"\n", owner.Permissions, to)
	}
}

// scriptShape is a Job that runs a shell script as root.
func (r *ShareJobRunner) scriptShape(script string) jobShape {
	return jobShape{image: r.image, command: []string{"/bin/sh", "-c", script}, runAsUser: 0}
}

// ApplyVMDisk runs the driver's apply-vm-disk mode on one share. An init
// container, as root, makes the snapshot folder. The main container runs as
// the QEMU user, because only that user can delete the staging file, and the
// new disk image must belong to it.
func (r *ShareJobRunner) ApplyVMDisk(ctx context.Context, key string, share ShareMount, volPath, stagingFile, dstPath string, moment time.Time) (bool, error) {
	if err := validateSharePath(volPath); err != nil {
		return false, err
	}
	if err := validateSharePath(dstPath); err != nil {
		return false, err
	}
	if !strings.HasPrefix(stagingFile, vmcopy.StagingPrefix) || strings.ContainsAny(stagingFile, "/\x00") {
		return false, fmt.Errorf("%q is not a staging file name", stagingFile)
	}
	if r.driverImage == "" {
		return false, errors.New("no driver image set for VM disk apply Jobs")
	}
	staging := "/share0" + volPath + "/" + stagingFile
	disk := "/share0" + dstPath + "/disk.img"
	// The Job is safe to run twice. A run after the staging file was applied and
	// removed (for example because the controller could not save that the
	// snapshot was ready) must leave the finished snapshot alone.
	run := fmt.Sprintf(`if [ ! -e "%s" ] && [ -f "%s" ]; then echo already applied; exit 0; fi
exec /usr/local/bin/vpc-file-pool-csi --mode=apply-vm-disk --staging-path="%s" --disk-path="%s" --to=%s --full`,
		staging, disk, staging, disk, moment.UTC().Format(time.RFC3339Nano))
	shape := jobShape{
		image:     r.driverImage,
		command:   []string{"/bin/sh", "-c", run},
		runAsUser: qemuUID,
		initScript: fmt.Sprintf("set -eu\nif [ -e \"%s\" ]; then\nrm -r -f -- \"/share0%s\"\nmkdir -p \"/share0%s\"\nchmod 0777 \"/share0%s\"\nfi\n",
			staging, dstPath, dstPath, dstPath),
	}
	return r.run(ctx, key, []ShareMount{share}, shape)
}

// run starts the Job for key, or reports on the Job that already exists.
func (r *ShareJobRunner) run(ctx context.Context, key string, shares []ShareMount, shape jobShape) (bool, error) {
	done, _, err := r.runMessage(ctx, key, shares, shape)
	return done, err
}

// runMessage is run, and it also returns the termination message of the Job's
// container when the Job has succeeded. It reads the message before the Job
// and its pods are removed.
func (r *ShareJobRunner) runMessage(ctx context.Context, key string, shares []ShareMount, shape jobShape) (bool, string, error) {
	jobName := shareJobName(key)
	job := &batchv1.Job{}
	err := r.client.Get(ctx, types.NamespacedName{Namespace: r.namespace, Name: jobName}, job)
	switch {
	case apierrors.IsNotFound(err):
		return false, "", r.start(ctx, jobName, shares, shape)
	case err != nil:
		return false, "", fmt.Errorf("get share Job %s: %w", jobName, err)
	case job.Status.Succeeded > 0:
		msg := r.terminationMessage(ctx, jobName)
		r.cleanup(ctx, jobName, len(shares))
		return true, msg, nil
	case job.Status.Failed > shareJobBackoffLimit || jobHasFailed(job):
		r.cleanup(ctx, jobName, len(shares))
		return false, "", fmt.Errorf("%w: %s", ErrShareJobFailed, jobName)
	default:
		return false, "", nil
	}
}

// jobHasFailed reports whether the Job has its Failed condition. A pod that
// restarts in place uses up the retries of a Job without raising the Job's
// failed count past the limit.
func jobHasFailed(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// terminationMessage returns what the container of a Job's pod wrote to its
// termination log, or "".
func (r *ShareJobRunner) terminationMessage(ctx context.Context, jobName string) string {
	pods := &corev1.PodList{}
	if err := r.client.List(ctx, pods, client.InNamespace(r.namespace), client.MatchingLabels{shareJobLabel: jobName}); err != nil {
		klog.V(4).InfoS("Could not list pods of a share data Job", "job", jobName, "err", err)
		return ""
	}
	for _, p := range pods.Items {
		for _, cs := range p.Status.ContainerStatuses {
			if cs.State.Terminated != nil && cs.State.Terminated.Message != "" {
				return cs.State.Terminated.Message
			}
		}
	}
	return ""
}

// podSecurityContext runs a share data Job as uid. Root Jobs set no group, as
// before; a Job for another user runs with that user's group too.
func podSecurityContext(uid int64) *corev1.PodSecurityContext {
	sc := &corev1.PodSecurityContext{RunAsUser: int64Ptr(uid)}
	if uid != 0 {
		sc.RunAsGroup = int64Ptr(uid)
	}
	return sc
}

func (r *ShareJobRunner) start(ctx context.Context, jobName string, shares []ShareMount, shape jobShape) error {
	labels := map[string]string{shareJobLabel: jobName}
	var volumes []corev1.Volume
	var mounts []corev1.VolumeMount

	for i, share := range shares {
		name := fmt.Sprintf("%s-s%d", jobName, i)
		if err := r.ensureSharePV(ctx, name, share, labels); err != nil {
			return err
		}
		if err := r.ensureShareClaim(ctx, name, labels); err != nil {
			return err
		}
		volName := fmt.Sprintf("share%d", i)
		volumes = append(volumes, corev1.Volume{
			Name: volName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: name},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: volName, MountPath: "/" + volName})
	}

	resources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
	}
	var initContainers []corev1.Container
	if shape.initScript != "" {
		initContainers = []corev1.Container{{
			Name:            "prepare",
			Image:           shape.image,
			Command:         []string{"/bin/sh", "-c", shape.initScript},
			SecurityContext: &corev1.SecurityContext{RunAsUser: int64Ptr(0)},
			Resources:       resources,
			VolumeMounts:    mounts,
		}}
	}
	backoffLimit := shareJobBackoffLimit
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: r.namespace, Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:   corev1.RestartPolicyOnFailure,
					SecurityContext: podSecurityContext(shape.runAsUser),
					InitContainers:  initContainers,
					Containers: []corev1.Container{{
						Name:         "share-data",
						Image:        shape.image,
						Command:      shape.command,
						Resources:    resources,
						VolumeMounts: mounts,
					}},
					Volumes: volumes,
				},
			},
		},
	}
	if err := r.client.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create share Job %s: %w", jobName, err)
	}
	klog.V(2).InfoS("Started share data Job", "job", jobName, "namespace", r.namespace)
	return nil
}

func (r *ShareJobRunner) ensureSharePV(ctx context.Context, name string, share ShareMount, labels map[string]string) error {
	exportPath := share.ExportPath
	if exportPath == "" {
		exportPath = "/"
	}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:    corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			// Retain: deleting this PV must never touch the share.
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			MountOptions:                  []string{"nfsvers=4.1", "soft", "timeo=600", "retrans=3", "sec=sys"},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				NFS: &corev1.NFSVolumeSource{Server: share.MountTargetIP, Path: exportPath},
			},
			ClaimRef: &corev1.ObjectReference{Name: name, Namespace: r.namespace},
		},
	}
	if err := r.client.Create(ctx, pv); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create share PV %s: %w", name, err)
	}
	return nil
}

func (r *ShareJobRunner) ensureShareClaim(ctx context.Context, name string, labels map[string]string) error {
	emptyClass := ""
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.namespace, Labels: labels},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			StorageClassName: &emptyClass,
			VolumeName:       name,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}
	if err := r.client.Create(ctx, pvc); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create share claim %s: %w", name, err)
	}
	return nil
}

// cleanup removes the Job, its claims and its PVs. Errors are logged; a
// later call retries the removal of anything left.
func (r *ShareJobRunner) cleanup(ctx context.Context, jobName string, shareCount int) {
	propagation := metav1.DeletePropagationBackground
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: r.namespace}}
	if err := r.client.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &propagation}); err != nil && !apierrors.IsNotFound(err) {
		klog.ErrorS(err, "Failed to delete share data Job", "job", jobName)
	}
	for i := 0; i < shareCount; i++ {
		name := fmt.Sprintf("%s-s%d", jobName, i)
		pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.namespace}}
		if err := r.client.Delete(ctx, pvc); err != nil && !apierrors.IsNotFound(err) {
			klog.ErrorS(err, "Failed to delete share data claim", "pvc", name)
		}
		pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if err := r.client.Delete(ctx, pv); err != nil && !apierrors.IsNotFound(err) {
			klog.ErrorS(err, "Failed to delete share data PV", "pv", name)
		}
	}
}

// shareJobName turns an operation key into a Job name that fits Kubernetes
// name limits with room for PV and claim suffixes.
func shareJobName(key string) string {
	name := "share-data-" + key
	if len(name) <= shareJobNameMax {
		return name
	}
	sum := sha256.Sum256([]byte(key))
	return name[:shareJobNameMax-9] + "-" + hex.EncodeToString(sum[:])[:8]
}

// validateSharePath accepts only volume and snapshot folders.
func validateSharePath(path string) error {
	if util.ValidateSubDir(path) == nil || util.ValidateSnapshotPath(path) == nil {
		return nil
	}
	return fmt.Errorf("path %q is not a volume or snapshot folder", path)
}

// Stop deletes the Job for key. It reports done once the Job and all its
// pods are gone; only then are the Job's claims and PVs removed.
func (r *ShareJobRunner) Stop(ctx context.Context, key string) (bool, error) {
	jobName := shareJobName(key)
	err := r.client.Get(ctx, types.NamespacedName{Namespace: r.namespace, Name: jobName}, &batchv1.Job{})
	if err != nil && !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("get share Job %s: %w", jobName, err)
	}
	if err == nil {
		propagation := metav1.DeletePropagationForeground
		job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: r.namespace}}
		if err := r.client.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &propagation}); err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("stop share Job %s: %w", jobName, err)
		}
		klog.V(2).InfoS("Stopping share data Job", "job", jobName, "namespace", r.namespace)
		return false, nil
	}

	pods := &corev1.PodList{}
	if err := r.client.List(ctx, pods, client.InNamespace(r.namespace), client.MatchingLabels{shareJobLabel: jobName}); err != nil {
		return false, fmt.Errorf("list pods of share Job %s: %w", jobName, err)
	}
	if len(pods.Items) > 0 {
		return false, nil
	}
	r.cleanup(ctx, jobName, 2)
	return true, nil
}
