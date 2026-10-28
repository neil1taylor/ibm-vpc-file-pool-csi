package webhook

import (
	"context"
	"testing"
)

const rewindUUID = "pvc-abc12345-1234-5678-9abc-def012345678"

// A failover to an earlier restore point makes a SubVolume whose folder is the
// volume's folder plus ".rewind-<epoch>" (#209). Exactly that form is allowed.
func TestSubVolumeValidator_Create_RewindSubPath(t *testing.T) {
	v := &SubVolumeValidator{}
	tests := []struct {
		name    string
		subPath string
		ok      bool
	}{
		{"rewind folder", "/pvcs/" + rewindUUID + ".rewind-1761129900", true},
		{"plain folder", "/pvcs/" + rewindUUID, true},
		{"empty epoch", "/pvcs/" + rewindUUID + ".rewind-", false},
		{"epoch not digits", "/pvcs/" + rewindUUID + ".rewind-abc", false},
		{"epoch with sign", "/pvcs/" + rewindUUID + ".rewind--1", false},
		{"traversal in epoch", "/pvcs/" + rewindUUID + ".rewind-1/../../etc", false},
		{"traversal after name", "/pvcs/" + rewindUUID + "/../x.rewind-1", false},
		{"part folder", "/pvcs/" + rewindUUID + ".rewind-1.part", false},
		{"other suffix", "/pvcs/" + rewindUUID + ".bak", false},
		{"rewind twice", "/pvcs/" + rewindUUID + ".rewind-1.rewind-2", false},
		{"rewind as a subfolder", "/pvcs/" + rewindUUID + "/.rewind-1", false},
		{"trailing slash", "/pvcs/" + rewindUUID + ".rewind-1/", false},
		{"newline", "/pvcs/" + rewindUUID + ".rewind-1\n", false},
		{"epoch too long", "/pvcs/" + rewindUUID + ".rewind-1234567890123456", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sv := validSubVolume()
			sv.Spec.SubPath = tt.subPath
			_, err := v.ValidateCreate(context.Background(), sv)
			if tt.ok && err != nil {
				t.Fatalf("subPath %q: unexpected error %v", tt.subPath, err)
			}
			if !tt.ok && err == nil {
				t.Fatalf("subPath %q: expected an error", tt.subPath)
			}
		})
	}
}
