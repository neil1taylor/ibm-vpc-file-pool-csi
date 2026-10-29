package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// restoreEnv is an env where alice may read the backups of apps and make claims there.
func newRestoreEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t, Config{}, memTree{})
	e.authz.allow("alice", "apps", "create", "persistentvolumeclaims")
	e.authz.allow("alice", "apps", "update", "persistentvolumeclaims")
	e.authz.allow("alice", "apps", "create", "pods")
	return e
}

func newReq() map[string]any {
	return map[string]any{
		"policy": "nightly", "namespace": "apps", "pvc": "db",
		"paths":           []string{"/var/www/index.php", `/etc/my\x0aname`},
		"targetNamespace": "apps", "targetName": "db-files",
	}
}

func (e *env) restores() []v1alpha1.BackupFileRestore {
	e.t.Helper()
	l := &v1alpha1.BackupFileRestoreList{}
	if err := e.c.List(context.Background(), l); err != nil {
		e.t.Fatal(err)
	}
	return l.Items
}

func (e *env) claim(ns, name, uid string) {
	e.t.Helper()
	e.create(&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID(uid)}})
}

func TestRestoreIntoANewClaim(t *testing.T) {
	e := newRestoreEnv(t)
	moment := time.Date(2026, 10, 21, 9, 30, 0, 0, time.UTC)
	req := newReq()
	req["moment"] = moment
	req["sizeGB"] = 5
	req["storageClassName"] = "gold"
	resp, body := e.do("POST", "/v1/restores", "alice-token", req)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var got Restore
	if err := json.Unmarshal(body, &got); err != nil || got.Name == "" || got.Phase != "Pending" {
		t.Fatalf("answer %s: %v", body, err)
	}
	rs := e.restores()
	if len(rs) != 1 {
		t.Fatalf("restores %d", len(rs))
	}
	sp := rs[0].Spec
	if rs[0].Name != got.Name || sp.Policy != "nightly" || sp.SourceNamespace != "apps" || sp.SourceName != "db" ||
		sp.TargetNamespace != "apps" || sp.TargetName != "db-files" || sp.Existing || sp.SizeGB != 5 || sp.StorageClassName != "gold" ||
		sp.Moment == nil || !sp.Moment.Equal(&metav1.Time{Time: moment}) || sp.RequestedBy != "alice" || sp.TargetUID != "" {
		t.Errorf("spec %+v", sp)
	}
	if len(sp.Paths) != 2 || sp.Paths[0] != "/var/www/index.php" || sp.Paths[1] != `/etc/my\x0aname` {
		t.Errorf("paths %q", sp.Paths)
	}
	if rs[0].Labels[userLabel] != userHash("alice") {
		t.Errorf("labels %v", rs[0].Labels)
	}
}

func TestRestoreIntoAnExistingClaimPinsItsUID(t *testing.T) {
	e := newRestoreEnv(t)
	e.claim("apps", "work", "claim-uid-1")
	req := newReq()
	req["targetName"], req["existing"] = "work", true
	resp, body := e.do("POST", "/v1/restores", "alice-token", req)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	sp := e.restores()[0].Spec
	if !sp.Existing || sp.TargetUID != "claim-uid-1" {
		t.Errorf("spec %+v: the UID of the claim the rights were checked for must be pinned", sp)
	}
}

