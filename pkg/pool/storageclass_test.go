package pool

import (
	"testing"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// ---------------------------------------------------------------------------
// 1. Non-tiered pool produces one StorageClass
// ---------------------------------------------------------------------------

func TestStorageClassesForPool_NonTiered(t *testing.T) {
	pool := &v1alpha1.FileSharePool{
		ObjectMeta: metav1.ObjectMeta{
			Name: "my-pool",
			UID:  types.UID("uid-123"),
		},
		TypeMeta: metav1.TypeMeta{
			APIVersion: "storage.ibmcloud.io/v1alpha1",
			Kind:       "FileSharePool",
		},
		Spec: v1alpha1.FileSharePoolSpec{
			Zone:               "us-south-1",
			Profile:            "dp2",
			ShareSizeGB:        1000,
			DefaultPermissions: "0755",
		},
	}

	classes := storageClassesForPool(pool)
	if len(classes) != 1 {
		t.Fatalf("expected 1 StorageClass, got %d", len(classes))
	}

	sc := classes[0]
	if sc.Name != "my-pool" {
		t.Errorf("expected name 'my-pool', got %q", sc.Name)
	}
	if sc.Provisioner != ProvisionerName {
		t.Errorf("expected provisioner %q, got %q", ProvisionerName, sc.Provisioner)
	}
	if sc.Parameters["pool"] != "my-pool" {
		t.Errorf("expected pool param 'my-pool', got %q", sc.Parameters["pool"])
	}
	if _, hasTier := sc.Parameters["tier"]; hasTier {
		t.Error("non-tiered pool should not have tier parameter")
	}
	if sc.Parameters["permissions"] != "0755" {
		t.Errorf("expected permissions '0755', got %q", sc.Parameters["permissions"])
	}
}

// ---------------------------------------------------------------------------
// 2. Tiered pool produces one SC per tier
// ---------------------------------------------------------------------------

func TestStorageClassesForPool_Tiered(t *testing.T) {
	pool := &v1alpha1.FileSharePool{
		ObjectMeta: metav1.ObjectMeta{
			Name: "tiered-pool",
			UID:  types.UID("uid-456"),
		},
		TypeMeta: metav1.TypeMeta{
			APIVersion: "storage.ibmcloud.io/v1alpha1",
			Kind:       "FileSharePool",
		},
		Spec: v1alpha1.FileSharePoolSpec{
			Zone:               "us-south-1",
			DefaultPermissions: "0755",
			Tiers: []v1alpha1.ShareTier{
				{Name: "standard", Profile: "dp2", ShareSizeGB: 1000, MaxShares: 5, InitialShares: 1},
				{Name: "premium", Profile: "custom", ShareSizeGB: 500, MaxShares: 3, InitialShares: 1},
			},
		},
	}

	classes := storageClassesForPool(pool)
	if len(classes) != 2 {
		t.Fatalf("expected 2 StorageClasses, got %d", len(classes))
	}

	// standard tier
	if classes[0].Name != "tiered-pool-standard" {
		t.Errorf("expected name 'tiered-pool-standard', got %q", classes[0].Name)
	}
	if classes[0].Parameters["tier"] != "standard" {
		t.Errorf("expected tier 'standard', got %q", classes[0].Parameters["tier"])
	}

	// premium tier
	if classes[1].Name != "tiered-pool-premium" {
		t.Errorf("expected name 'tiered-pool-premium', got %q", classes[1].Name)
	}
	if classes[1].Parameters["tier"] != "premium" {
		t.Errorf("expected tier 'premium', got %q", classes[1].Parameters["tier"])
	}
}

// ---------------------------------------------------------------------------
// 3. Default mount options
// ---------------------------------------------------------------------------

func TestBuildStorageClass_DefaultMountOptions(t *testing.T) {
	pool := &v1alpha1.FileSharePool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-defaults", UID: types.UID("uid-789")},
		TypeMeta:   metav1.TypeMeta{APIVersion: "storage.ibmcloud.io/v1alpha1", Kind: "FileSharePool"},
		Spec:       v1alpha1.FileSharePoolSpec{Zone: "us-south-1"},
	}

	sc := buildStorageClass(pool, "")
	expected := []string{"nfsvers=4.1", "soft", "timeo=600", "retrans=3"}
	if len(sc.MountOptions) != len(expected) {
		t.Fatalf("expected %d mount options, got %d", len(expected), len(sc.MountOptions))
	}
	for i, opt := range expected {
		if sc.MountOptions[i] != opt {
			t.Errorf("mount option [%d]: expected %q, got %q", i, opt, sc.MountOptions[i])
		}
	}
}

