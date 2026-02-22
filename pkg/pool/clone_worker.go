package pool

import (
	"context"
	"fmt"
	"sync"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/k8s"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/metrics"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// DefaultCloneWorkerInterval is how often the clone worker checks for pending clones.
	DefaultCloneWorkerInterval = 10 * time.Second

	// DefaultCloneImage is the container image used for clone Jobs.
	DefaultCloneImage = "quay.io/centos/centos:stream9"

	// cloneTempLabel marks temporary resources created by the clone worker.
	cloneTempLabel = "storage.ibmcloud.io/clone-temp"

	// cloneSubVolumeLabel identifies which SubVolume a clone resource belongs to.
	cloneSubVolumeLabel = "storage.ibmcloud.io/clone-subvolume"
)

// CloneWorker processes SubVolumes with cloneStatus=Pending or InProgress.
// It creates Kubernetes Jobs to perform the data copy, since the controller
// pod has no NFS filesystem access.
type CloneWorker struct {
	k8sClient    k8s.Client
	directClient client.Client
	cloneImage   string
	interval     time.Duration

	// processing tracks SubVolume names currently being processed to prevent
	// concurrent workers from processing the same clone.
	processing sync.Map
}

// NewCloneWorker creates a new clone worker with the given dependencies.
func NewCloneWorker(k8sClient k8s.Client, directClient client.Client, cloneImage string) *CloneWorker {
	if cloneImage == "" {
		cloneImage = DefaultCloneImage
	}
	return &CloneWorker{
		k8sClient:    k8sClient,
		directClient: directClient,
		cloneImage:   cloneImage,
		interval:     DefaultCloneWorkerInterval,
	}
}

// SetInterval overrides the default poll interval. Useful for tests.
func (w *CloneWorker) SetInterval(d time.Duration) {
	w.interval = d
}

// Run starts the clone worker loop. It blocks until the context is cancelled.
func (w *CloneWorker) Run(ctx context.Context) {
	klog.V(2).InfoS("Clone worker started", "interval", w.interval)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			klog.V(2).InfoS("Clone worker stopped")
			return
		case <-ticker.C:
			w.processOnce(ctx)
		}
	}
}

// processOnce lists all clone SubVolumes and processes any that are Pending or InProgress.
func (w *CloneWorker) processOnce(ctx context.Context) {
	clones, err := w.k8sClient.ListCloneSubVolumes(ctx)
	if err != nil {
		klog.ErrorS(err, "Clone worker failed to list clone SubVolumes")
		return
	}

	for i := range clones {
		sv := &clones[i]
		switch sv.Status.CloneStatus {
		case "Pending", "InProgress":
			// Check if already being processed by another goroutine
			if _, loaded := w.processing.LoadOrStore(sv.Name, true); loaded {
				klog.V(6).InfoS("Clone already being processed, skipping",
					"subVolume", sv.Name)
				continue
			}

			go func(name string) {
				defer w.processing.Delete(name)
				w.processClone(ctx, name)
			}(sv.Name)
		}
	}
}