func TestRestoreRightsAreChecked(t *testing.T) {
	cases := []struct {
		name   string
		user   string
		mutate func(map[string]any)
		setup  func(*env)
		want   int
		msg    string
	}{
		{"no right to read the backups of the source", "bob", func(r map[string]any) {}, func(e *env) {
			e.authz.allow("bob", "apps", "create", "persistentvolumeclaims")
		}, 403, "may not read the backups"},
		{"the right to read a backup of another namespace is not enough", "alice", func(r map[string]any) {
			r["namespace"], r["pvc"] = "shop", "cart"
		}, func(e *env) {}, 403, "may not read the backups"},
		{"no right to create claims in the target", "alice", func(r map[string]any) { r["targetNamespace"] = "other" }, func(e *env) {}, 403, "claims in that namespace"},
		{"an existing claim needs the right to update it", "carol", func(r map[string]any) { r["existing"] = true; r["targetName"] = "work" }, func(e *env) {
			e.authz.set("carol", "apps", true)
			e.authz.allow("carol", "apps", "create", "persistentvolumeclaims")
			e.claim("apps", "work", "u1")
		}, 403, "claims in that namespace"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newRestoreEnv(t)
			e.g.authn = fakeAuthn{users: map[string]User{"alice-token": {Name: "alice"}, "bob-token": {Name: "bob"}, "carol-token": {Name: "carol"}}}
			c.setup(e)
			req := newReq()
			c.mutate(req)
			resp, body := e.do("POST", "/v1/restores", c.user+"-token", req)
			if resp.StatusCode != c.want || !strings.Contains(string(body), c.msg) {
				t.Fatalf("status %d: %s", resp.StatusCode, body)
			}
			if len(e.restores()) != 0 {
				t.Fatal("a restore was made for a user who may not")
			}
		})
	}
}

// Writing files into a claim is the same power as mounting it. A user who can
// edit the claim object but cannot start a pod must not write into its data, and
// the refusal is the same whether the claim exists or not.
func TestRestoreIntoAnExistingClaimNeedsTheRightToStartPods(t *testing.T) {
	e := newRestoreEnv(t)
	e.g.authn = fakeAuthn{users: map[string]User{"dave-token": {Name: "dave"}}}
	e.authz.set("dave", "apps", true)
	e.authz.allow("dave", "apps", "create", "persistentvolumeclaims")
	e.authz.allow("dave", "apps", "update", "persistentvolumeclaims")
	e.claim("apps", "work", "u1")
	var answers []string
	for _, target := range []string{"work", "not-there"} {
		req := newReq()
		req["existing"], req["targetName"] = true, target
		resp, body := e.do("POST", "/v1/restores", "dave-token", req)
		if resp.StatusCode != 403 || !strings.Contains(string(body), "start pods") {
			t.Fatalf("%s: %d %s", target, resp.StatusCode, body)
		}
		answers = append(answers, string(body))
	}
	if answers[0] != answers[1] {
		t.Errorf("the refusal tells whether the claim exists: %q and %q", answers[0], answers[1])
	}
	if len(e.restores()) != 0 {
		t.Fatal("a restore was made")
	}
	// A new claim needs no pod right: the user only makes a claim.
	req := newReq()
	if resp, body := e.do("POST", "/v1/restores", "dave-token", req); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("new claim: %d %s", resp.StatusCode, body)
	}
}

// A person without the right to read the source learns nothing about the volume
// or the target claim: the answer is the same whether they exist or not.
func TestRestoreForbiddenHidesWhatExists(t *testing.T) {
	e := newRestoreEnv(t)
	e.claim("apps", "work", "u1")
	var answers []string
	for _, pvc := range []string{"db", "nothing-here"} {
		req := newReq()
		req["namespace"], req["pvc"] = "shop", pvc
		req["existing"], req["targetName"] = true, "work"
		resp, body := e.do("POST", "/v1/restores", "alice-token", req)
		if resp.StatusCode != 403 {
			t.Fatalf("status %d", resp.StatusCode)
		}
		answers = append(answers, string(body))
	}
	if answers[0] != answers[1] {
		t.Errorf("the answers differ: %q and %q", answers[0], answers[1])
	}
	for _, asked := range e.authz.asked {
		if strings.Contains(asked, "persistentvolumeclaims") {
			t.Errorf("the target was looked at before the source right was known: %v", e.authz.asked)
		}
	}
}

func TestRestoreRightsFailureIsNeverAYes(t *testing.T) {
	e := newRestoreEnv(t)
	e.authz.err = true
	resp, _ := e.do("POST", "/v1/restores", "alice-token", newReq())
	if resp.StatusCode != 403 || len(e.restores()) != 0 {
		t.Fatalf("status %d, restores %d", resp.StatusCode, len(e.restores()))
	}
}

