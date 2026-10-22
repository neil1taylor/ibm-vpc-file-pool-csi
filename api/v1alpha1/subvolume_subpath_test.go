package v1alpha1

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The pattern the API server enforces on spec.subPath is generated from the
// kubebuilder marker. SubPathPattern is the same rule for Go code (the failover
// tool checks it in a dry run), so the two must not drift apart (#209).
func TestSubPathPattern_MatchesTheCRD(t *testing.T) {
	b, err := os.ReadFile("../../config/crd/storage.ibmcloud.io_subvolumes.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var found string
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "pattern: "); ok && strings.Contains(rest, "/pvcs/") {
			found = rest
		}
	}
	if found != SubPathPattern {
		t.Fatalf("CRD subPath pattern %q differs from SubPathPattern %q", found, SubPathPattern)
	}
}

func TestSubPathPattern_Accepts(t *testing.T) {
	re := regexp.MustCompile(SubPathPattern)
	for _, p := range []string{
		"/pvcs/pvc-abc12345-1234-5678-9abc-def012345678",
		"/pvcs/pvc-abc12345-1234-5678-9abc-def012345678.rewind-1761129900",
	} {
		if !re.MatchString(p) {
			t.Errorf("%q should match", p)
		}
	}
	for _, p := range []string{
		"/pvcs/../etc",
		"/pvcs/pvc-abc.rewind-",
		"/pvcs/pvc-abc.rewind-1/../x",
		"/pvcs/pvc-abc.rewind-1.part",
		"/pvcs/pvc-abc/.rewind-1",
		"/pvcs/pvc-abc.rewind-1\n",
	} {
		if re.MatchString(p) {
			t.Errorf("%q must not match", p)
		}
	}
}
