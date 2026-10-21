package util

import (
	"strings"
	"testing"
)

func TestValidateSubDir_Valid(t *testing.T) {
	valid := []string{
		"/pvcs/pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab",
		"/pvcs/pvc-00000000-0000-0000-0000-000000000000",
		"/pvcs/pvc-ffffffff-ffff-ffff-ffff-ffffffffffff",
	}
	for _, s := range valid {
		if err := ValidateSubDir(s); err != nil {
			t.Errorf("ValidateSubDir(%q) = %v, want nil", s, err)
		}
	}
}

func TestValidateSubDir_Invalid(t *testing.T) {
	tests := []struct {
		name   string
		subDir string
	}{
		{"empty", ""},
		{"just_slash", "/"},
		{"no_leading_slash", "pvcs/pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"},
		{"wrong_prefix", "/volumes/pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"},
		{"no_pvc_prefix", "/pvcs/a1b2c3d4-5678-90ab-cdef-1234567890ab"},
		{"uppercase_hex", "/pvcs/pvc-A1B2C3D4-5678-90AB-CDEF-1234567890AB"},
		{"short_uuid", "/pvcs/pvc-a1b2c3d4"},
		{"extra_trailing_slash", "/pvcs/pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab/"},
		{"nested", "/pvcs/pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab/data"},
		{"traversal_dotdot", "/pvcs/../etc/passwd"},
		{"traversal_embedded", "/pvcs/pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab/../../etc"},
		{"dot_only", "."},
		{"dotdot_only", ".."},
		{"spaces", "/pvcs/pvc a1b2c3d4-5678-90ab-cdef-1234567890ab"},
		{"not_a_uuid", "/pvcs/pvc-not-a-valid-uuid-at-all-nope"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateSubDir(tt.subDir); err == nil {
				t.Errorf("ValidateSubDir(%q) = nil, want error", tt.subDir)
			}
		})
	}
}

func TestSafeJoin_Valid(t *testing.T) {
	tests := []struct {
		name string
		base string
		sub  string
		want string
	}{
		{
			name: "normal",
			base: "/mnt/staging",
			sub:  "/pvcs/pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab",
			want: "/mnt/staging/pvcs/pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab",
		},
		{
			name: "trailing_slash_base",
			base: "/mnt/staging/",
			sub:  "/pvcs/pvc-00000000-0000-0000-0000-000000000000",
			want: "/mnt/staging/pvcs/pvc-00000000-0000-0000-0000-000000000000",
		},
		{
			name: "deep_base",
			base: "/var/lib/kubelet/plugins/file-pool-csi/staging",
			sub:  "/pvcs/pvc-ffffffff-ffff-ffff-ffff-ffffffffffff",
			want: "/var/lib/kubelet/plugins/file-pool-csi/staging/pvcs/pvc-ffffffff-ffff-ffff-ffff-ffffffffffff",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SafeJoin(tt.base, tt.sub)
			if err != nil {
				t.Fatalf("SafeJoin(%q, %q) error: %v", tt.base, tt.sub, err)
			}
			if got != tt.want {
				t.Errorf("SafeJoin(%q, %q) = %q, want %q", tt.base, tt.sub, got, tt.want)
			}
		})
	}
}

func TestSafeJoin_InvalidSubDir(t *testing.T) {
	invalidSubs := []string{
		"",
		"/pvcs/../etc/passwd",
		"pvcs/pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab",
		"/volumes/something",
	}
	for _, sub := range invalidSubs {
		_, err := SafeJoin("/mnt/staging", sub)
		if err == nil {
			t.Errorf("SafeJoin(/mnt/staging, %q) = nil error, want error", sub)
		}
	}
}

// ---------------------------------------------------------------------------
// ValidateSnapshotPath Tests
// ---------------------------------------------------------------------------

func TestValidateSnapshotPath_Valid(t *testing.T) {
	valid := []string{
		"/pvcs/.snapshots/snap-abc123",
		"/pvcs/.snapshots/snap-my-snapshot-name",
		"/pvcs/.snapshots/snap-A1B2C3",
		"/pvcs/.snapshots/snap-pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab",
	}
	for _, s := range valid {
		if err := ValidateSnapshotPath(s); err != nil {
			t.Errorf("ValidateSnapshotPath(%q) = %v, want nil", s, err)
		}
	}
}

