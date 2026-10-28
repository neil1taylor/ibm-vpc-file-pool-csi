package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/gateway"
)

// restoreStub is a gateway that takes restore requests and answers their status
// from a script.
type restoreStub struct {
	mu       sync.Mutex
	requests []gateway.RestoreRequest
	// statuses are the answers to GET, one after the other; the last one repeats.
	statuses []gateway.Restore
	gets     int
	denied   bool
	badName  string
}

func newRestoreStub(t *testing.T) (*restoreStub, *gwClient) {
	t.Helper()
	s := &restoreStub{}
	mux := http.NewServeMux()
	fail := func(w http.ResponseWriter, code int, e gateway.ErrorBody) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(e)
	}
	mux.HandleFunc("POST /v1/restores", func(w http.ResponseWriter, r *http.Request) {
		if s.denied {
			fail(w, 403, gateway.ErrorBody{Code: "forbidden", Message: "you may not read the backups of that namespace"})
			return
		}
		var req gateway.RestoreRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		s.mu.Lock()
		s.requests = append(s.requests, req)
		s.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(gateway.Restore{Name: "bfr-abc", Phase: "Pending", TargetNamespace: req.TargetNamespace, TargetName: req.TargetName, Existing: req.Existing})
	})
	mux.HandleFunc("GET /v1/restores/{name}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.badName = r.PathValue("name")
		i := s.gets
		if i >= len(s.statuses) {
			i = len(s.statuses) - 1
		}
		s.gets++
		if i < 0 {
			fail(w, 404, gateway.ErrorBody{Code: "not_found", Message: "no such restore"})
			return
		}
		st := s.statuses[i]
		st.Name = r.PathValue("name")
		_ = json.NewEncoder(w).Encode(st)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return s, &gwClient{base: ts.URL, hc: ts.Client()}
}

// fastPoll makes the commands look at a restore every millisecond, for one test.
func fastPoll(t *testing.T) {
	t.Helper()
	old := pollEvery
	pollEvery = time.Millisecond
	t.Cleanup(func() { pollEvery = old })
}

func restoreArgs(extra ...string) []string {
	return append([]string{"--policy", "nightly", "--pvc", "apps/db", "--path", "/var/www/index.php", "--path", `/etc/my\x0aname`, "--to", "apps/db-files"}, extra...)
}

func runRF(t *testing.T, gw *gwClient, args []string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := runRestoreFiles(context.Background(), gw, &out, args)
	return out.String(), err
}

func TestRestoreFiles_AsksForANewClaimByDefault(t *testing.T) {
	s, gw := newRestoreStub(t)
	out, err := runRF(t, gw, restoreArgs("--moment", "2026-10-21T09:30:00Z", "--size-gb", "5", "--storage-class", "gold"))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.requests) != 1 {
		t.Fatalf("requests %d", len(s.requests))
	}
	r := s.requests[0]
	if r.Policy != "nightly" || r.Namespace != "apps" || r.PVC != "db" || r.TargetNamespace != "apps" || r.TargetName != "db-files" ||
		r.Existing || r.SizeGB != 5 || r.StorageClassName != "gold" || r.Moment == nil || !r.Moment.Equal(time.Date(2026, 10, 21, 9, 30, 0, 0, time.UTC)) {
		t.Errorf("request %+v", r)
	}
	if len(r.Paths) != 2 || r.Paths[0] != "/var/www/index.php" || r.Paths[1] != `/etc/my\x0aname` {
		t.Errorf("paths %q", r.Paths)
	}
	if !strings.Contains(out, "bfr-abc") || !strings.Contains(out, "new claim apps/db-files") || !strings.Contains(out, "--status bfr-abc") {
		t.Errorf("output %q", out)
	}
	if s.gets != 0 {
		t.Error("it waited without --wait")
	}
}

func TestRestoreFiles_ExistingClaimGetsANewFolder(t *testing.T) {
	s, gw := newRestoreStub(t)
	out, err := runRF(t, gw, restoreArgs("--existing"))
	if err != nil {
		t.Fatal(err)
	}
	if !s.requests[0].Existing || s.requests[0].SizeGB != 0 {
		t.Errorf("request %+v", s.requests[0])
	}
	if !strings.Contains(out, "new folder in the claim apps/db-files") {
		t.Errorf("output %q", out)
	}
	// The explicit word for the default is accepted.
	if _, err := runRF(t, gw, restoreArgs("--new-claim")); err != nil || s.requests[1].Existing {
		t.Errorf("--new-claim: %v %+v", err, s.requests[1])
	}
}

