package migrate

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

// MigrationResult is returned when a migration completes.
type MigrationResult struct {
	// SourcePVC is the name of the source PVC.
	SourcePVC string
	// TargetPVC is the name of the newly created target PVC.
	TargetPVC string
	// PodName is the migration pod that performed the copy.
	PodName string
	// Duration is how long the migration took.
	Duration time.Duration
	// Success indicates whether the migration completed without errors.
	Success bool
	// Message is a human-readable summary.
	Message string
}

// MigrationStatus represents the current state of a migration.
type MigrationStatus struct {
	// PodName is the migration pod name.
	PodName string
	// Namespace is the namespace.
	Namespace string
	// SourcePVC is the source PVC from annotations.
	SourcePVC string
	// TargetPVC is the target PVC from annotations.
	TargetPVC string
	// Phase is the pod phase.
	Phase corev1.PodPhase
	// StartTime is when the pod started.
	StartTime *metav1.Time
}

// Executor performs the migration of a single PVC.
type Executor struct {
	clientset kubernetes.Interface
	output    io.Writer
}

// NewExecutor creates an Executor with the given Kubernetes client.
// The output writer receives progress messages (typically os.Stdout).
func NewExecutor(clientset kubernetes.Interface, output io.Writer) *Executor {
	return &Executor{
		clientset: clientset,
		output:    output,
	}
}

