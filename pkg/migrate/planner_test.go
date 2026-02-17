package migrate

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func TestPlanner_Plan_FiltersByStorageClass(t *testing.T) {
	targetSC := "ibm-vpc-file-5iops"
	otherSC := "ibm-vpc-file-pool"

	objects := []runtime.Object{
		makePVC("pvc-match-1", "default", targetSC, "10Gi", corev1.ClaimBound),
		makePVC("pvc-match-2", "default", targetSC, "20Gi", corev1.ClaimBound),
		makePVC("pvc-other", "default", otherSC, "5Gi", corev1.ClaimBound),
		makePVC("pvc-no-sc", "default", "", "1Gi", corev1.ClaimPending),
	}

	clientset := fake.NewSimpleClientset(objects...) //nolint:staticcheck // NewClientset requires generated apply configs
	planner := NewPlanner(clientset)

	plan, err := planner.Plan(context.Background(), "default", targetSC, "general-purpose")
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	if len(plan.PVCs) != 2 {
		t.Fatalf("expected 2 PVCs, got %d", len(plan.PVCs))
	}
	if plan.PVCs[0].Name != "pvc-match-1" {
		t.Errorf("expected first PVC name pvc-match-1, got %s", plan.PVCs[0].Name)
	}
	if plan.PVCs[1].Name != "pvc-match-2" {
		t.Errorf("expected second PVC name pvc-match-2, got %s", plan.PVCs[1].Name)
	}
	if plan.TotalSizeGB != 30 {
		t.Errorf("expected total size 30 GiB, got %d", plan.TotalSizeGB)
	}
	if plan.TargetPool != "general-purpose" {
		t.Errorf("expected target pool general-purpose, got %s", plan.TargetPool)
	}
	if plan.SourceStorageClass != targetSC {
		t.Errorf("expected source SC %s, got %s", targetSC, plan.SourceStorageClass)
	}
}

func TestPlanner_Plan_NoPVCsFound(t *testing.T) {
	clientset := fake.NewSimpleClientset() //nolint:staticcheck // NewClientset requires generated apply configs
	planner := NewPlanner(clientset)

	plan, err := planner.Plan(context.Background(), "default", "nonexistent-sc", "pool")
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}
	if len(plan.PVCs) != 0 {
		t.Fatalf("expected 0 PVCs, got %d", len(plan.PVCs))
	}
	if plan.TotalSizeGB != 0 {
		t.Errorf("expected total size 0, got %d", plan.TotalSizeGB)
	}
}

func TestPlanner_Plan_SizeRounding(t *testing.T) {
	// 500Mi should round up to 1 GiB.
	objects := []runtime.Object{
		makePVC("pvc-small", "default", "sc", "500Mi", corev1.ClaimBound),
	}

	clientset := fake.NewSimpleClientset(objects...) //nolint:staticcheck // NewClientset requires generated apply configs
	planner := NewPlanner(clientset)

	plan, err := planner.Plan(context.Background(), "default", "sc", "pool")
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	if len(plan.PVCs) != 1 {
		t.Fatalf("expected 1 PVC, got %d", len(plan.PVCs))
	}
	if plan.PVCs[0].SizeGB != 1 {
		t.Errorf("expected 500Mi to round up to 1 GiB, got %d", plan.PVCs[0].SizeGB)
	}
}

func TestPlanner_Plan_IncludesPhase(t *testing.T) {
	objects := []runtime.Object{
		makePVC("pvc-bound", "default", "sc", "10Gi", corev1.ClaimBound),
		makePVC("pvc-pending", "default", "sc", "5Gi", corev1.ClaimPending),
	}

	clientset := fake.NewSimpleClientset(objects...) //nolint:staticcheck // NewClientset requires generated apply configs
	planner := NewPlanner(clientset)

	plan, err := planner.Plan(context.Background(), "default", "sc", "pool")
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	if len(plan.PVCs) != 2 {
		t.Fatalf("expected 2 PVCs, got %d", len(plan.PVCs))
	}

	phaseMap := make(map[string]corev1.PersistentVolumeClaimPhase)
	for _, p := range plan.PVCs {
		phaseMap[p.Name] = p.Phase
	}
	if phaseMap["pvc-bound"] != corev1.ClaimBound {
		t.Errorf("expected pvc-bound to be Bound, got %s", phaseMap["pvc-bound"])
	}
	if phaseMap["pvc-pending"] != corev1.ClaimPending {
		t.Errorf("expected pvc-pending to be Pending, got %s", phaseMap["pvc-pending"])
	}
}

