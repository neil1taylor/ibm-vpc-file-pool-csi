package util

import "testing"

func TestParseVolumeID_Valid(t *testing.T) {
	tests := []struct {
		name                          string
		id                            string
		wantPool, wantShare, wantName string
	}{
		{
			name:      "standard_volume",
			id:        "my-pool/r006-abc123/pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab",
			wantPool:  "my-pool",
			wantShare: "r006-abc123",
			wantName:  "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab",
		},
		{
			name:      "snapshot_id",
			id:        "my-pool/r006-abc123/snap-my-snapshot",
			wantPool:  "my-pool",
			wantShare: "r006-abc123",
			wantName:  "snap-my-snapshot",
		},
		{
			name:      "name_with_slashes",
			id:        "pool/share/name/with/slashes",
			wantPool:  "pool",
			wantShare: "share",
			wantName:  "name/with/slashes",
		},
		{
			name:      "minimal",
			id:        "a/b/c",
			wantPool:  "a",
			wantShare: "b",
			wantName:  "c",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool, share, name, err := ParseVolumeID(tt.id)
			if err != nil {
				t.Fatalf("ParseVolumeID(%q) error: %v", tt.id, err)
			}
			if pool != tt.wantPool {
				t.Errorf("pool = %q, want %q", pool, tt.wantPool)
			}
			if share != tt.wantShare {
				t.Errorf("share = %q, want %q", share, tt.wantShare)
			}
			if name != tt.wantName {
				t.Errorf("name = %q, want %q", name, tt.wantName)
			}
		})
	}
}

func TestParseVolumeID_Invalid(t *testing.T) {
	tests := []struct {
		name string
		id   string
	}{
		{"empty", ""},
		{"no_slashes", "no-slashes-here"},
		{"one_slash", "pool/share"},
		{"single_slash", "/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, _, err := ParseVolumeID(tt.id)
			if err == nil {
				t.Errorf("ParseVolumeID(%q) = nil error, want error", tt.id)
			}
		})
	}
}