// Execute migrates data from sourcePVC to a new PVC on the pool driver.
// It creates the target PVC, launches a migration pod, streams its logs,
// and waits for completion. The old PVC is NOT deleted.
//
// If dryRun is true, the function prints what it would do without making changes.
func (e *Executor) Execute(ctx context.Context, namespace, sourcePVC, targetPool, targetStorageClass string, dryRun bool) (*MigrationResult, error) {
	start := time.Now()

	// 1. Fetch the source PVC to determine size.
	srcPVC, err := e.clientset.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, sourcePVC, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get source PVC %s/%s: %w", namespace, sourcePVC, err)
	}

	if srcPVC.Status.Phase != corev1.ClaimBound {
		return nil, fmt.Errorf("source PVC %s is not Bound (phase: %s); only Bound PVCs can be migrated", sourcePVC, srcPVC.Status.Phase)
	}

	storageSize := srcPVC.Spec.Resources.Requests[corev1.ResourceStorage]
	targetPVCName := fmt.Sprintf("%s-pooled", sourcePVC)
	migrationPodName := fmt.Sprintf("migrate-%s", sourcePVC)

	// Truncate names to Kubernetes limits (253 chars for pod, 253 for PVC).
	if len(targetPVCName) > 253 {
		targetPVCName = targetPVCName[:253]
	}
	if len(migrationPodName) > 253 {
		migrationPodName = migrationPodName[:253]
	}

	if dryRun {
		_, _ = fmt.Fprintf(e.output, "[dry-run] Would create target PVC %s/%s (size: %s, storageClass: %s)\n",
			namespace, targetPVCName, storageSize.String(), targetStorageClass)
		_, _ = fmt.Fprintf(e.output, "[dry-run] Would create migration pod %s/%s\n", namespace, migrationPodName)
		_, _ = fmt.Fprintf(e.output, "[dry-run] Would rsync data from %s to %s\n", sourcePVC, targetPVCName)
		_, _ = fmt.Fprintf(e.output, "[dry-run] Source PVC would NOT be deleted\n")
		return &MigrationResult{
			SourcePVC: sourcePVC,
			TargetPVC: targetPVCName,
			PodName:   migrationPodName,
			Duration:  time.Since(start),
			Success:   true,
			Message:   "dry-run completed",
		}, nil
	}

	// 2. Create the target PVC on the pool driver.
	_, _ = fmt.Fprintf(e.output, "Creating target PVC %s/%s (size: %s, storageClass: %s)...\n",
		namespace, targetPVCName, storageSize.String(), targetStorageClass)

	targetPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      targetPVCName,
			Namespace: namespace,
			Labels: map[string]string{
				LabelMigration: "true",
			},
			Annotations: map[string]string{
				AnnotationSourcePVC:             sourcePVC,
				"vpc-file-pool-csi.ibm.io/pool": targetPool,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			StorageClassName: &targetStorageClass,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: storageSize,
				},
			},
		},
	}

	_, err = e.clientset.CoreV1().PersistentVolumeClaims(namespace).Create(ctx, targetPVC, metav1.CreateOptions{})
	if err != nil {
		if errors.IsAlreadyExists(err) {
			_, _ = fmt.Fprintf(e.output, "Target PVC %s already exists, reusing it.\n", targetPVCName)
		} else {
			return nil, fmt.Errorf("failed to create target PVC: %w", err)
		}
	}

	// Wait for target PVC to bind.
	_, _ = fmt.Fprintf(e.output, "Waiting for target PVC to bind...\n")
	if err := e.waitForPVCBound(ctx, namespace, targetPVCName, 2*time.Minute); err != nil {
		return nil, fmt.Errorf("target PVC did not bind: %w", err)
	}
	_, _ = fmt.Fprintf(e.output, "Target PVC is Bound.\n")

	// 3. Create the migration pod.
	_, _ = fmt.Fprintf(e.output, "Creating migration pod %s/%s...\n", namespace, migrationPodName)

	migrationPod := BuildMigrationPod(MigrationPodSpec{
		Name:      migrationPodName,
		Namespace: namespace,
		SourcePVC: sourcePVC,
		TargetPVC: targetPVCName,
	})

	// Clean up any previous migration pod with the same name.
	_ = e.clientset.CoreV1().Pods(namespace).Delete(ctx, migrationPodName, metav1.DeleteOptions{})
	// Brief pause to let the delete propagate.
	time.Sleep(2 * time.Second)

	_, err = e.clientset.CoreV1().Pods(namespace).Create(ctx, migrationPod, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to create migration pod: %w", err)
	}

	// 4. Wait for the pod to start running, then stream logs.
	_, _ = fmt.Fprintf(e.output, "Waiting for migration pod to start...\n")
	if err := e.waitForPodRunning(ctx, namespace, migrationPodName, 5*time.Minute); err != nil {
		// Attempt cleanup.
		e.cleanupPod(ctx, namespace, migrationPodName)
		return nil, fmt.Errorf("migration pod did not start: %w", err)
	}

	_, _ = fmt.Fprintf(e.output, "Migration pod is running. Streaming logs:\n")
	_, _ = fmt.Fprintf(e.output, "---\n")

	if err := e.streamPodLogs(ctx, namespace, migrationPodName); err != nil {
		_, _ = fmt.Fprintf(e.output, "---\n")
		_, _ = fmt.Fprintf(e.output, "Warning: failed to stream logs: %v\n", err)
	} else {
		_, _ = fmt.Fprintf(e.output, "---\n")
	}

	// 5. Wait for pod completion.
	podPhase, err := e.waitForPodComplete(ctx, namespace, migrationPodName, 30*time.Minute)
	if err != nil {
		e.cleanupPod(ctx, namespace, migrationPodName)
		return nil, fmt.Errorf("migration did not complete: %w", err)
	}

	duration := time.Since(start)
	success := podPhase == corev1.PodSucceeded

	result := &MigrationResult{
		SourcePVC: sourcePVC,
		TargetPVC: targetPVCName,
		PodName:   migrationPodName,
		Duration:  duration,
		Success:   success,
	}

	if success {
		result.Message = fmt.Sprintf("Migration completed in %s. New PVC: %s/%s. "+
			"Update your workloads to use the new PVC, then delete the old PVC when ready.",
			duration.Round(time.Second), namespace, targetPVCName)
	} else {
		result.Message = fmt.Sprintf("Migration pod failed (phase: %s). Check pod logs: kubectl logs -n %s %s",
			podPhase, namespace, migrationPodName)
	}

	return result, nil
}

// Status returns the current state of all migration pods in the given namespace.
func (e *Executor) Status(ctx context.Context, namespace string) ([]MigrationStatus, error) {
	podList, err := e.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: LabelMigration + "=true",
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list migration pods: %w", err)
	}

	var statuses []MigrationStatus
	for i := range podList.Items {
		pod := &podList.Items[i]
		statuses = append(statuses, MigrationStatus{
			PodName:   pod.Name,
			Namespace: pod.Namespace,
			SourcePVC: pod.Annotations[AnnotationSourcePVC],
			TargetPVC: pod.Annotations[AnnotationTargetPVC],
			Phase:     pod.Status.Phase,
			StartTime: pod.Status.StartTime,
		})
	}
	return statuses, nil
}

