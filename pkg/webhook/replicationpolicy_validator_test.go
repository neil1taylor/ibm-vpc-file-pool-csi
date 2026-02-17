package webhook

import (
	"context"
	"testing"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func validReplicationPolicy() *v1alpha1.ReplicationPolicy {
	return &v1alpha1.ReplicationPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "repl-1"},
		Spec: v1alpha1.ReplicationPolicySpec{
			SourcePoolName:       "test-pool",
			DestinationNFSServer: "10.0.0.1",
			DestinationBasePath:  "/pvcs",
			Schedule:             "15m",
			MaxRetries:           3,
		},
	}
}

func TestReplicationPolicyValidator_Create_Valid(t *testing.T) {
	v := &ReplicationPolicyValidator{}
	_, err := v.ValidateCreate(context.Background(), validReplicationPolicy())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReplicationPolicyValidator_Create_MissingSourcePool(t *testing.T) {
	v := &ReplicationPolicyValidator{}
	rp := validReplicationPolicy()
	rp.Spec.SourcePoolName = ""
	_, err := v.ValidateCreate(context.Background(), rp)
	if err == nil {
		t.Fatal("expected error for missing sourcePoolName")
	}
}

func TestReplicationPolicyValidator_Create_MissingDestServer(t *testing.T) {
	v := &ReplicationPolicyValidator{}
	rp := validReplicationPolicy()
	rp.Spec.DestinationNFSServer = ""
	_, err := v.ValidateCreate(context.Background(), rp)
	if err == nil {
		t.Fatal("expected error for missing destinationNFSServer")
	}
}

func TestReplicationPolicyValidator_Create_MissingBasePath(t *testing.T) {
	v := &ReplicationPolicyValidator{}
	rp := validReplicationPolicy()
	rp.Spec.DestinationBasePath = ""
	_, err := v.ValidateCreate(context.Background(), rp)
	if err == nil {
		t.Fatal("expected error for missing destinationBasePath")
	}
}

func TestReplicationPolicyValidator_Create_MissingSchedule(t *testing.T) {
	v := &ReplicationPolicyValidator{}
	rp := validReplicationPolicy()
	rp.Spec.Schedule = ""
	_, err := v.ValidateCreate(context.Background(), rp)
	if err == nil {
		t.Fatal("expected error for missing schedule")
	}
}

func TestReplicationPolicyValidator_Create_InvalidSchedule(t *testing.T) {
	v := &ReplicationPolicyValidator{}
	rp := validReplicationPolicy()
	rp.Spec.Schedule = "not-a-duration"
	_, err := v.ValidateCreate(context.Background(), rp)
	if err == nil {
		t.Fatal("expected error for invalid schedule duration")
	}
}

func TestReplicationPolicyValidator_Create_NegativeMaxRetries(t *testing.T) {
	v := &ReplicationPolicyValidator{}
	rp := validReplicationPolicy()
	rp.Spec.MaxRetries = -1
	_, err := v.ValidateCreate(context.Background(), rp)
	if err == nil {
		t.Fatal("expected error for negative maxRetries")
	}
}

func TestReplicationPolicyValidator_Create_ValidSchedules(t *testing.T) {
	v := &ReplicationPolicyValidator{}
	schedules := []string{"15m", "1h", "6h", "30s", "24h"}
	for _, sched := range schedules {
		rp := validReplicationPolicy()
		rp.Spec.Schedule = sched
		_, err := v.ValidateCreate(context.Background(), rp)
		if err != nil {
			t.Fatalf("unexpected error for schedule %q: %v", sched, err)
		}
	}
}

func TestReplicationPolicyValidator_Delete(t *testing.T) {
	v := &ReplicationPolicyValidator{}
	_, err := v.ValidateDelete(context.Background(), validReplicationPolicy())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
