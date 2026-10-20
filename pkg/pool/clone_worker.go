package pool

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/k8s"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/metrics"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// DefaultCloneWorkerInterval is how often the clone worker checks for pending clones.
	DefaultCloneWorkerInterval = 10 * time.Second

	// DefaultCloneImage is the container image used for clone Jobs.
	DefaultCloneImage = "quay.io/centos/centos:stream9"
)

// CloneWorker processes SubVolumes with cloneStatus=Pending or InProgress.
// The controller pod has no NFS access, so a share data Job in the driver's
// namespace copies the source folder into the clone's folder.
type CloneWorker struct {
	k8sClient k8s.Client
	shareData ShareData
	interval  time.Duration

	// processing tracks SubVolume names currently being processed to prevent
	// concurrent workers from processing the same clone.
	processing sync.Map
}

// NewCloneWorker creates a clone worker whose copy Jobs run in the driver's
// namespace with cloneImage. directClient should read without a cache.
func NewCloneWorker(k8sClient k8s.Client, directClient client.Client, cloneImage string) *CloneWorker {
	return &CloneWorker{
		k8sClient: k8sClient,
		shareData: NewShareJobRunner(directClient, DefaultDriverNamespace, cloneImage),
		interval:  DefaultCloneWorkerInterval,
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

// processClone starts or checks the copy Job for a single clone SubVolume.
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

	klog.V(4).InfoS("Processing async clone",
		"subVolume", sv.Name,
		"source", sv.Spec.SourceVolume,
		"cloneStatus", sv.Status.CloneStatus,
	)

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
		if err = w.k8sClient.UpdateSubVolumeStatus(ctx, sv); err != nil {
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

	// 3. Start or check the copy Job.
	done, err := w.shareData.CopyDir(ctx, cloneCopyKey(sv.Name),
		subVolumeShareMount(sourceSV), sourceSV.Spec.SubPath,
		subVolumeShareMount(sv), sv.Spec.SubPath, cloneOwnership(sv))
	switch {
	case errors.Is(err, ErrShareJobFailed):
		w.failClone(ctx, sv, poolName, "clone Job failed after retries")
		return
	case err != nil:
		klog.ErrorS(err, "Failed to start or check clone Job, retrying next tick", "subVolume", sv.Name)
		return
	case !done:
		return
	}

	// 4. The Job succeeded: mark the SubVolume Complete.
	completedAt := metav1.Now()
	sv.Status.CloneStatus = "Complete"
	sv.Status.Phase = "Bound"
	if sv.Status.CloneProgress != nil {
		sv.Status.CloneProgress.BytesCopied = sv.Status.CloneProgress.TotalBytes
		sv.Status.CloneProgress.CompletedAt = &completedAt
	}
	if err = w.k8sClient.UpdateSubVolumeStatus(ctx, sv); err != nil {
		klog.ErrorS(err, "Failed to update clone status to Complete", "subVolume", sv.Name)
		return
	}

	duration := time.Since(start).Seconds()
	metrics.ClonesTotal.WithLabelValues(poolName, "async_success").Inc()
	metrics.CloneDuration.WithLabelValues(poolName).Observe(duration)
	klog.V(2).InfoS("Async clone completed",
		"subVolume", sv.Name,
		"source", sv.Spec.SourceVolume,
	)
}

// cloneCopyKey names the share data Job that copies data into a clone.
func cloneCopyKey(svName string) string {
	return "clone-" + svName
}

// cloneOwnership is applied to the whole copied tree: the clone's owner, or
// root, and its permissions, or 0755, on the top folder.
func cloneOwnership(sv *v1alpha1.SubVolume) *Ownership {
	uid, gid := int64(0), int64(0)
	if sv.Spec.UID != nil {
		uid = *sv.Spec.UID
	}
	if sv.Spec.GID != nil {
		gid = *sv.Spec.GID
	}
	perms := sv.Spec.Permissions
	if perms == "" {
		perms = "0755"
	}
	return &Ownership{UID: &uid, GID: &gid, Permissions: perms, Recursive: true}
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
