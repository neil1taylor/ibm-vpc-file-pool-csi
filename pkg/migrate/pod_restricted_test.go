package migrate

import (
	"regexp"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// A user with only the edit role gets the restricted rules on OpenShift: a
// random user id, no root and no package install (issue #196). The copy pod
// must work like that.

func migrationPodForTest() *corev1.Pod {
	return BuildMigrationPod(MigrationPodSpec{Name: "migrate-data", Namespace: "apps", SourcePVC: "data", TargetPVC: "data-pooled"})
}

func TestBuildMigrationPod_InstallsNothingAtStart(t *testing.T) {
	cmd := strings.Join(migrationPodForTest().Spec.Containers[0].Command, " ")
	for _, banned := range []string{"apk ", "dnf ", "yum ", "microdnf ", "apt-get "} {
		if strings.Contains(cmd, banned) {
			t.Errorf("the copy script runs %q: a random non-root user cannot install packages", banned)
		}
	}
}

func TestBuildMigrationPod_UsesAnImageThatHasRsync(t *testing.T) {
	image := migrationPodForTest().Spec.Containers[0].Image
	if want := "image-registry.openshift-image-registry.svc:5000/openshift/tools:latest"; image != want {
		t.Errorf("image = %q, want %q: it holds rsync and find and needs no pull secret", image, want)
	}
}

func TestBuildMigrationPod_FitsTheRestrictedRules(t *testing.T) {
	pod := migrationPodForTest()
	c := pod.Spec.Containers[0]
	if c.SecurityContext == nil || c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation {
		t.Error("allowPrivilegeEscalation must be false")
	}
	if c.SecurityContext == nil || c.SecurityContext.Capabilities == nil ||
		len(c.SecurityContext.Capabilities.Drop) != 1 || c.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Error("the container must drop ALL capabilities")
	}
	if pod.Spec.SecurityContext == nil || pod.Spec.SecurityContext.SeccompProfile == nil ||
		pod.Spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Error("the pod must use the RuntimeDefault seccomp profile")
	}
	if c.SecurityContext != nil && c.SecurityContext.RunAsUser != nil || pod.Spec.SecurityContext != nil && pod.Spec.SecurityContext.RunAsUser != nil {
		t.Error("no user id may be set: the cluster picks one that its rules allow")
	}
}

var rsyncFlags = regexp.MustCompile(`(?m)^rsync (-[A-Za-z]+)`)

func TestBuildMigrationPod_CopiesWithoutChangingOwners(t *testing.T) {
	cmd := strings.Join(migrationPodForTest().Spec.Containers[0].Command, " ")
	m := rsyncFlags.FindStringSubmatch(cmd)
	if m == nil {
		t.Fatalf("no rsync line in %q", cmd)
	}
	for _, bad := range "aogp" {
		if strings.ContainsRune(m[1], bad) {
			t.Errorf("rsync flags %s include %q: a user who is not root cannot set owners or the mode of the top folder", m[1], bad)
		}
	}
	for _, want := range "rltS" {
		if !strings.ContainsRune(m[1], want) {
			t.Errorf("rsync flags %s must include %q (recurse, links, file times, holes)", m[1], want)
		}
	}
	// The top folder of the new claim belongs to the pool's user. Setting its
	// times or mode fails for anyone else and would fail the whole copy.
	if !strings.Contains(cmd, "--omit-dir-times") {
		t.Error("rsync must not set folder times: the target's top folder belongs to another user")
	}
}