func TestValidateSnapshotPath_Invalid(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{"empty", ""},
		{"just_slash", "/"},
		{"no_snap_prefix", "/pvcs/.snapshots/abc123"},
		{"wrong_base", "/volumes/.snapshots/snap-abc123"},
		{"missing_snapshots_dir", "/pvcs/snap-abc123"},
		{"traversal", "/pvcs/.snapshots/../../../etc/passwd"},
		{"trailing_slash", "/pvcs/.snapshots/snap-abc123/"},
		{"nested", "/pvcs/.snapshots/snap-abc123/data"},
		{"spaces", "/pvcs/.snapshots/snap abc123"},
		{"dotdot", "/pvcs/..snapshots/snap-abc123"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateSnapshotPath(tt.path); err == nil {
				t.Errorf("ValidateSnapshotPath(%q) = nil, want error", tt.path)
			}
		})
	}
}

func TestSafeJoinSnapshot_Valid(t *testing.T) {
	got, err := SafeJoinSnapshot("/mnt/staging", "/pvcs/.snapshots/snap-abc123")
	if err != nil {
		t.Fatalf("SafeJoinSnapshot failed: %v", err)
	}
	want := "/mnt/staging/pvcs/.snapshots/snap-abc123"
	if got != want {
		t.Errorf("SafeJoinSnapshot = %q, want %q", got, want)
	}
}

func TestSafeJoinSnapshot_InvalidPath(t *testing.T) {
	_, err := SafeJoinSnapshot("/mnt/staging", "/pvcs/../etc/passwd")
	if err == nil {
		t.Error("SafeJoinSnapshot should reject invalid snapshot path")
	}
}

const backupPV = "pvc-11111111-2222-3333-4444-555555555555"

func TestValidateBackupPath_Valid(t *testing.T) {
	for _, p := range []string{
		"/backup/nightly/pvcs/" + backupPV,
		"/backup/a/pvcs/" + backupPV,
		"/backup/my-policy.v2/pvcs/" + backupPV,
	} {
		if err := ValidateBackupPath(p); err != nil {
			t.Errorf("ValidateBackupPath(%q) = %v, want nil", p, err)
		}
	}
	for _, p := range []string{
		"/backup/nightly/.moments/pvcs/" + backupPV,
		"/backup/a/.moments/pvcs/" + backupPV,
	} {
		if err := ValidateBackupMomentsPath(p); err != nil {
			t.Errorf("ValidateBackupMomentsPath(%q) = %v, want nil", p, err)
		}
	}
}

func TestValidateBackupPath_Invalid(t *testing.T) {
	bad := []string{
		"",
		"/backup",
		"/backup/nightly",
		"/backup/nightly/pvcs/" + backupPV + "/",
		"/backup/nightly/pvcs/" + backupPV + "/x",
		"/backup/../pvcs/" + backupPV,
		"/backup/nightly/../other/pvcs/" + backupPV,
		"/backup//pvcs/" + backupPV,
		"/backup/Night ly/pvcs/" + backupPV,
		"/backup/-x/pvcs/" + backupPV,
		"/backup/x-/pvcs/" + backupPV,
		"/backup/a..b/pvcs/" + backupPV,
		"/pvcs/" + backupPV,
		"/backup/nightly/pvcs/pvc-short",
		"/backup/nightly/.moments/pvcs/" + backupPV, // that is a moments path, not a volume path
		"backup/nightly/pvcs/" + backupPV,
	}
	for _, p := range bad {
		if err := ValidateBackupPath(p); err == nil {
			t.Errorf("ValidateBackupPath(%q) accepted an unsafe path", p)
		}
	}
	for _, p := range append(bad, "/backup/nightly/pvcs/"+backupPV) { // a volume path is not a moments path
		if err := ValidateBackupMomentsPath(p); err == nil && p != "/backup/nightly/.moments/pvcs/"+backupPV {
			t.Errorf("ValidateBackupMomentsPath(%q) accepted a bad path", p)
		}
	}
	long := "/backup/" + strings.Repeat("a", 64) + "/pvcs/" + backupPV
	if err := ValidateBackupPath(long); err == nil {
		t.Error("a policy name of 64 characters does not fit a label value and must be refused")
	}
}
