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