// ---------------------------------------------------------------------------
// 4. Custom mount options override defaults
// ---------------------------------------------------------------------------

func TestBuildStorageClass_CustomMountOptions(t *testing.T) {
	pool := &v1alpha1.FileSharePool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-custom", UID: types.UID("uid-custom")},
		TypeMeta:   metav1.TypeMeta{APIVersion: "storage.ibmcloud.io/v1alpha1", Kind: "FileSharePool"},
		Spec: v1alpha1.FileSharePoolSpec{
			Zone:         "us-south-1",
			MountOptions: []string{"nfsvers=4.1", "hard", "timeo=300"},
		},
	}

	sc := buildStorageClass(pool, "")
	if len(sc.MountOptions) != 3 {
		t.Fatalf("expected 3 mount options, got %d", len(sc.MountOptions))
	}
	if sc.MountOptions[1] != "hard" {
		t.Errorf("expected 'hard', got %q", sc.MountOptions[1])
	}
}

// ---------------------------------------------------------------------------
// 5. UID/GID propagation
// ---------------------------------------------------------------------------

func TestBuildStorageClass_UIDGIDPropagation(t *testing.T) {
	uid := int64(1000)
	gid := int64(2000)
	pool := &v1alpha1.FileSharePool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-uidgid", UID: types.UID("uid-ug")},
		TypeMeta:   metav1.TypeMeta{APIVersion: "storage.ibmcloud.io/v1alpha1", Kind: "FileSharePool"},
		Spec: v1alpha1.FileSharePoolSpec{
			Zone:       "us-south-1",
			DefaultUID: &uid,
			DefaultGID: &gid,
		},
	}

	sc := buildStorageClass(pool, "")
	if sc.Parameters["uid"] != "1000" {
		t.Errorf("expected uid '1000', got %q", sc.Parameters["uid"])
	}
	if sc.Parameters["gid"] != "2000" {
		t.Errorf("expected gid '2000', got %q", sc.Parameters["gid"])
	}
}

// ---------------------------------------------------------------------------
// 6. No UID/GID when not set
// ---------------------------------------------------------------------------

func TestBuildStorageClass_NoUIDGID(t *testing.T) {
	pool := &v1alpha1.FileSharePool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-nouid", UID: types.UID("uid-nouid")},
		TypeMeta:   metav1.TypeMeta{APIVersion: "storage.ibmcloud.io/v1alpha1", Kind: "FileSharePool"},
		Spec:       v1alpha1.FileSharePoolSpec{Zone: "us-south-1"},
	}

	sc := buildStorageClass(pool, "")
	if _, has := sc.Parameters["uid"]; has {
		t.Error("expected no uid parameter when DefaultUID is nil")
	}
	if _, has := sc.Parameters["gid"]; has {
		t.Error("expected no gid parameter when DefaultGID is nil")
	}
}

// ---------------------------------------------------------------------------
// 7. WaitForFirstConsumer when accessor zones are set
// ---------------------------------------------------------------------------

func TestBuildStorageClass_WFFCForMultiZone(t *testing.T) {
	pool := &v1alpha1.FileSharePool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-mz", UID: types.UID("uid-mz")},
		TypeMeta:   metav1.TypeMeta{APIVersion: "storage.ibmcloud.io/v1alpha1", Kind: "FileSharePool"},
		Spec: v1alpha1.FileSharePoolSpec{
			Zone: "us-south-1",
			AccessorZones: []v1alpha1.AccessorZone{
				{Zone: "us-south-2", SubnetID: "subnet-2"},
			},
		},
	}

	sc := buildStorageClass(pool, "")
	if *sc.VolumeBindingMode != storagev1.VolumeBindingWaitForFirstConsumer {
		t.Errorf("expected WaitForFirstConsumer, got %v", *sc.VolumeBindingMode)
	}
}

// ---------------------------------------------------------------------------
// 8. Immediate binding when no accessor zones
// ---------------------------------------------------------------------------

