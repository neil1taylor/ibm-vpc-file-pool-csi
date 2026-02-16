package migrate

import (
	"bytes"
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func TestExecutor_Execute_DryRun(t *testing.T) {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-data",
			Namespace: "default",
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: strPtr("ibm-vpc-file-5iops"),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("10Gi"),
				},
			},
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase: corev1.ClaimBound,
		},
	}

	clientset := fake.NewSimpleClientset(pvc)
	var buf bytes.Buffer
	executor := NewExecutor(clientset, &buf)

	result, err := executor.Execute(context.Background(), "default", "my-data", "general-purpose", "ibm-vpc-file-pool", true)
	if err != nil {
		t.Fatalf("Execute() dry-run error: %v", err)
	}

	if !result.Success {
		t.Error("expected dry-run to succeed")
	}
	if result.SourcePVC != "my-data" {
		t.Errorf("expected source PVC my-data, got %s", result.SourcePVC)
	}
	if result.TargetPVC != "my-data-pooled" {
		t.Errorf("expected target PVC my-data-pooled, got %s", result.TargetPVC)
	}
	if result.Message != "dry-run completed" {
		t.Errorf("expected message 'dry-run completed', got %s", result.Message)
	}

	output := buf.String()
	if !contains(output, "[dry-run]") {
		t.Error("expected dry-run output to contain '[dry-run]'")
	}
	if !contains(output, "10Gi") {
		t.Error("expected dry-run output to mention the size 10Gi")
	}
}

func TestExecutor_Execute_PVCNotFound(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	var buf bytes.Buffer
	executor := NewExecutor(clientset, &buf)

	_, err := executor.Execute(context.Background(), "default", "nonexistent", "pool", "sc", false)
	if err == nil {
		t.Fatal("expected error for nonexistent PVC")
	}
	if !contains(err.Error(), "failed to get source PVC") {
		t.Errorf("expected 'failed to get source PVC' in error, got: %s", err.Error())
	}
}

func TestExecutor_Execute_PVCNotBound(t *testing.T) {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pending-pvc",
			Namespace: "default",
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("5Gi"),
				},
			},
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase: corev1.ClaimPending,
		},
	}

	clientset := fake.NewSimpleClientset(pvc)
	var buf bytes.Buffer
	executor := NewExecutor(clientset, &buf)

	_, err := executor.Execute(context.Background(), "default", "pending-pvc", "pool", "sc", false)
	if err == nil {
		t.Fatal("expected error for unbound PVC")
	}
	if !contains(err.Error(), "not Bound") {
		t.Errorf("expected 'not Bound' in error, got: %s", err.Error())
	}
}

func TestExecutor_Status_NoMigrations(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	var buf bytes.Buffer
	executor := NewExecutor(clientset, &buf)

	statuses, err := executor.Status(context.Background(), "default")
	if err != nil {
		t.Fatalf("Status() error: %v", err)
	}
	if len(statuses) != 0 {
		t.Errorf("expected 0 statuses, got %d", len(statuses))
	}
}

func TestExecutor_Status_FindsMigrationPods(t *testing.T) {
	pods := []runtime.Object{
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "migrate-pvc-1",
				Namespace: "default",
				Labels: map[string]string{
					LabelMigration: "true",
				},
				Annotations: map[string]string{
					AnnotationSourcePVC: "pvc-1",
					AnnotationTargetPVC: "pvc-1-pooled",
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodSucceeded,
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "migrate-pvc-2",
				Namespace: "default",
				Labels: map[string]string{
					LabelMigration: "true",
				},
				Annotations: map[string]string{
					AnnotationSourcePVC: "pvc-2",
					AnnotationTargetPVC: "pvc-2-pooled",
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
			},
		},
		// This pod does NOT have the migration label — should be excluded.
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "regular-pod",
				Namespace: "default",
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
			},
		},
	}

	clientset := fake.NewSimpleClientset(pods...)
	var buf bytes.Buffer
	executor := NewExecutor(clientset, &buf)

	statuses, err := executor.Status(context.Background(), "default")
	if err != nil {
		t.Fatalf("Status() error: %v", err)
	}

	if len(statuses) != 2 {
		t.Fatalf("expected 2 migration statuses, got %d", len(statuses))
	}

	statusMap := make(map[string]MigrationStatus)
	for _, s := range statuses {
		statusMap[s.PodName] = s
	}

	s1, ok := statusMap["migrate-pvc-1"]
	if !ok {
		t.Fatal("expected to find migrate-pvc-1 in statuses")
	}
	if s1.SourcePVC != "pvc-1" {
		t.Errorf("expected source PVC pvc-1, got %s", s1.SourcePVC)
	}
	if s1.Phase != corev1.PodSucceeded {
		t.Errorf("expected phase Succeeded, got %s", s1.Phase)
	}

	s2, ok := statusMap["migrate-pvc-2"]
	if !ok {
		t.Fatal("expected to find migrate-pvc-2 in statuses")
	}
	if s2.TargetPVC != "pvc-2-pooled" {
		t.Errorf("expected target PVC pvc-2-pooled, got %s", s2.TargetPVC)
	}
	if s2.Phase != corev1.PodRunning {
		t.Errorf("expected phase Running, got %s", s2.Phase)
	}
}

func TestTargetPVCSize(t *testing.T) {
	tests := []struct {
		name     string
		sizeGB   int64
		expected string
	}{
		{name: "normal size", sizeGB: 10, expected: "10Gi"},
		{name: "zero rounds up", sizeGB: 0, expected: "1Gi"},
		{name: "negative rounds up", sizeGB: -5, expected: "1Gi"},
		{name: "large size", sizeGB: 2000, expected: "2000Gi"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q := targetPVCSize(tc.sizeGB)
			if q.String() != tc.expected {
				t.Errorf("targetPVCSize(%d) = %s, want %s", tc.sizeGB, q.String(), tc.expected)
			}
		})
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstring(s, substr))
}

func containsSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func strPtr(s string) *string {
	return &s
}
