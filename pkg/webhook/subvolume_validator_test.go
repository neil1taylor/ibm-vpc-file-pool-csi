package webhook

import (
	"context"
	"testing"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func validSubVolume() *v1alpha1.SubVolume {
	return &v1alpha1.SubVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-abc12345-1234-5678-9abc-def012345678"},
		Spec: v1alpha1.SubVolumeSpec{
			PoolName:     "test-pool",
			ShareID:      "r006-xxxx",
			PVName:       "pvc-abc12345-1234-5678-9abc-def012345678",
			PVCName:      "my-pvc",
			PVCNamespace: "default",
			RequestedGB:  10,
			SubPath:      "/pvcs/pvc-abc12345-1234-5678-9abc-def012345678",
		},
	}
}

func TestSubVolumeValidator_Create_Valid(t *testing.T) {
	v := &SubVolumeValidator{}
	_, err := v.ValidateCreate(context.Background(), validSubVolume())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSubVolumeValidator_Create_MissingPoolName(t *testing.T) {
	v := &SubVolumeValidator{}
	sv := validSubVolume()
	sv.Spec.PoolName = ""
	_, err := v.ValidateCreate(context.Background(), sv)
	if err == nil {
		t.Fatal("expected error for missing poolName")
	}
}

func TestSubVolumeValidator_Create_MissingPVCName(t *testing.T) {
	v := &SubVolumeValidator{}
	sv := validSubVolume()
	sv.Spec.PVCName = ""
	_, err := v.ValidateCreate(context.Background(), sv)
	if err == nil {
		t.Fatal("expected error for missing pvcName")
	}
}

func TestSubVolumeValidator_Create_MissingPVCNamespace(t *testing.T) {
	v := &SubVolumeValidator{}
	sv := validSubVolume()
	sv.Spec.PVCNamespace = ""
	_, err := v.ValidateCreate(context.Background(), sv)
	if err == nil {
		t.Fatal("expected error for missing pvcNamespace")
	}
}

func TestSubVolumeValidator_Create_ZeroRequestedSize(t *testing.T) {
	v := &SubVolumeValidator{}
	sv := validSubVolume()
	sv.Spec.RequestedGB = 0
	_, err := v.ValidateCreate(context.Background(), sv)
	if err == nil {
		t.Fatal("expected error for zero requestedGB")
	}
}

func TestSubVolumeValidator_Create_InvalidSubPath(t *testing.T) {
	v := &SubVolumeValidator{}

	tests := []struct {
		name    string
		subPath string
	}{
		{"empty", ""},
		{"no prefix", "pvc-abc12345-1234-5678-9abc-def012345678"},
		{"traversal", "/pvcs/../etc/passwd"},
		{"wrong pattern", "/pvcs/my-volume"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sv := validSubVolume()
			sv.Spec.SubPath = tt.subPath
			_, err := v.ValidateCreate(context.Background(), sv)
			if err == nil {
				t.Fatalf("expected error for subPath %q", tt.subPath)
			}
		})
	}
}

func TestSubVolumeValidator_Delete(t *testing.T) {
	v := &SubVolumeValidator{}
	_, err := v.ValidateDelete(context.Background(), validSubVolume())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