func TestBuildStorageClass_ImmediateForSingleZone(t *testing.T) {
	pool := &v1alpha1.FileSharePool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-sz", UID: types.UID("uid-sz")},
		TypeMeta:   metav1.TypeMeta{APIVersion: "storage.ibmcloud.io/v1alpha1", Kind: "FileSharePool"},
		Spec:       v1alpha1.FileSharePoolSpec{Zone: "us-south-1"},
	}

	sc := buildStorageClass(pool, "")
	if *sc.VolumeBindingMode != storagev1.VolumeBindingImmediate {
		t.Errorf("expected Immediate, got %v", *sc.VolumeBindingMode)
	}
}

// ---------------------------------------------------------------------------
// 9. OwnerReference is set correctly
// ---------------------------------------------------------------------------

func TestBuildStorageClass_OwnerReference(t *testing.T) {
	pool := &v1alpha1.FileSharePool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-owner", UID: types.UID("uid-owner")},
		TypeMeta:   metav1.TypeMeta{APIVersion: "storage.ibmcloud.io/v1alpha1", Kind: "FileSharePool"},
		Spec:       v1alpha1.FileSharePoolSpec{Zone: "us-south-1"},
	}

	sc := buildStorageClass(pool, "")
	if len(sc.OwnerReferences) != 1 {
		t.Fatalf("expected 1 OwnerReference, got %d", len(sc.OwnerReferences))
	}
	ref := sc.OwnerReferences[0]
	if ref.Name != "pool-owner" {
		t.Errorf("expected OwnerReference name 'pool-owner', got %q", ref.Name)
	}
	if ref.UID != types.UID("uid-owner") {
		t.Errorf("expected OwnerReference UID 'uid-owner', got %q", ref.UID)
	}
	if ref.APIVersion != "storage.ibmcloud.io/v1alpha1" {
		t.Errorf("expected APIVersion 'storage.ibmcloud.io/v1alpha1', got %q", ref.APIVersion)
	}
	if ref.Kind != "FileSharePool" {
		t.Errorf("expected Kind 'FileSharePool', got %q", ref.Kind)
	}
}

// ---------------------------------------------------------------------------
// 10. Labels are set correctly
// ---------------------------------------------------------------------------

func TestBuildStorageClass_Labels(t *testing.T) {
	pool := &v1alpha1.FileSharePool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-labels", UID: types.UID("uid-labels")},
		TypeMeta:   metav1.TypeMeta{APIVersion: "storage.ibmcloud.io/v1alpha1", Kind: "FileSharePool"},
		Spec:       v1alpha1.FileSharePoolSpec{Zone: "us-south-1"},
	}

	sc := buildStorageClass(pool, "")
	if sc.Labels[managedByLabel] != managedByValue {
		t.Errorf("expected managed-by label %q, got %q", managedByValue, sc.Labels[managedByLabel])
	}
	if sc.Labels[poolLabel] != "pool-labels" {
		t.Errorf("expected pool label 'pool-labels', got %q", sc.Labels[poolLabel])
	}
}

// ---------------------------------------------------------------------------
// 11. AllowVolumeExpansion and ReclaimPolicy
// ---------------------------------------------------------------------------

func TestBuildStorageClass_ReclaimAndExpansion(t *testing.T) {
	pool := &v1alpha1.FileSharePool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-rp", UID: types.UID("uid-rp")},
		TypeMeta:   metav1.TypeMeta{APIVersion: "storage.ibmcloud.io/v1alpha1", Kind: "FileSharePool"},
		Spec:       v1alpha1.FileSharePoolSpec{Zone: "us-south-1"},
	}

	sc := buildStorageClass(pool, "")
	if sc.AllowVolumeExpansion == nil || !*sc.AllowVolumeExpansion {
		t.Error("expected AllowVolumeExpansion to be true")
	}
	if sc.ReclaimPolicy == nil || string(*sc.ReclaimPolicy) != "Delete" {
		t.Errorf("expected ReclaimPolicy 'Delete', got %v", sc.ReclaimPolicy)
	}
}

// ---------------------------------------------------------------------------
// 12. Opt-out annotation
// ---------------------------------------------------------------------------

func TestShouldSkipStorageClass(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		expected    bool
	}{
		{"no annotations", nil, false},
		{"empty annotations", map[string]string{}, false},
		{"annotation false", map[string]string{SkipStorageClassAnnotation: "false"}, false},
		{"annotation true", map[string]string{SkipStorageClassAnnotation: "true"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := &v1alpha1.FileSharePool{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test",
					Annotations: tt.annotations,
				},
			}
			if got := shouldSkipStorageClass(pool); got != tt.expected {
				t.Errorf("shouldSkipStorageClass() = %v, want %v", got, tt.expected)
			}
		})
	}
}
