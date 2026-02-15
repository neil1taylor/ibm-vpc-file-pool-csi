package util

import (
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
