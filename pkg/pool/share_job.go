package pool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
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

// shareJobFailure is ErrShareJobFailed, with what the Job's container wrote to
// its termination log before it failed. The message is the Job's own output and
// may be empty.
type shareJobFailure struct {
	job string
	msg string
}

func (e *shareJobFailure) Error() string {
	return fmt.Sprintf("%s: %s", ErrShareJobFailed, e.job)
}

func (e *shareJobFailure) Is(target error) bool { return target == ErrShareJobFailed }

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
	// FetchFromCOS restores one export of a volume from the Kopia repository in
	// COS into the scratch folder req.Scratch on dst, and reports its moment.
	FetchFromCOS(ctx context.Context, key string, dst ShareMount, repo ExportRepo, req COSRestoreRequest) (bool, COSRestoreOutcome, error)
	// RemoveCOSScratch removes the scratch folder of a restore from COS.
	RemoveCOSScratch(ctx context.Context, key string, share ShareMount, scratch string) (bool, error)
	// ShareUsage reads how full the share is, as a whole percent.
	ShareUsage(ctx context.Context, key string, share ShareMount) (bool, int, error)
	// ExportToCOS sends the backup folder of one volume and its moments folder
	// to the Kopia repository in COS. It reports the moment it exported and how
	// many bytes it sent. The controller passes only the names of Secrets.
	ExportToCOS(ctx context.Context, key string, share ShareMount, repo ExportRepo, backupPath, momentsPath string, moment time.Time) (bool, ExportOutcome, error)
	// MaintainCOSRepo runs Kopia's maintenance on the repository in COS, and
	// deletes the snapshots older than repo.ExpireBefore.
	MaintainCOSRepo(ctx context.Context, key string, repo ExportRepo) (bool, error)
	// Stop ends any work running for key. It reports done=true once
	// nothing runs for key any more.
	Stop(ctx context.Context, key string) (bool, error)
}

// ShareJobRunner implements ShareData with Kubernetes Jobs in the driver's
// namespace, because the controller pod has no NFS access.
type ShareJobRunner struct {
	client        client.Client
	namespace     string
	image         string
	driverImage   string
	launcherImage string
	browseImage   string
	// browsePullSecrets are the pull Secrets of browse helper pods.
	browsePullSecrets []string
	// browseCOSCPU and browseCOSMemory are the limits of the helper pod that
	// reads a backup in COS. Empty means the defaults.
	browseCOSCPU, browseCOSMemory string
}

// SetLauncherImage sets KubeVirt's virt-launcher image, which has qemu-img,
// for the Jobs that change a VM disk's snapshot chain. Without it, those Jobs
// are refused.
func (r *ShareJobRunner) SetLauncherImage(image string) {
	r.launcherImage = image
}

// RunChainScript runs a pkg/vmsnap script as the QEMU user in the
// virt-launcher image, after initScript (if set) as root. Folder i is mounted
// at /vol<i>; nothing else of the shares is mounted. It implements ChainJobs.
func (r *ShareJobRunner) RunChainScript(ctx context.Context, key string, folders []ShareFolder, script, initScript string) (bool, error) {
	if r.launcherImage == "" {
		return false, errors.New("no virt-launcher image set for VM disk chain Jobs")
	}
	var shares []ShareMount
	// The root step runs in the runner's own image: the virt-launcher image
	// only ever runs as the QEMU user.
	shape := jobShape{
		image:      r.launcherImage,
		command:    []string{"/bin/sh", "-c", script},
		runAsUser:  qemuUID,
		initScript: initScript,
		initImage:  r.image,
	}
	for i, f := range folders {
		if err := validateSharePath(f.Path); err != nil {
			return false, err
		}
		idx := -1
		for j, sh := range shares {
			if sh == f.Share {
				idx = j
			}
		}
		if idx < 0 {
			shares = append(shares, f.Share)
			idx = len(shares) - 1
		}
		shape.folders = append(shape.folders, folderMount{share: idx, subPath: strings.TrimPrefix(f.Path, "/"), at: fmt.Sprintf("/vol%d", i)})
	}
	return r.run(ctx, key, shares, shape)
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
	// initImage runs initScript; empty means image.
	initImage string
	// folders, if set, replace the share mounts: each mounts one folder of
	// a share.
	folders []folderMount
	// readOnly mounts every share read-only, down to the NFS export.
	readOnly bool
	// env is added to the main container. Secrets go here as references, so
	// they are never in the Job's command.
	env []corev1.EnvVar
	// volumes and mounts are added to the Job and its main container: Secrets
	// as files, and memory-backed folders.
	volumes []corev1.Volume
	mounts  []corev1.VolumeMount
	// resources replaces the default limits of the main container.
	resources *corev1.ResourceRequirements
	// labels are added to the Job, its pod and the share objects.
	labels map[string]string
	// owner, if set, owns the Job, the share PVs and the claims, so that
	// deleting the owner removes them.
	owner *metav1.OwnerReference
	// hardened gives the main container no privilege escalation, no
	// capabilities and a read-only root file system.
	hardened bool
	// addCaps are the capabilities a hardened container gets back.
	addCaps []corev1.Capability
	// session makes a pod that runs once, with a deadline, and never restarts.
	session bool
	// noToken keeps the Kubernetes API token out of the pod. A pod that holds the
	// backup key or the COS key must never have it. A pod with no folder mounts
	// gets the token unless this is set.
	noToken bool
	// activeDeadline is the Job's time limit in seconds. A session always has
	// one; any other Job has one only if this is set.
	activeDeadline int64
	// readiness is the main container's readiness probe.
	readiness *corev1.Probe
	// pullSecrets are the image pull Secrets of the pod.
	pullSecrets []string
}

