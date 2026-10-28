package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}").Output()
	if err != nil {
		t.Skipf("go list: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// The browse image has its own Dockerfile, which CI does not build. These tests
// stand in for the build: they catch a Kopia version that drifted from the
// driver image's, and a package the binary imports that the Dockerfile does not
// copy (which would only fail on the build server).
func TestDockerfileBrowse_KopiaIsTheSameAsTheDriverImages(t *testing.T) {
	root := repoRoot(t)
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	pick := func(text, arg string) string {
		m := regexp.MustCompile(`(?m)^ARG ` + arg + `=(\S+)$`).FindStringSubmatch(text)
		if m == nil {
			t.Fatalf("no ARG %s", arg)
		}
		return m[1]
	}
	driver, browseText := read("Dockerfile"), read("Dockerfile.browse")
	for _, arg := range []string{"KOPIA_VERSION", "KOPIA_SHA256"} {
		if a, b := pick(driver, arg), pick(browseText, arg); a != b {
			t.Errorf("%s differs: driver %s, browse %s", arg, a, b)
		}
	}
	if !strings.Contains(browseText, "sha256sum -c") {
		t.Error("the Kopia download is not checked against its checksum")
	}
}

func TestDockerfileBrowse_CopiesEveryPackageTheBinaryImports(t *testing.T) {
	root := repoRoot(t)
	// The image is built for Linux, so list the packages as Linux sees them.
	cmd := exec.Command("go", "list", "-deps", "-f", "{{if not .Standard}}{{.Dir}}{{end}}", "./cmd/browse")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64")
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("go list: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(root, "Dockerfile.browse"))
	if err != nil {
		t.Fatal(err)
	}
	copied := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^COPY (\S+) \S+$`).FindAllStringSubmatch(string(b), -1) {
		copied[strings.TrimSuffix(m[1], "/")] = true
	}
	for _, dir := range strings.Fields(string(out)) {
		rel, err := filepath.Rel(root, dir)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue // a module dependency, fetched by go mod download
		}
		if !copied[rel] {
			t.Errorf("Dockerfile.browse does not COPY %s, which the binary imports", rel)
		}
	}
}