func TestRestoreRefusals(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any)
		setup  func(*env)
		want   int
	}{
		{"no backup of that volume", func(r map[string]any) { r["pvc"] = "gone" }, func(e *env) {}, 404},
		{"a new claim that exists", func(r map[string]any) {}, func(e *env) { e.claim("apps", "db-files", "u9") }, 409},
		{"an existing claim that does not exist", func(r map[string]any) { r["existing"] = true }, func(e *env) {}, 404},
		{"no paths", func(r map[string]any) { r["paths"] = []string{} }, func(e *env) {}, 400},
		{"a path with ..", func(r map[string]any) { r["paths"] = []string{"/a/../../etc"} }, func(e *env) {}, 400},
		{"a path with a bad escape", func(r map[string]any) { r["paths"] = []string{`/a\q`} }, func(e *env) {}, 400},
		{"too many paths", func(r map[string]any) {
			p := make([]string, 1001)
			for i := range p {
				p[i] = "/a"
			}
			r["paths"] = p
		}, func(e *env) {}, 400},
		{"no target", func(r map[string]any) { delete(r, "targetName") }, func(e *env) {}, 400},
		{"a target that is not a name", func(r map[string]any) { r["targetName"] = "Bad Name" }, func(e *env) {}, 400},
		{"a size with an existing claim", func(r map[string]any) { r["existing"], r["sizeGB"] = true, 5 }, func(e *env) { e.claim("apps", "db-files", "u1") }, 400},
		{"a negative size", func(r map[string]any) { r["sizeGB"] = -1 }, func(e *env) {}, 400},
		{"an unknown field", func(r map[string]any) { r["targetUID"] = "someone-elses" }, func(e *env) {}, 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newRestoreEnv(t)
			c.setup(e)
			req := newReq()
			c.mutate(req)
			resp, body := e.do("POST", "/v1/restores", "alice-token", req)
			if resp.StatusCode != c.want {
				t.Fatalf("status %d: %s", resp.StatusCode, body)
			}
			if len(e.restores()) != 0 {
				t.Fatal("a restore was made")
			}
		})
	}
}

// One user cannot start unlimited restores: each one is a Job that mounts shares.
func TestRestoreOpenRestoresPerUserAreLimited(t *testing.T) {
	e := newRestoreEnv(t)
	e.g.cfg.MaxOpenRestores = 3
	for i := 0; i < 3; i++ {
		req := newReq()
		req["targetName"] = "c" + string(rune('a'+i))
		if resp, body := e.do("POST", "/v1/restores", "alice-token", req); resp.StatusCode != http.StatusAccepted {
			t.Fatalf("restore %d: %d %s", i, resp.StatusCode, body)
		}
	}
	req := newReq()
	req["targetName"] = "cx"
	resp, body := e.do("POST", "/v1/restores", "alice-token", req)
	if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(string(body), "3") {
		t.Fatalf("the fourth: %d %s", resp.StatusCode, body)
	}
	// Another user is not held back by alice's restores.
	e.authz.allow("bob", "shop", "create", "persistentvolumeclaims")
	e.claim("shop", "x", "bobs-uid")
	// A restore that has ended no longer counts.
	for _, fr := range e.restores() {
		fr.Status.Phase = "Done"
		if err := e.c.Status().Update(context.Background(), &fr); err != nil {
			t.Fatal(err)
		}
	}
	if resp, body := e.do("POST", "/v1/restores", "alice-token", req); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("after the restores ended: %d %s", resp.StatusCode, body)
	}
}

