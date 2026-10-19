package webhook

import (
	"context"
	"testing"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func validFileSharePool() *v1alpha1.FileSharePool {
	return &v1alpha1.FileSharePool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pool"},
		Spec: v1alpha1.FileSharePoolSpec{
			Zone:                   "us-south-1",
			Profile:                "dp2",
			ShareSizeGB:            1000,
			MaxShares:              10,
			InitialShares:          1,
			ExpandThresholdPercent: 80,
			AllocationStrategy:     "spread",
		},
	}
}

func TestFileSharePoolValidator_Create_Valid(t *testing.T) {
	v := &FileSharePoolValidator{}
	_, err := v.ValidateCreate(context.Background(), validFileSharePool())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestFileSharePoolValidator_Create_MissingZone(t *testing.T) {
	v := &FileSharePoolValidator{}
	pool := validFileSharePool()
	pool.Spec.Zone = ""
	_, err := v.ValidateCreate(context.Background(), pool)
	if err == nil {
		t.Fatal("expected error for missing zone")
	}
}

func TestFileSharePoolValidator_Create_MissingProfile(t *testing.T) {
	v := &FileSharePoolValidator{}
	pool := validFileSharePool()
	pool.Spec.Profile = ""
	_, err := v.ValidateCreate(context.Background(), pool)
	if err == nil {
		t.Fatal("expected error for missing profile")
	}
}

func TestFileSharePoolValidator_Create_ShareSizeTooSmall(t *testing.T) {
	v := &FileSharePoolValidator{}
	pool := validFileSharePool()
	pool.Spec.ShareSizeGB = 5
	_, err := v.ValidateCreate(context.Background(), pool)
	if err == nil {
		t.Fatal("expected error for small shareSizeGB")
	}
}

func TestFileSharePoolValidator_Create_ShareSizeTooLarge(t *testing.T) {
	v := &FileSharePoolValidator{}
	pool := validFileSharePool()
	pool.Spec.ShareSizeGB = 33000
	_, err := v.ValidateCreate(context.Background(), pool)
	if err == nil {
		t.Fatal("expected error for large shareSizeGB")
	}
}

func TestFileSharePoolValidator_Create_MaxSharesRange(t *testing.T) {
	v := &FileSharePoolValidator{}

	pool := validFileSharePool()
	pool.Spec.MaxShares = 0
	_, err := v.ValidateCreate(context.Background(), pool)
	if err == nil {
		t.Fatal("expected error for maxShares=0")
	}

	pool = validFileSharePool()
	pool.Spec.MaxShares = 1001
	_, err = v.ValidateCreate(context.Background(), pool)
	if err == nil {
		t.Fatal("expected error for maxShares=1001")
	}
}

func TestFileSharePoolValidator_Create_InitialExceedsMax(t *testing.T) {
	v := &FileSharePoolValidator{}
	pool := validFileSharePool()
	pool.Spec.InitialShares = 20
	pool.Spec.MaxShares = 10
	_, err := v.ValidateCreate(context.Background(), pool)
	if err == nil {
		t.Fatal("expected error for initialShares > maxShares")
	}
}

func TestFileSharePoolValidator_Create_ExpandThreshold(t *testing.T) {
	v := &FileSharePoolValidator{}

	pool := validFileSharePool()
	pool.Spec.ExpandThresholdPercent = 0
	_, err := v.ValidateCreate(context.Background(), pool)
	if err == nil {
		t.Fatal("expected error for expandThresholdPercent=0")
	}

	pool = validFileSharePool()
	pool.Spec.ExpandThresholdPercent = 100
	_, err = v.ValidateCreate(context.Background(), pool)
	if err == nil {
		t.Fatal("expected error for expandThresholdPercent=100")
	}
}

func TestFileSharePoolValidator_Create_InvalidStrategy(t *testing.T) {
	v := &FileSharePoolValidator{}
	pool := validFileSharePool()
	pool.Spec.AllocationStrategy = "random"
	_, err := v.ValidateCreate(context.Background(), pool)
	if err == nil {
		t.Fatal("expected error for invalid allocationStrategy")
	}
}

func TestFileSharePoolValidator_Update_ImmutableZone(t *testing.T) {
	v := &FileSharePoolValidator{}
	oldPool := validFileSharePool()
	newPool := validFileSharePool()
	newPool.Spec.Zone = "us-south-2"
	_, err := v.ValidateUpdate(context.Background(), oldPool, newPool)
	if err == nil {
		t.Fatal("expected error for immutable zone change")
	}
}

func TestFileSharePoolValidator_Update_ImmutableProfile(t *testing.T) {
	v := &FileSharePoolValidator{}
	oldPool := validFileSharePool()
	newPool := validFileSharePool()
	newPool.Spec.Profile = "custom"
	_, err := v.ValidateUpdate(context.Background(), oldPool, newPool)
	if err == nil {
		t.Fatal("expected error for immutable profile change")
	}
}

func TestFileSharePoolValidator_Update_Valid(t *testing.T) {
	v := &FileSharePoolValidator{}
	oldPool := validFileSharePool()
	newPool := validFileSharePool()
	newPool.Spec.MaxShares = 20
	_, err := v.ValidateUpdate(context.Background(), oldPool, newPool)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestFileSharePoolValidator_Delete(t *testing.T) {
	v := &FileSharePoolValidator{}
	_, err := v.ValidateDelete(context.Background(), validFileSharePool())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// The node agent silently falls back to its default when the permissions
// string is not octal, so the webhook must refuse it up front.
func TestFileSharePoolValidator_Create_DefaultPermissions(t *testing.T) {
	tests := []struct {
		perms   string
		wantErr bool
	}{
		{"", false},
		{"0755", false},
		{"0777", false},
		{"755", false},
		{"2775", false},
		{"not-octal", true},
		{"0789", true},
		{"17777", true},
		{"-755", true},
	}
	for _, tt := range tests {
		t.Run(tt.perms, func(t *testing.T) {
			pool := validFileSharePool()
			pool.Spec.DefaultPermissions = tt.perms
			_, err := (&FileSharePoolValidator{}).ValidateCreate(context.Background(), pool)
			if (err != nil) != tt.wantErr {
				t.Errorf("defaultPermissions %q: err = %v, wantErr %v", tt.perms, err, tt.wantErr)
			}
		})
	}
}
