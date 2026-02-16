package migrate

import (
	"strings"
	"testing"
)

func TestBuildMigrationPod_BasicStructure(t *testing.T) {
	spec := MigrationPodSpec{
		Name:      "migrate-my-pvc",
		Namespace: "default",
		SourcePVC: "my-pvc",
		TargetPVC: "my-pvc-pooled",
	}

	pod := BuildMigrationPod(spec)

	if pod.Name != "migrate-my-pvc" {
		t.Errorf("expected pod name migrate-my-pvc, got %s", pod.Name)
	}
	if pod.Namespace != "default" {
		t.Errorf("expected namespace default, got %s", pod.Namespace)
	}
}

func TestBuildMigrationPod_Labels(t *testing.T) {
	spec := MigrationPodSpec{
		Name:      "migrate-pvc",
		Namespace: "test-ns",
		SourcePVC: "src",
		TargetPVC: "tgt",
	}

	pod := BuildMigrationPod(spec)

	if pod.Labels[LabelMigration] != "true" {
		t.Errorf("expected migration label to be 'true', got %q", pod.Labels[LabelMigration])
	}
}

func TestBuildMigrationPod_Annotations(t *testing.T) {
	spec := MigrationPodSpec{
		Name:      "migrate-pvc",
		Namespace: "test-ns",
		SourcePVC: "old-data",
		TargetPVC: "new-data",
	}

	pod := BuildMigrationPod(spec)

	if pod.Annotations[AnnotationSourcePVC] != "old-data" {
		t.Errorf("expected source PVC annotation 'old-data', got %q", pod.Annotations[AnnotationSourcePVC])
	}
	if pod.Annotations[AnnotationTargetPVC] != "new-data" {
		t.Errorf("expected target PVC annotation 'new-data', got %q", pod.Annotations[AnnotationTargetPVC])
	}
}

func TestBuildMigrationPod_VolumeMounts(t *testing.T) {
	spec := MigrationPodSpec{
		Name:      "migrate-pvc",
		Namespace: "default",
		SourcePVC: "src-pvc",
		TargetPVC: "tgt-pvc",
	}

	pod := BuildMigrationPod(spec)

	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(pod.Spec.Containers))
	}

	container := pod.Spec.Containers[0]
	if len(container.VolumeMounts) != 2 {
		t.Fatalf("expected 2 volume mounts, got %d", len(container.VolumeMounts))
	}

	sourceMount := container.VolumeMounts[0]
	if sourceMount.MountPath != "/source" {
		t.Errorf("expected source mount path /source, got %s", sourceMount.MountPath)
	}
	if !sourceMount.ReadOnly {
		t.Error("expected source mount to be read-only")
	}

	targetMount := container.VolumeMounts[1]
	if targetMount.MountPath != "/target" {
		t.Errorf("expected target mount path /target, got %s", targetMount.MountPath)
	}
	if targetMount.ReadOnly {
		t.Error("expected target mount to be writable")
	}
}

func TestBuildMigrationPod_Volumes(t *testing.T) {
	spec := MigrationPodSpec{
		Name:      "migrate-pvc",
		Namespace: "default",
		SourcePVC: "src-pvc",
		TargetPVC: "tgt-pvc",
	}

	pod := BuildMigrationPod(spec)

	if len(pod.Spec.Volumes) != 2 {
		t.Fatalf("expected 2 volumes, got %d", len(pod.Spec.Volumes))
	}

	sourceVol := pod.Spec.Volumes[0]
	if sourceVol.PersistentVolumeClaim == nil {
		t.Fatal("expected source volume to be a PVC")
	}
	if sourceVol.PersistentVolumeClaim.ClaimName != "src-pvc" {
		t.Errorf("expected source PVC claim name src-pvc, got %s", sourceVol.PersistentVolumeClaim.ClaimName)
	}
	if !sourceVol.PersistentVolumeClaim.ReadOnly {
		t.Error("expected source PVC to be read-only")
	}

	targetVol := pod.Spec.Volumes[1]
	if targetVol.PersistentVolumeClaim == nil {
		t.Fatal("expected target volume to be a PVC")
	}
	if targetVol.PersistentVolumeClaim.ClaimName != "tgt-pvc" {
		t.Errorf("expected target PVC claim name tgt-pvc, got %s", targetVol.PersistentVolumeClaim.ClaimName)
	}
}

func TestBuildMigrationPod_RsyncCommand(t *testing.T) {
	spec := MigrationPodSpec{
		Name:      "migrate-pvc",
		Namespace: "default",
		SourcePVC: "src-pvc",
		TargetPVC: "tgt-pvc",
	}

	pod := BuildMigrationPod(spec)
	container := pod.Spec.Containers[0]

	// The command should contain rsync.
	cmd := strings.Join(container.Command, " ")
	if !strings.Contains(cmd, "rsync") {
		t.Error("expected migration container command to include rsync")
	}
	if !strings.Contains(cmd, "/source/") {
		t.Error("expected rsync command to reference /source/")
	}
	if !strings.Contains(cmd, "/target/") {
		t.Error("expected rsync command to reference /target/")
	}
}

func TestBuildMigrationPod_RestartPolicy(t *testing.T) {
	spec := MigrationPodSpec{
		Name:      "migrate-pvc",
		Namespace: "default",
		SourcePVC: "src-pvc",
		TargetPVC: "tgt-pvc",
	}

	pod := BuildMigrationPod(spec)

	if pod.Spec.RestartPolicy != "Never" {
		t.Errorf("expected RestartPolicy Never, got %s", pod.Spec.RestartPolicy)
	}
}

func TestBuildMigrationPod_Image(t *testing.T) {
	spec := MigrationPodSpec{
		Name:      "migrate-pvc",
		Namespace: "default",
		SourcePVC: "src-pvc",
		TargetPVC: "tgt-pvc",
	}

	pod := BuildMigrationPod(spec)

	if pod.Spec.Containers[0].Image != MigrationImage {
		t.Errorf("expected image %s, got %s", MigrationImage, pod.Spec.Containers[0].Image)
	}
}

func TestBuildMigrationPod_Resources(t *testing.T) {
	spec := MigrationPodSpec{
		Name:      "migrate-pvc",
		Namespace: "default",
		SourcePVC: "src-pvc",
		TargetPVC: "tgt-pvc",
	}

	pod := BuildMigrationPod(spec)
	container := pod.Spec.Containers[0]

	cpuReq := container.Resources.Requests["cpu"]
	if cpuReq.String() != "100m" {
		t.Errorf("expected CPU request 100m, got %s", cpuReq.String())
	}

	memReq := container.Resources.Requests["memory"]
	if memReq.String() != "128Mi" {
		t.Errorf("expected memory request 128Mi, got %s", memReq.String())
	}

	cpuLimit := container.Resources.Limits["cpu"]
	if cpuLimit.String() != "500m" {
		t.Errorf("expected CPU limit 500m, got %s", cpuLimit.String())
	}

	memLimit := container.Resources.Limits["memory"]
	if memLimit.String() != "256Mi" {
		t.Errorf("expected memory limit 256Mi, got %s", memLimit.String())
	}
}
