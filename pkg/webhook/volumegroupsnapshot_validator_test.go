package webhook

import (
	"context"
	"testing"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func validVolumeGroupSnapshot() *v1alpha1.VolumeGroupSnapshot {
	return &v1alpha1.VolumeGroupSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "group-snap-1"},
		Spec: v1alpha1.VolumeGroupSnapshotSpec{
			PoolName:      "test-pool",
			SourcePVCs:    []string{"pvc-1", "pvc-2"},
			FailurePolicy: "Abort",
		},
	}
}

func TestVolumeGroupSnapshotValidator_Create_Valid(t *testing.T) {
	v := &VolumeGroupSnapshotValidator{}
	_, err := v.ValidateCreate(context.Background(), validVolumeGroupSnapshot())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestVolumeGroupSnapshotValidator_Create_MissingPoolName(t *testing.T) {
	v := &VolumeGroupSnapshotValidator{}
	vgs := validVolumeGroupSnapshot()
	vgs.Spec.PoolName = ""
	_, err := v.ValidateCreate(context.Background(), vgs)
	if err == nil {
		t.Fatal("expected error for missing poolName")
	}
}

func TestVolumeGroupSnapshotValidator_Create_EmptySourcePVCs(t *testing.T) {
	v := &VolumeGroupSnapshotValidator{}
	vgs := validVolumeGroupSnapshot()
	vgs.Spec.SourcePVCs = nil
	_, err := v.ValidateCreate(context.Background(), vgs)
	if err == nil {
		t.Fatal("expected error for empty sourcePVCs")
	}
}

func TestVolumeGroupSnapshotValidator_Create_InvalidFailurePolicy(t *testing.T) {
	v := &VolumeGroupSnapshotValidator{}
	vgs := validVolumeGroupSnapshot()
	vgs.Spec.FailurePolicy = "Invalid"
	_, err := v.ValidateCreate(context.Background(), vgs)
	if err == nil {
		t.Fatal("expected error for invalid failurePolicy")
	}
}

func TestVolumeGroupSnapshotValidator_Create_ContinuePolicy(t *testing.T) {
	v := &VolumeGroupSnapshotValidator{}
	vgs := validVolumeGroupSnapshot()
	vgs.Spec.FailurePolicy = "Continue"
	_, err := v.ValidateCreate(context.Background(), vgs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestVolumeGroupSnapshotValidator_Delete(t *testing.T) {
	v := &VolumeGroupSnapshotValidator{}
	_, err := v.ValidateDelete(context.Background(), validVolumeGroupSnapshot())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
