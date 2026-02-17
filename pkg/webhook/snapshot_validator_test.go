package webhook

import (
	"context"
	"testing"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func validSnapshot() *v1alpha1.Snapshot {
	return &v1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-123"},
		Spec: v1alpha1.SnapshotSpec{
			SourceSubVolume: "pvc-abc123",
			PoolName:        "test-pool",
			ShareID:         "r006-xxxx",
			SnapshotPath:    "/pvcs/.snapshots/snap-123",
			SourceSubPath:   "/pvcs/pvc-abc123",
			SizeGB:          10,
		},
	}
}

func TestSnapshotValidator_Create_Valid(t *testing.T) {
	v := &SnapshotValidator{}
	_, err := v.ValidateCreate(context.Background(), validSnapshot())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSnapshotValidator_Create_MissingSource(t *testing.T) {
	v := &SnapshotValidator{}
	snap := validSnapshot()
	snap.Spec.SourceSubVolume = ""
	_, err := v.ValidateCreate(context.Background(), snap)
	if err == nil {
		t.Fatal("expected error for missing sourceSubVolume")
	}
}

func TestSnapshotValidator_Create_MissingPoolName(t *testing.T) {
	v := &SnapshotValidator{}
	snap := validSnapshot()
	snap.Spec.PoolName = ""
	_, err := v.ValidateCreate(context.Background(), snap)
	if err == nil {
		t.Fatal("expected error for missing poolName")
	}
}

func TestSnapshotValidator_Delete(t *testing.T) {
	v := &SnapshotValidator{}
	_, err := v.ValidateDelete(context.Background(), validSnapshot())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