// The cap holds when requests arrive at the same time (or at two gateway
// replicas): a restore takes one of a user's numbered slots, and a slot that is
// taken cannot be created again.
func TestRestoreCapHoldsForParallelRequests(t *testing.T) {
	e := newRestoreEnv(t)
	e.g.cfg.MaxOpenRestores = 3
	var wg sync.WaitGroup
	var accepted, refused atomic.Int32
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := newReq()
			req["targetName"] = "p" + string(rune('a'+i))
			resp, _ := e.do("POST", "/v1/restores", "alice-token", req)
			switch resp.StatusCode {
			case http.StatusAccepted:
				accepted.Add(1)
			case http.StatusTooManyRequests:
				refused.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if accepted.Load() != 3 || refused.Load() != 7 || len(e.restores()) != 3 {
		t.Fatalf("accepted %d refused %d made %d", accepted.Load(), refused.Load(), len(e.restores()))
	}
}

func TestRestoreStatusIsForTheOwnerOnly(t *testing.T) {
	e := newRestoreEnv(t)
	_, body := e.do("POST", "/v1/restores", "alice-token", newReq())
	var made Restore
	_ = json.Unmarshal(body, &made)

	// The controller ends it.
	fr := e.restores()[0]
	fr.Status = v1alpha1.BackupFileRestoreStatus{Phase: "Done", Message: "restored 2 files", Folder: "restored-nightly-x", Files: 2, Bytes: 10, Skipped: 1, Unreadable: 3, SizeGB: 1}
	if err := e.c.Status().Update(context.Background(), &fr); err != nil {
		t.Fatal(err)
	}
	resp, body := e.do("GET", "/v1/restores/"+made.Name, "alice-token", nil)
	var got Restore
	if resp.StatusCode != 200 || json.Unmarshal(body, &got) != nil || got.Phase != "Done" || got.Folder != "restored-nightly-x" || got.Files != 2 || got.Unreadable != 3 || got.Skipped != 1 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	// Another user, even one with the same rights, sees no such restore.
	e.authz.set("bob", "apps", true)
	resp, body = e.do("GET", "/v1/restores/"+made.Name, "bob-token", nil)
	if resp.StatusCode != 404 {
		t.Errorf("another user: %d %s", resp.StatusCode, body)
	}
	resp, _ = e.do("GET", "/v1/restores/no-such", "alice-token", nil)
	if resp.StatusCode != 404 {
		t.Errorf("no such restore: %d", resp.StatusCode)
	}
	// The right is checked again on every call.
	e.authz.set("alice", "apps", false)
	resp, _ = e.do("GET", "/v1/restores/"+made.Name, "alice-token", nil)
	if resp.StatusCode != 403 {
		t.Errorf("after the right was removed: %d", resp.StatusCode)
	}
}

func TestRestoreAuditEvent(t *testing.T) {
	e := newRestoreEnv(t)
	req := newReq()
	req["paths"] = []string{"/var/www/index.php", "/etc/tab\there", `/etc/my\x0aname`}
	_, body := e.do("POST", "/v1/restores", "alice-token", req)
	var made Restore
	_ = json.Unmarshal(body, &made)
	var found *corev1.Event
	for _, ev := range e.events() {
		if ev.Reason == "FileRestoreRequested" {
			found = &ev
		}
	}
	if found == nil {
		t.Fatalf("no audit Event: %v", e.events())
	}
	m := found.Message
	for _, want := range []string{"alice", "apps/db", "nightly", "apps/db-files", "/var/www/index.php", `/etc/my\x0aname`, `/etc/tab\x09here`} {
		if !strings.Contains(m, want) {
			t.Errorf("message lacks %q: %q", want, m)
		}
	}
	if strings.ContainsAny(m, "\n\t\x00") || strings.Contains(m, "alice-token") {
		t.Errorf("message holds a control character or a token: %q", m)
	}
	if found.Related == nil || found.Related.Kind != "BackupFileRestore" || found.Related.Name != made.Name {
		t.Errorf("involved %+v", found.InvolvedObject)
	}
	if found.Namespace != driverNS {
		t.Errorf("namespace %q", found.Namespace)
	}
	// The stored paths are the canonical form too.
	if got := e.restores()[0].Spec.Paths[1]; got != `/etc/tab\x09here` {
		t.Errorf("stored path %q", got)
	}
}

func TestRestoreCallsNeedAToken(t *testing.T) {
	e := newRestoreEnv(t)
	for _, call := range []struct{ method, path string }{{"POST", "/v1/restores"}, {"GET", "/v1/restores/x"}} {
		for _, token := range []string{"", "wrong"} {
			resp, _ := e.do(call.method, call.path, token, newReq())
			if resp.StatusCode != 401 {
				t.Errorf("%s %s with token %q: %d", call.method, call.path, token, resp.StatusCode)
			}
		}
	}
}

var _ client.Client // the env uses a fake client