// folderMount mounts subPath of share number share at at.
type folderMount struct {
	share   int
	subPath string
	at      string
	// readOnly mounts this folder read-only even if the Job's other mounts are not.
	readOnly bool
}

// ShareFolder is one folder on a share.
type ShareFolder struct {
	Share ShareMount
	Path  string // e.g. /pvcs/pvc-abc
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
		msg := r.terminationMessage(ctx, jobName)
		r.cleanup(ctx, jobName, len(shares))
		return false, "", &shareJobFailure{job: jobName, msg: msg}
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
	for k, v := range shape.labels {
		labels[k] = v
	}
	var owners []metav1.OwnerReference
	if shape.owner != nil {
		owners = []metav1.OwnerReference{*shape.owner}
	}
	var volumes []corev1.Volume
	var mounts []corev1.VolumeMount

	for i, share := range shares {
		name := fmt.Sprintf("%s-s%d", jobName, i)
		if err := r.ensureSharePV(ctx, name, share, labels, owners, shape.readOnly); err != nil {
			return err
		}
		if err := r.ensureShareClaim(ctx, name, labels, owners); err != nil {
			return err
		}
		volName := fmt.Sprintf("share%d", i)
		volumes = append(volumes, corev1.Volume{
			Name: volName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: name, ReadOnly: shape.readOnly},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: volName, MountPath: "/" + volName, ReadOnly: shape.readOnly})
	}
	if shape.folders != nil {
		mounts = nil
		for _, f := range shape.folders {
			mounts = append(mounts, corev1.VolumeMount{Name: fmt.Sprintf("share%d", f.share), MountPath: f.at, SubPath: f.subPath, ReadOnly: shape.readOnly || f.readOnly})
		}
	}

	mainMounts := append(slices.Clone(mounts), shape.mounts...)
	volumes = append(volumes, shape.volumes...)

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
	mainResources := resources
	if shape.resources != nil {
		mainResources = *shape.resources
	}
	var initContainers []corev1.Container
	if shape.initScript != "" {
		initContainers = []corev1.Container{{
			Name:            "prepare",
			Image:           orDefault(shape.initImage, shape.image),
			Command:         []string{"/bin/sh", "-c", shape.initScript},
			SecurityContext: &corev1.SecurityContext{RunAsUser: int64Ptr(0)},
			Resources:       resources,
			VolumeMounts:    mounts,
		}}
	}
	backoffLimit := shareJobBackoffLimit
	restart := corev1.RestartPolicyOnFailure
	var deadline *int64
	if shape.session {
		backoffLimit, restart, deadline = 0, corev1.RestartPolicyNever, &shape.activeDeadline
	} else if shape.activeDeadline > 0 {
		deadline = &shape.activeDeadline
	}
	var containerSecurity *corev1.SecurityContext
	if shape.hardened {
		containerSecurity = &corev1.SecurityContext{
			AllowPrivilegeEscalation: boolPtr(false),
			ReadOnlyRootFilesystem:   boolPtr(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}, Add: shape.addCaps},
		}
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: r.namespace, Labels: labels, OwnerReferences: owners},
		Spec: batchv1.JobSpec{
			BackoffLimit:          &backoffLimit,
			ActiveDeadlineSeconds: deadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					// A Job that opens VM disk files needs no API access.
					AutomountServiceAccountToken: boolPtr(shape.folders == nil && !shape.noToken),
					RestartPolicy:                restart,
					SecurityContext:              podSecurityContext(shape.runAsUser),
					InitContainers:               initContainers,
					ImagePullSecrets:             pullSecretRefs(shape.pullSecrets),
					Containers: []corev1.Container{{
						Name:            "share-data",
						Image:           shape.image,
						Command:         shape.command,
						Env:             shape.env,
						Resources:       mainResources,
						VolumeMounts:    mainMounts,
						SecurityContext: containerSecurity,
						ReadinessProbe:  shape.readiness,
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

func (r *ShareJobRunner) ensureSharePV(ctx context.Context, name string, share ShareMount, labels map[string]string, owners []metav1.OwnerReference, readOnly bool) error {
	exportPath := share.ExportPath
	if exportPath == "" {
		exportPath = "/"
	}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels, OwnerReferences: owners},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:    corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			// Retain: deleting this PV must never touch the share.
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			MountOptions:                  []string{"nfsvers=4.1", "soft", "timeo=600", "retrans=3", "sec=sys"},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				NFS: &corev1.NFSVolumeSource{Server: share.MountTargetIP, Path: exportPath, ReadOnly: readOnly},
			},
			ClaimRef: &corev1.ObjectReference{Name: name, Namespace: r.namespace},
		},
	}
	if err := r.client.Create(ctx, pv); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create share PV %s: %w", name, err)
	}
	return nil
}

func (r *ShareJobRunner) ensureShareClaim(ctx context.Context, name string, labels map[string]string, owners []metav1.OwnerReference) error {
	emptyClass := ""
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.namespace, Labels: labels, OwnerReferences: owners},
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

func boolPtr(v bool) *bool { return &v }

func pullSecretRefs(names []string) []corev1.LocalObjectReference {
	var refs []corev1.LocalObjectReference
	for _, n := range names {
		refs = append(refs, corev1.LocalObjectReference{Name: n})
	}
	return refs
}