// processClone orchestrates the Job-based clone for a single SubVolume.
func (w *CloneWorker) processClone(ctx context.Context, subVolumeName string) {
	start := time.Now()

	// Re-fetch the SubVolume to get the latest version.
	sv, err := w.k8sClient.GetSubVolume(ctx, subVolumeName)
	if err != nil {
		klog.ErrorS(err, "Clone worker failed to get SubVolume", "subVolume", subVolumeName)
		return
	}

	// Double-check status.
	if sv.Status.CloneStatus != "Pending" && sv.Status.CloneStatus != "InProgress" {
		klog.V(4).InfoS("Clone status changed since list, skipping",
			"subVolume", subVolumeName, "cloneStatus", sv.Status.CloneStatus)
		return
	}

	poolName := sv.Spec.PoolName

	klog.V(2).InfoS("Processing async clone",
		"subVolume", sv.Name,
		"source", sv.Spec.SourceVolume,
		"cloneStatus", sv.Status.CloneStatus,
	)

	// Check if a clone Job already exists.
	done, succeeded, checkErr := w.checkCloneJob(ctx, sv)
	if checkErr != nil {
		klog.ErrorS(checkErr, "Failed to check clone Job status", "subVolume", sv.Name)
		return
	}

	if done {
		if succeeded {
			// Job succeeded — mark SubVolume as Complete and cleanup.
			completedAt := metav1.Now()
			sv.Status.CloneStatus = "Complete"
			sv.Status.Phase = "Bound"
			if sv.Status.CloneProgress != nil {
				sv.Status.CloneProgress.BytesCopied = sv.Status.CloneProgress.TotalBytes
				sv.Status.CloneProgress.CompletedAt = &completedAt
			}
			if err := w.k8sClient.UpdateSubVolumeStatus(ctx, sv); err != nil {
				klog.ErrorS(err, "Failed to update clone status to Complete", "subVolume", sv.Name)
				return
			}
			w.cleanupCloneResources(ctx, sv.Name, sv.Spec.PVCNamespace)

			duration := time.Since(start).Seconds()
			metrics.ClonesTotal.WithLabelValues(poolName, "async_success").Inc()
			metrics.CloneDuration.WithLabelValues(poolName).Observe(duration)
			klog.V(2).InfoS("Async clone completed",
				"subVolume", sv.Name,
				"source", sv.Spec.SourceVolume,
				"duration", fmt.Sprintf("%.1fs", duration),
			)
		} else {
			// Job failed — mark SubVolume as Failed and cleanup.
			w.failClone(ctx, sv, poolName, "clone Job failed after retries")
			w.cleanupCloneResources(ctx, sv.Name, sv.Spec.PVCNamespace)
		}
		return
	}

	// Job exists but is still running — check again next tick.
	jobName := cloneJobName(sv.Name)
	existing := &batchv1.Job{}
	if err := w.directClient.Get(ctx, types.NamespacedName{
		Namespace: sv.Spec.PVCNamespace,
		Name:      jobName,
	}, existing); err == nil {
		klog.V(6).InfoS("Clone Job still running", "subVolume", sv.Name, "job", jobName)
		return
	}

	// No Job exists yet — create one.

	// 1. Transition to InProgress.
	if sv.Status.CloneStatus == "Pending" {
		now := metav1.Now()
		sv.Status.CloneStatus = "InProgress"
		sv.Status.Phase = "Cloning"
		if sv.Status.CloneProgress == nil {
			sv.Status.CloneProgress = &v1alpha1.CloneProgress{
				TotalBytes: sv.Spec.RequestedGB * (1 << 30),
			}
		}
		sv.Status.CloneProgress.StartedAt = &now
		if err := w.k8sClient.UpdateSubVolumeStatus(ctx, sv); err != nil {
			klog.ErrorS(err, "Failed to update clone status to InProgress", "subVolume", sv.Name)
			return
		}
	}

	// 2. Look up source SubVolume.
	sourceSV, err := w.k8sClient.GetSubVolume(ctx, sv.Spec.SourceVolume)
	if err != nil {
		w.failClone(ctx, sv, poolName, fmt.Sprintf("source SubVolume %q not found: %v", sv.Spec.SourceVolume, err))
		return
	}

	// 3. Create temp NFS PV for the target share root.
	if err := w.ensureClonePV(ctx, sv); err != nil {
		w.failClone(ctx, sv, poolName, fmt.Sprintf("failed to create temp PV: %v", err))
		return
	}

	// 4. Create temp PVC bound to the PV.
	if err := w.ensureClonePVC(ctx, sv); err != nil {
		w.failClone(ctx, sv, poolName, fmt.Sprintf("failed to create temp PVC: %v", err))
		w.cleanupCloneResources(ctx, sv.Name, sv.Spec.PVCNamespace)
		return
	}

	// 5. Create clone Job.
	if err := w.ensureCloneJob(ctx, sv, sourceSV); err != nil {
		w.failClone(ctx, sv, poolName, fmt.Sprintf("failed to create clone Job: %v", err))
		w.cleanupCloneResources(ctx, sv.Name, sv.Spec.PVCNamespace)
		return
	}

	klog.V(2).InfoS("Created clone Job",
		"subVolume", sv.Name,
		"source", sv.Spec.SourceVolume,
		"job", cloneJobName(sv.Name),
	)
}

