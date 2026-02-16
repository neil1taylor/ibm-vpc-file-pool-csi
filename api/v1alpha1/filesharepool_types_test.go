package v1alpha1

import (
	"testing"
)

func TestTierConfig_NoTiers_ReturnsTopLevel(t *testing.T) {
	iops := int64(5000)
	spec := &FileSharePoolSpec{
		Profile:       "dp2",
		ShareSizeGB:   1000,
		IOPS:          &iops,
		MaxShares:     10,
		InitialShares: 2,
	}

	profile, sizeGB, gotIOPS, maxShares, initialShares, err := spec.TierConfig("")
	if err != nil {
		t.Fatalf("TierConfig failed: %v", err)
	}
	if profile != "dp2" {
		t.Errorf("expected profile 'dp2', got %q", profile)
	}
	if sizeGB != 1000 {
		t.Errorf("expected sizeGB 1000, got %d", sizeGB)
	}
	if gotIOPS == nil || *gotIOPS != 5000 {
		t.Errorf("expected IOPS 5000, got %v", gotIOPS)
	}
	if maxShares != 10 {
		t.Errorf("expected maxShares 10, got %d", maxShares)
	}
	if initialShares != 2 {
		t.Errorf("expected initialShares 2, got %d", initialShares)
	}
}

func TestTierConfig_NoTiers_IgnoresTierName(t *testing.T) {
	spec := &FileSharePoolSpec{
		Profile:       "dp2",
		ShareSizeGB:   1000,
		MaxShares:     10,
		InitialShares: 1,
	}

	// When no tiers are defined, any tier name should still return top-level fields
	profile, _, _, _, _, err := spec.TierConfig("anything")
	if err != nil {
		t.Fatalf("TierConfig failed: %v", err)
	}
	if profile != "dp2" {
		t.Errorf("expected profile 'dp2', got %q", profile)
	}
}

func TestTierConfig_NamedTier_ReturnsMatch(t *testing.T) {
	premiumIOPS := int64(10000)
	spec := &FileSharePoolSpec{
		Profile:       "dp2",
		ShareSizeGB:   1000,
		MaxShares:     10,
		InitialShares: 1,
		Tiers: []ShareTier{
			{Name: "standard", Profile: "dp2", ShareSizeGB: 1000, MaxShares: 5, InitialShares: 2},
			{Name: "premium", Profile: "custom", ShareSizeGB: 500, IOPS: &premiumIOPS, MaxShares: 3, InitialShares: 1},
		},
	}

	profile, sizeGB, gotIOPS, maxShares, initialShares, err := spec.TierConfig("premium")
	if err != nil {
		t.Fatalf("TierConfig failed: %v", err)
	}
	if profile != "custom" {
		t.Errorf("expected profile 'custom', got %q", profile)
	}
	if sizeGB != 500 {
		t.Errorf("expected sizeGB 500, got %d", sizeGB)
	}
	if gotIOPS == nil || *gotIOPS != 10000 {
		t.Errorf("expected IOPS 10000, got %v", gotIOPS)
	}
	if maxShares != 3 {
		t.Errorf("expected maxShares 3, got %d", maxShares)
	}
	if initialShares != 1 {
		t.Errorf("expected initialShares 1, got %d", initialShares)
	}
}

func TestTierConfig_UnknownTier_ReturnsError(t *testing.T) {
	spec := &FileSharePoolSpec{
		Tiers: []ShareTier{
			{Name: "standard", Profile: "dp2", ShareSizeGB: 1000, MaxShares: 5, InitialShares: 1},
		},
	}

	_, _, _, _, _, err := spec.TierConfig("nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown tier")
	}
}

func TestTierConfig_EmptyNameWithTiers_ReturnsError(t *testing.T) {
	spec := &FileSharePoolSpec{
		Tiers: []ShareTier{
			{Name: "standard", Profile: "dp2", ShareSizeGB: 1000, MaxShares: 5, InitialShares: 1},
		},
	}

	_, _, _, _, _, err := spec.TierConfig("")
	if err == nil {
		t.Fatal("expected error when tier is empty but pool has tiers")
	}
}