func TestRestoreFiles_PathsFromAFile(t *testing.T) {
	s, gw := newRestoreStub(t)
	f := filepath.Join(t.TempDir(), "paths.txt")
	if err := os.WriteFile(f, []byte("/a/one\n\n  /a/two  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"--policy", "nightly", "--pvc", "apps/db", "--paths-file", f, "--path", "/c", "--to", "apps/x"}
	if _, err := runRF(t, gw, args); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(s.requests[0].Paths, ","); got != "/c,/a/one,/a/two" {
		t.Errorf("paths %q", got)
	}
}

func TestRestoreFiles_BadArguments(t *testing.T) {
	cases := map[string][]string{
		"no paths":                     {"--policy", "nightly", "--pvc", "apps/db", "--to", "apps/x"},
		"a path with ..":               {"--policy", "nightly", "--pvc", "apps/db", "--path", "/a/../b", "--to", "apps/x"},
		"a bad escape":                 {"--policy", "nightly", "--pvc", "apps/db", "--path", `/a\q`, "--to", "apps/x"},
		"no target":                    {"--policy", "nightly", "--pvc", "apps/db", "--path", "/a"},
		"a target without a namespace": {"--policy", "nightly", "--pvc", "apps/db", "--path", "/a", "--to", "x"},
		"no policy":                    {"--pvc", "apps/db", "--path", "/a", "--to", "apps/x"},
		"no pvc":                       {"--policy", "nightly", "--path", "/a", "--to", "apps/x"},
		"a bad moment":                 {"--policy", "nightly", "--pvc", "apps/db", "--path", "/a", "--to", "apps/x", "--moment", "yesterday"},
		"both targets":                 {"--policy", "nightly", "--pvc", "apps/db", "--path", "/a", "--to", "apps/x", "--new-claim", "--existing"},
		"a size for an existing claim": {"--policy", "nightly", "--pvc", "apps/db", "--path", "/a", "--to", "apps/x", "--existing", "--size-gb", "3"},
		"a bad size":                   {"--policy", "nightly", "--pvc", "apps/db", "--path", "/a", "--to", "apps/x", "--size-gb", "lots"},
		"a missing paths file":         {"--policy", "nightly", "--pvc", "apps/db", "--paths-file", "/no/such/file", "--to", "apps/x"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			s, gw := newRestoreStub(t)
			if _, err := runRF(t, gw, args); err == nil {
				t.Fatal("accepted")
			}
			if len(s.requests) != 0 {
				t.Fatal("a request was sent")
			}
		})
	}
}

func TestRestoreFiles_WaitFollowsTheRestoreToDone(t *testing.T) {
	s, gw := newRestoreStub(t)
	fastPoll(t)
	s.statuses = []gateway.Restore{
		{Phase: "Measuring", Message: "measuring"},
		{Phase: "Restoring", Message: "copying"},
		{Phase: "Restoring", Message: "copying"},
		{Phase: "Done", Folder: "restored-nightly-20261021T093000Z", Files: 2, Dirs: 3, Links: 1, Bytes: 4096, Unreadable: 2, Skipped: 1, TargetNamespace: "apps", TargetName: "db-files", Existing: true},
	}
	out, err := runRF(t, gw, restoreArgs("--existing", "--wait"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Measuring", "Restoring", "Done", "restored-nightly-20261021T093000Z", "2 files", "2 could not be read"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q: %s", want, out)
		}
	}
	if strings.Count(out, "Restoring") != 1 {
		t.Errorf("a phase that did not change was printed again: %s", out)
	}
}

func TestRestoreFiles_WaitReportsAFailure(t *testing.T) {
	s, gw := newRestoreStub(t)
	fastPoll(t)
	s.statuses = []gateway.Restore{{Phase: "Failed", Message: "not enough space: the chosen files need 10 bytes"}}
	_, err := runRF(t, gw, restoreArgs("--wait"))
	if err == nil || !strings.Contains(err.Error(), "not enough space") {
		t.Fatalf("err %v", err)
	}
}

func TestRestoreFiles_WaitTimesOutAndSaysHowToGoOn(t *testing.T) {
	s, gw := newRestoreStub(t)
	fastPoll(t)
	s.statuses = []gateway.Restore{{Phase: "Restoring"}}
	_, err := runRF(t, gw, restoreArgs("--wait", "--timeout", "50ms"))
	if err == nil || !strings.Contains(err.Error(), "--status bfr-abc") {
		t.Fatalf("err %v", err)
	}
}

func TestRestoreFiles_Status(t *testing.T) {
	s, gw := newRestoreStub(t)
	s.statuses = []gateway.Restore{{Phase: "Done", Message: "restored 2 files", Folder: "restored-x", Files: 2, TargetNamespace: "apps", TargetName: "db-files", Existing: true}}
	out, err := runRF(t, gw, []string{"--status", "bfr-abc"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Done") || !strings.Contains(out, "restored-x") || s.badName != "bfr-abc" {
		t.Errorf("output %q name %q", out, s.badName)
	}
	s.statuses = nil
	if _, err := runRF(t, gw, []string{"--status", "nope"}); err == nil {
		t.Error("an unknown restore was not an error")
	}
}

func TestRestoreFiles_ForbiddenIsReportedPlainly(t *testing.T) {
	s, gw := newRestoreStub(t)
	s.denied = true
	_, err := runRF(t, gw, restoreArgs())
	if err == nil || !strings.Contains(err.Error(), "may not read the backups") {
		t.Fatalf("err %v", err)
	}
}

func TestRestoreFiles_PathsAreSentInTheirCanonicalForm(t *testing.T) {
	s, gw := newRestoreStub(t)
	args := []string{"--policy", "nightly", "--pvc", "apps/db", "--path", "a//b/", "--path", "/tab\there", "--to", "apps/x"}
	if _, err := runRF(t, gw, args); err != nil {
		t.Fatal(err)
	}
	if got := s.requests[0].Paths; got[0] != "/a/b" || got[1] != `/tab\x09here` {
		t.Errorf("paths %q", got)
	}
}