// ensureClonePV creates a temporary NFS PV that mounts the target share root.
func (w *CloneWorker) ensureClonePV(ctx context.Context, sv *v1alpha1.SubVolume) error {
	pvName := clonePVName(sv.Name)

	existing := &corev1.PersistentVolume{}
	if err := w.directClient.Get(ctx, types.NamespacedName{Name: pvName}, existing); err == nil {
		return nil // already exists
	}

	exportPath := sv.Spec.ShareExportPath
	if exportPath == "" {
		exportPath = "/"
	}

	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: pvName,
			Labels: map[string]string{
				cloneTempLabel:      "true",
				cloneSubVolumeLabel: sv.Name,
			},
		},
		Spec: corev1.PersistentVolumeSpec{
			Capacity: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse("100Gi"),
			},
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteMany,
			},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
			MountOptions: []string{
				"nfsvers=4.1",
				"soft",
				"timeo=600",
				"retrans=3",
				"sec=sys",
			},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				NFS: &corev1.NFSVolumeSource{
					Server: sv.Spec.ShareMountTargetIP,
					Path:   exportPath,
				},
			},
			StorageClassName: "",
			ClaimRef: &corev1.ObjectReference{
				Name:      clonePVCName(sv.Name),
				Namespace: sv.Spec.PVCNamespace,
			},
		},
	}

	if err := w.directClient.Create(ctx, pv); err != nil {
		if errors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("creating temp PV %s: %w", pvName, err)
	}

	klog.V(4).InfoS("Created temp clone PV",
		"pv", pvName,
		"server", sv.Spec.ShareMountTargetIP,
		"exportPath", exportPath,
	)
	return nil
}

// ensureClonePVC creates a temporary PVC pre-bound to the temp NFS PV.
func (w *CloneWorker) ensureClonePVC(ctx context.Context, sv *v1alpha1.SubVolume) error {
	pvcName := clonePVCName(sv.Name)
	ns := sv.Spec.PVCNamespace

	existing := &corev1.PersistentVolumeClaim{}
	if err := w.directClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: pvcName}, existing); err == nil {
		return nil // already exists
	}

	emptyStorageClass := ""
	pvName := clonePVName(sv.Name)
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: ns,
			Labels: map[string]string{
				cloneTempLabel:      "true",
				cloneSubVolumeLabel: sv.Name,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteMany,
			},
			StorageClassName: &emptyStorageClass,
			VolumeName:       pvName,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("100Gi"),
				},
			},
		},
	}

	if err := w.directClient.Create(ctx, pvc); err != nil {
		if errors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("creating temp PVC %s/%s: %w", ns, pvcName, err)
	}

	klog.V(4).InfoS("Created temp clone PVC",
		"namespace", ns,
		"pvc", pvcName,
		"boundTo", pvName,
	)
	return nil
}

// ensureCloneJob creates the Job that copies data from source PVC to target share.
func (w *CloneWorker) ensureCloneJob(ctx context.Context, sv, sourceSV *v1alpha1.SubVolume) error {
	jobName := cloneJobName(sv.Name)
	ns := sv.Spec.PVCNamespace

	existing := &batchv1.Job{}
	if err := w.directClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: jobName}, existing); err == nil {
		return nil // already exists
	}

	// Build the copy script.
	uid := int64(0)
	gid := int64(0)
	perms := "0755"
	if sv.Spec.UID != nil {
		uid = *sv.Spec.UID
	}
	if sv.Spec.GID != nil {
		gid = *sv.Spec.GID
	}
	if sv.Spec.Permissions != "" {
		perms = sv.Spec.Permissions
	}

	script := fmt.Sprintf(`set -euo pipefail
echo "Clone: copying %s -> %s"
mkdir -p "/target-share%s"
cp -a /source/. "/target-share%s/"
chown -R %d:%d "/target-share%s"
chmod %s "/target-share%s"
echo "Clone complete."
`, sourceSV.Spec.SubPath, sv.Spec.SubPath,
		sv.Spec.SubPath, sv.Spec.SubPath,
		uid, gid, sv.Spec.SubPath,
		perms, sv.Spec.SubPath)

	backoffLimit := int32(3)
	ttl := int32(600)

	// The source PVC is the PVC that was bound to the source SubVolume.
	// Its PV name is the source SubVolume's PVName, and the PVC name is stored
	// in the source SubVolume's spec.
	sourcePVCName := sourceSV.Spec.PVCName

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: ns,
			Labels: map[string]string{
				cloneTempLabel:      "true",
				cloneSubVolumeLabel: sv.Name,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						cloneTempLabel:      "true",
						cloneSubVolumeLabel: sv.Name,
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsUser: int64Ptr(0),
					},
					Containers: []corev1.Container{
						{
							Name:    "clone",
							Image:   w.cloneImage,
							Command: []string{"/bin/sh", "-c", script},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("128Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("1"),
									corev1.ResourceMemory: resource.MustParse("512Mi"),
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "source", MountPath: "/source", ReadOnly: true},
								{Name: "target-share", MountPath: "/target-share"},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "source",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: sourcePVCName,
									ReadOnly:  true,
								},
							},
						},
						{
							Name: "target-share",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: clonePVCName(sv.Name),
								},
							},
						},
					},
				},
			},
		},
	}

	if err := w.directClient.Create(ctx, job); err != nil {
		if errors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("creating clone Job %s/%s: %w", ns, jobName, err)
	}

	klog.V(2).InfoS("Created clone Job",
		"namespace", ns,
		"job", jobName,
		"sourcePVC", sourcePVCName,
		"targetSubPath", sv.Spec.SubPath,
	)
	return nil
}