// ---------------------------------------------------------------------------
// Cross-Zone Helper Method Tests
// ---------------------------------------------------------------------------

func TestMountTargetIPForZone_Found(t *testing.T) {
	share := &PoolShareStatus{
		MountTargetIP: "10.0.0.1",
		MountTargets: []ZoneMountTarget{
			{Zone: "us-south-1", MountTargetIP: "10.0.0.1"},
			{Zone: "us-south-2", MountTargetIP: "10.0.1.1"},
			{Zone: "us-south-3", MountTargetIP: "10.0.2.1"},
		},
	}

	if got := share.MountTargetIPForZone("us-south-2"); got != "10.0.1.1" {
		t.Errorf("expected 10.0.1.1, got %q", got)
	}
}

func TestMountTargetIPForZone_FallsBackToPrimary(t *testing.T) {
	share := &PoolShareStatus{
		MountTargetIP: "10.0.0.1",
		MountTargets: []ZoneMountTarget{
			{Zone: "us-south-1", MountTargetIP: "10.0.0.1"},
		},
	}

	if got := share.MountTargetIPForZone("eu-de-1"); got != "10.0.0.1" {
		t.Errorf("expected fallback to primary 10.0.0.1, got %q", got)
	}
}

func TestMountTargetIPForZone_EmptyMountTargets(t *testing.T) {
	share := &PoolShareStatus{
		MountTargetIP: "10.0.0.1",
	}

	if got := share.MountTargetIPForZone("us-south-1"); got != "10.0.0.1" {
		t.Errorf("expected fallback to primary 10.0.0.1, got %q", got)
	}
}

func TestAllAccessibleZones_IncludesHomeAndAccessors(t *testing.T) {
	spec := &FileSharePoolSpec{
		Zone: "us-south-1",
		AccessorZones: []AccessorZone{
			{Zone: "us-south-2", SubnetID: "subnet-2"},
			{Zone: "us-south-3", SubnetID: "subnet-3"},
		},
	}

	zones := spec.AllAccessibleZones()
	if len(zones) != 3 {
		t.Fatalf("expected 3 zones, got %d: %v", len(zones), zones)
	}
	if zones[0] != "us-south-1" {
		t.Errorf("expected first zone 'us-south-1', got %q", zones[0])
	}
	if zones[1] != "us-south-2" {
		t.Errorf("expected second zone 'us-south-2', got %q", zones[1])
	}
	if zones[2] != "us-south-3" {
		t.Errorf("expected third zone 'us-south-3', got %q", zones[2])
	}
}

func TestAllAccessibleZones_NoAccessors_HomeOnly(t *testing.T) {
	spec := &FileSharePoolSpec{
		Zone: "us-south-1",
	}

	zones := spec.AllAccessibleZones()
	if len(zones) != 1 {
		t.Fatalf("expected 1 zone, got %d: %v", len(zones), zones)
	}
	if zones[0] != "us-south-1" {
		t.Errorf("expected zone 'us-south-1', got %q", zones[0])
	}
}

func TestIsAccessibleZone(t *testing.T) {
	spec := &FileSharePoolSpec{
		Zone: "us-south-1",
		AccessorZones: []AccessorZone{
			{Zone: "us-south-2", SubnetID: "subnet-2"},
		},
	}

	if !spec.IsAccessibleZone("us-south-1") {
		t.Error("home zone should be accessible")
	}
	if !spec.IsAccessibleZone("us-south-2") {
		t.Error("accessor zone should be accessible")
	}
	if spec.IsAccessibleZone("eu-de-1") {
		t.Error("unknown zone should not be accessible")
	}
}

func TestPoolShareStatus_AllAccessibleZones(t *testing.T) {
	share := &PoolShareStatus{
		MountTargets: []ZoneMountTarget{
			{Zone: "us-south-1", MountTargetIP: "10.0.0.1"},
			{Zone: "us-south-2", MountTargetIP: "10.0.1.1"},
		},
	}

	zones := share.AllAccessibleZones()
	if len(zones) != 2 {
		t.Fatalf("expected 2 zones, got %d", len(zones))
	}
}