func TestPlanner_Plan_ShareIDFromPV(t *testing.T) {
	pvc := makePVC("pvc-with-pv", "default", "sc", "10Gi", corev1.ClaimBound)
	pvc.Spec.VolumeName = "pv-1"

	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-1"},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:       "vpc.file.csi.ibm.io",
					VolumeHandle: "vol-1",
					VolumeAttributes: map[string]string{
						"shareID": "r006-abc-123",
					},
				},
			},
		},
	}

	clientset := fake.NewSimpleClientset(pvc, pv) //nolint:staticcheck // NewClientset requires generated apply configs
	planner := NewPlanner(clientset)

	plan, err := planner.Plan(context.Background(), "default", "sc", "pool")
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	if len(plan.PVCs) != 1 {
		t.Fatalf("expected 1 PVC, got %d", len(plan.PVCs))
	}
	if plan.PVCs[0].ShareID != "r006-abc-123" {
		t.Errorf("expected share ID r006-abc-123, got %s", plan.PVCs[0].ShareID)
	}
}

func TestPlanner_Plan_FindsPodsUsingPVC(t *testing.T) {
	pvc := makePVC("data-pvc", "default", "sc", "10Gi", corev1.ClaimBound)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-app-pod",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "busybox"}},
			Volumes: []corev1.Volume{
				{
					Name: "data",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: "data-pvc",
						},
					},
				},
			},
		},
	}

	clientset := fake.NewSimpleClientset(pvc, pod) //nolint:staticcheck // NewClientset requires generated apply configs
	planner := NewPlanner(clientset)

	plan, err := planner.Plan(context.Background(), "default", "sc", "pool")
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	if len(plan.PVCs) != 1 {
		t.Fatalf("expected 1 PVC, got %d", len(plan.PVCs))
	}
	if len(plan.PVCs[0].Pods) != 1 || plan.PVCs[0].Pods[0] != "my-app-pod" {
		t.Errorf("expected pods [my-app-pod], got %v", plan.PVCs[0].Pods)
	}
}

func TestPlanner_Plan_SortsByName(t *testing.T) {
	objects := []runtime.Object{
		makePVC("zzz-pvc", "default", "sc", "1Gi", corev1.ClaimBound),
		makePVC("aaa-pvc", "default", "sc", "1Gi", corev1.ClaimBound),
		makePVC("mmm-pvc", "default", "sc", "1Gi", corev1.ClaimBound),
	}

	clientset := fake.NewSimpleClientset(objects...) //nolint:staticcheck // NewClientset requires generated apply configs
	planner := NewPlanner(clientset)

	plan, err := planner.Plan(context.Background(), "default", "sc", "pool")
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	if len(plan.PVCs) != 3 {
		t.Fatalf("expected 3 PVCs, got %d", len(plan.PVCs))
	}
	if plan.PVCs[0].Name != "aaa-pvc" || plan.PVCs[1].Name != "mmm-pvc" || plan.PVCs[2].Name != "zzz-pvc" {
		t.Errorf("PVCs not sorted by name: %v", []string{plan.PVCs[0].Name, plan.PVCs[1].Name, plan.PVCs[2].Name})
	}
}

// makePVC creates a PVC for testing.
func makePVC(name, namespace, storageClassName, size string, phase corev1.PersistentVolumeClaimPhase) *corev1.PersistentVolumeClaim {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(size),
				},
			},
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase: phase,
		},
	}
	if storageClassName != "" {
		pvc.Spec.StorageClassName = &storageClassName
	}
	return pvc
}