// checkCloneJob checks the status of a clone Job.
// Returns (done, succeeded, error). If done=false, the Job is still running or doesn't exist.
func (w *CloneWorker) checkCloneJob(ctx context.Context, sv *v1alpha1.SubVolume) (bool, bool, error) {
	jobName := cloneJobName(sv.Name)
	ns := sv.Spec.PVCNamespace

	job := &batchv1.Job{}
	err := w.directClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: jobName}, job)
	if err != nil {
		if errors.IsNotFound(err) {
			return false, false, nil // no Job yet
		}
		return false, false, fmt.Errorf("getting clone Job %s/%s: %w", ns, jobName, err)
	}

	if job.Status.Succeeded > 0 {
		return true, true, nil
	}

	// Check if all retries exhausted.
	backoffLimit := int32(3)
	if job.Spec.BackoffLimit != nil {
		backoffLimit = *job.Spec.BackoffLimit
	}
	if job.Status.Failed >= backoffLimit+1 {
		return true, false, nil
	}

	return false, false, nil // still running
}

// cleanupCloneResources deletes the temporary PV, PVC, and Job for a clone.
func (w *CloneWorker) cleanupCloneResources(ctx context.Context, svName, namespace string) {
	jobName := cloneJobName(svName)
	pvcName := clonePVCName(svName)
	pvName := clonePVName(svName)

	// Delete Job first (stops pods).
	propagation := metav1.DeletePropagationBackground
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: namespace},
	}
	if err := w.directClient.Delete(ctx, job, &client.DeleteOptions{
		PropagationPolicy: &propagation,
	}); err != nil && !errors.IsNotFound(err) {
		klog.ErrorS(err, "Failed to delete temp clone Job", "job", jobName)
	}

	// Delete PVC.
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: pvcName, Namespace: namespace},
	}
	if err := w.directClient.Delete(ctx, pvc); err != nil && !errors.IsNotFound(err) {
		klog.ErrorS(err, "Failed to delete temp clone PVC", "pvc", pvcName)
	}

	// Delete PV.
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: pvName},
	}
	if err := w.directClient.Delete(ctx, pv); err != nil && !errors.IsNotFound(err) {
		klog.ErrorS(err, "Failed to delete temp clone PV", "pv", pvName)
	}

	klog.V(4).InfoS("Cleaned up temp clone resources", "subVolume", svName)
}

// failClone sets the SubVolume clone status to Failed with an error message.
func (w *CloneWorker) failClone(ctx context.Context, sv *v1alpha1.SubVolume, poolName, errMsg string) {
	completedAt := metav1.Now()
	sv.Status.CloneStatus = "Failed"
	sv.Status.Phase = "Failed"
	if sv.Status.CloneProgress == nil {
		sv.Status.CloneProgress = &v1alpha1.CloneProgress{}
	}
	sv.Status.CloneProgress.Error = errMsg
	sv.Status.CloneProgress.CompletedAt = &completedAt

	if err := w.k8sClient.UpdateSubVolumeStatus(ctx, sv); err != nil {
		klog.ErrorS(err, "Failed to update clone status to Failed",
			"subVolume", sv.Name, "error", errMsg)
	}

	metrics.ClonesTotal.WithLabelValues(poolName, "async_error").Inc()

	klog.ErrorS(nil, "Async clone failed",
		"subVolume", sv.Name,
		"error", errMsg,
	)
}

// Naming helpers for temp clone resources.

func cloneJobName(svName string) string {
	// Job names are limited to 63 chars. PV names like "pvc-<uuid>" are 40 chars.
	// "clone-" prefix + 40 chars = 46 chars. Safe.
	return fmt.Sprintf("clone-%s", svName)
}

func clonePVCName(svName string) string {
	return fmt.Sprintf("clone-tgt-%s", svName)
}

func clonePVName(svName string) string {
	return fmt.Sprintf("clone-tgt-%s", svName)
}