// waitForPVCBound waits until the PVC reaches the Bound phase or the timeout expires.
func (e *Executor) waitForPVCBound(ctx context.Context, namespace, name string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	watcher, err := e.clientset.CoreV1().PersistentVolumeClaims(namespace).Watch(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("metadata.name=%s", name),
	})
	if err != nil {
		return fmt.Errorf("failed to watch PVC: %w", err)
	}
	defer watcher.Stop()

	for event := range watcher.ResultChan() {
		if event.Type == watch.Error {
			return fmt.Errorf("watch error")
		}
		pvc, ok := event.Object.(*corev1.PersistentVolumeClaim)
		if !ok {
			continue
		}
		if pvc.Status.Phase == corev1.ClaimBound {
			return nil
		}
	}
	return fmt.Errorf("timed out waiting for PVC %s/%s to bind", namespace, name)
}

// waitForPodRunning waits until the pod is Running or has completed.
func (e *Executor) waitForPodRunning(ctx context.Context, namespace, name string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	watcher, err := e.clientset.CoreV1().Pods(namespace).Watch(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("metadata.name=%s", name),
	})
	if err != nil {
		return fmt.Errorf("failed to watch pod: %w", err)
	}
	defer watcher.Stop()

	for event := range watcher.ResultChan() {
		if event.Type == watch.Error {
			return fmt.Errorf("watch error")
		}
		pod, ok := event.Object.(*corev1.Pod)
		if !ok {
			continue
		}
		switch pod.Status.Phase {
		case corev1.PodRunning, corev1.PodSucceeded:
			return nil
		case corev1.PodFailed:
			return fmt.Errorf("pod failed")
		}
	}
	return fmt.Errorf("timed out waiting for pod %s/%s to start", namespace, name)
}

// waitForPodComplete waits until the pod reaches Succeeded or Failed.
func (e *Executor) waitForPodComplete(ctx context.Context, namespace, name string, timeout time.Duration) (corev1.PodPhase, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Check current state first — the pod may have already completed.
	pod, err := e.clientset.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			return pod.Status.Phase, nil
		}
	}

	watcher, err := e.clientset.CoreV1().Pods(namespace).Watch(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("metadata.name=%s", name),
	})
	if err != nil {
		return "", fmt.Errorf("failed to watch pod: %w", err)
	}
	defer watcher.Stop()

	for event := range watcher.ResultChan() {
		if event.Type == watch.Error {
			return "", fmt.Errorf("watch error")
		}
		pod, ok := event.Object.(*corev1.Pod)
		if !ok {
			continue
		}
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			return pod.Status.Phase, nil
		}
	}
	return "", fmt.Errorf("timed out waiting for pod %s/%s to complete", namespace, name)
}

// streamPodLogs follows the migration pod logs to e.output.
func (e *Executor) streamPodLogs(ctx context.Context, namespace, name string) error {
	req := e.clientset.CoreV1().Pods(namespace).GetLogs(name, &corev1.PodLogOptions{
		Follow: true,
	})
	stream, err := req.Stream(ctx)
	if err != nil {
		return fmt.Errorf("failed to open log stream: %w", err)
	}
	defer func() { _ = stream.Close() }()

	scanner := bufio.NewScanner(stream)
	for scanner.Scan() {
		_, _ = fmt.Fprintln(e.output, scanner.Text())
	}
	return scanner.Err()
}

// cleanupPod deletes the migration pod (best-effort).
func (e *Executor) cleanupPod(ctx context.Context, namespace, name string) {
	_ = e.clientset.CoreV1().Pods(namespace).Delete(ctx, name, metav1.DeleteOptions{})
}

// targetPVCSize returns the storage size as a resource.Quantity, ensuring
// a minimum of 1Gi.
func targetPVCSize(sizeGB int64) resource.Quantity {
	if sizeGB < 1 {
		sizeGB = 1
	}
	return resource.MustParse(fmt.Sprintf("%dGi", sizeGB))
}
