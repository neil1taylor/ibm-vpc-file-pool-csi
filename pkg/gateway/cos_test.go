package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// The policy "nightly" of newEnv has apps/db with 3 restore points, the newest
// on 22 Oct 10:00. cosPolicy gives it an export block and says what was exported
// and how far back the local backups go.
func (e *env) cosPolicy(block bool, exported *time.Time, oldest *time.Time, lastBackup bool) {
	e.t.Helper()
	pol := &v1alpha1.BackupPolicy{}
	if err := e.c.Get(context.Background(), types.NamespacedName{Name: "nightly"}, pol); err != nil {
		e.t.Fatal(err)
	}
	if block {
		pol.Spec.Export = &v1alpha1.BackupExport{Bucket: "bkt", Endpoint: "s3.eu-de.cloud-object-storage.appdomain.cloud",
			CredentialsSecret: "cos-credentials", BackupKeySecret: "cos-backup-key",
			KeyProtect: v1alpha1.KeyProtectRef{InstanceID: "i1", Region: "eu-de", RootKeyID: "r1", APIKeySecret: "kp-apikey"}}
		if err := e.c.Update(context.Background(), pol); err != nil {
			e.t.Fatal(err)
		}
	}
	for i := range pol.Status.Volumes {
		v := &pol.Status.Volumes[i]
		if v.PVCName != "db" {
			continue
		}
		v.ExportedTime, v.OldestBackupTime = nil, nil
		if exported != nil {
			m := metav1.NewTime(*exported)
			v.ExportedTime = &m
		}
		if oldest != nil {
			m := metav1.NewTime(*oldest)
			v.OldestBackupTime = &m
		}
		if !lastBackup {
			v.LastBackupTime = nil
		}
	}
	if err := e.c.Status().Update(context.Background(), pol); err != nil {
		e.t.Fatal(err)
	}
}

func tm(day, hour int) *time.Time {
	t := time.Date(2026, 10, day, hour, 0, 0, 0, time.UTC)
	return &t
}

func (e *env) openSession(req SessionRequest, token string) (int, Session, ErrorBody) {
	e.t.Helper()
	resp, body := e.do("POST", "/v1/sessions", token, req)
	var s Session
	var eb ErrorBody
	_ = json.Unmarshal(body, &s)
	_ = json.Unmarshal(body, &eb)
	return resp.StatusCode, s, eb
}

func TestSessionSourceIsChosenByWhereTheMomentIs(t *testing.T) {
	cases := []struct {
		name       string
		block      bool
		exported   *time.Time
		oldest     *time.Time
		lastBackup bool
		moment     *time.Time
		asked      string
		wantStatus int
		wantSource string // what the session spec holds
	}{
		{"a moment still held locally stays local", true, tm(22, 10), tm(15, 10), true, tm(20, 9), "", 202, ""},
		{"the latest copy stays local", true, tm(22, 10), tm(15, 10), true, nil, "", 202, ""},
		{"a moment older than every local restore point is read from COS", true, tm(22, 10), tm(15, 10), true, tm(10, 9), "", 202, "cos"},
		{"no local backup left but an export", true, tm(22, 10), nil, false, nil, "", 202, "cos"},
		{"an old moment of a volume that was never exported stays local", true, nil, tm(15, 10), true, tm(10, 9), "", 202, ""},
		{"a policy without an export block stays local", false, nil, tm(15, 10), true, tm(10, 9), "", 202, ""},
		{"COS can be asked for", true, tm(22, 10), tm(15, 10), true, tm(20, 9), "cos", 202, "cos"},
		{"local can be asked for", true, tm(22, 10), tm(15, 10), true, tm(10, 9), "local", 202, ""},
		{"COS asked for, no export block", false, nil, nil, true, nil, "cos", 409, ""},
		{"COS asked for, nothing exported yet", true, nil, nil, true, nil, "cos", 409, ""},
		{"a source that is not one", true, tm(22, 10), nil, true, nil, "tape", 400, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, Config{}, memTree{})
			e.cosPolicy(c.block, c.exported, c.oldest, c.lastBackup)
			status, s, eb := e.openSession(SessionRequest{Policy: "nightly", Namespace: "apps", PVC: "db", Moment: c.moment, Source: c.asked}, "alice-token")
			if status != c.wantStatus {
				t.Fatalf("status %d (%s)", status, eb.Message)
			}
			if c.wantStatus != http.StatusAccepted {
				if n := len(e.sessions()); n != 0 {
					t.Errorf("%d sessions were made", n)
				}
				return
			}
			bb := e.sessions()[0]
			if bb.Spec.Source != c.wantSource || bb.Spec.FromCOS != nil {
				t.Errorf("spec source = %q (%+v)", bb.Spec.Source, bb.Spec.FromCOS)
			}
			wantReply := "local"
			if c.wantSource == "cos" {
				wantReply = "cos"
			}
			if s.Source != wantReply {
				t.Errorf("the reply says source %q, want %q", s.Source, wantReply)
			}
		})
	}
}

func TestACOSSessionIsNotAnswerForALocalOneAndTheOtherWayRound(t *testing.T) {
	e := newEnv(t, Config{}, memTree{})
	e.cosPolicy(true, tm(22, 10), tm(15, 10), true)
	req := SessionRequest{Policy: "nightly", Namespace: "apps", PVC: "db", Moment: tm(20, 9)}
	_, local, _ := e.openSession(req, "alice-token")
	req.Source = "cos"
	_, cos, _ := e.openSession(req, "alice-token")
	if local.Name == cos.Name || len(e.sessions()) != 2 {
		t.Fatalf("a session was reused across sources: %s and %s", local.Name, cos.Name)
	}
	_, again, _ := e.openSession(req, "alice-token")
	if again.Name != cos.Name {
		t.Error("the same COS request did not reuse its session")
	}
	req.Source = "local"
	_, localAgain, _ := e.openSession(req, "alice-token")
	if localAgain.Name != local.Name {
		t.Error("the same local request did not reuse its session")
	}
}

func TestVolumesShowWhatIsInCOS(t *testing.T) {
	e := newEnv(t, Config{}, memTree{})
	e.cosPolicy(true, tm(22, 9), tm(15, 10), true)
	resp, body := e.do("GET", "/v1/volumes?namespace=apps", "alice-token", nil)
	var vols []Volume
	if resp.StatusCode != 200 || json.Unmarshal(body, &vols) != nil || len(vols) != 1 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if !vols[0].Exported || vols[0].ExportedTime == nil || !vols[0].ExportedTime.Equal(*tm(22, 9)) {
		t.Errorf("volume = %+v", vols[0])
	}
	// A policy that dropped its export block no longer offers COS.
	pol := &v1alpha1.BackupPolicy{}
	_ = e.c.Get(context.Background(), types.NamespacedName{Name: "nightly"}, pol)
	pol.Spec.Export = nil
	if err := e.c.Update(context.Background(), pol); err != nil {
		t.Fatal(err)
	}
	_, body = e.do("GET", "/v1/volumes?namespace=apps", "alice-token", nil)
	var after []Volume
	_ = json.Unmarshal(body, &after)
	if len(after) != 1 || after[0].Exported {
		t.Error("a volume is shown as exported though the policy has no export block")
	}
}

func TestAUserWithoutRightsCannotReachCOSEither(t *testing.T) {
	e := newEnv(t, Config{}, memTree{})
	e.cosPolicy(true, tm(22, 10), tm(15, 10), true)
	r1, _ := e.do("POST", "/v1/sessions", "alice-token", SessionRequest{Policy: "nightly", Namespace: "shop", PVC: "cart", Source: "cos"})
	r2, _ := e.do("POST", "/v1/sessions", "alice-token", SessionRequest{Policy: "nightly", Namespace: "ghost", PVC: "x", Source: "cos"})
	if r1.StatusCode != 403 || r2.StatusCode != 403 {
		t.Errorf("status %d and %d", r1.StatusCode, r2.StatusCode)
	}
}

func bucket() map[string]any {
	return map[string]any{
		"bucket": "other-bucket", "endpoint": "s3.us-south.cloud-object-storage.appdomain.cloud", "prefix": "team-a",
		"credentialsSecret": "cos-credentials", "backupKeySecret": "cos-backup-key",
		"keyProtect": map[string]any{"instanceID": "i1", "region": "us-south", "rootKeyID": "r1", "apiKeySecret": "kp-apikey"},
		"volume":     "pvc-99999999-8888-7777-6666-555555555555",
	}
}

func (e *env) makeAdmin(user string) {
	e.t.Helper()
	for _, s := range []string{"cos-credentials", "cos-backup-key", "kp-apikey"} {
		e.authz.allowSecret(user, driverNS, s, true)
	}
}

func TestANamedBucketNeedsTheRightToReadItsSecrets(t *testing.T) {
	e := newEnv(t, Config{}, memTree{})
	req := map[string]any{"policy": "lost-policy", "fromCOS": bucket()}

	// alice may read backups of her namespace; that is not enough for a bucket.
	if resp, body := e.do("POST", "/v1/sessions", "alice-token", req); resp.StatusCode != 403 || !strings.Contains(string(body), "Secrets") {
		t.Fatalf("alice: %d %s", resp.StatusCode, body)
	}
	// One Secret short is still no.
	e.authz.allowSecret("alice", driverNS, "cos-credentials", true)
	e.authz.allowSecret("alice", driverNS, "cos-backup-key", true)
	if resp, _ := e.do("POST", "/v1/sessions", "alice-token", req); resp.StatusCode != 403 {
		t.Errorf("two of three Secrets: %d", resp.StatusCode)
	}
	if n := len(e.sessions()); n != 0 {
		t.Fatalf("%d sessions were made for a refused request", n)
	}
	// The right is asked for each Secret by its name, in the driver namespace.
	asked := strings.Join(e.authz.asked, "\n")
	for _, want := range []string{"|get||secrets|cos-credentials", "|get||secrets|cos-backup-key", "|" + driverNS + "|get||secrets|"} {
		if !strings.Contains(asked, want) {
			t.Errorf("the rights asked for lack %q:\n%s", want, asked)
		}
	}
	e.authz.allowSecret("alice", driverNS, "kp-apikey", true)
	resp, body := e.do("POST", "/v1/sessions", "alice-token", req)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("alice with the Secrets: %d %s", resp.StatusCode, body)
	}
	bb := e.sessions()[0]
	if bb.Spec.Source != "cos" || bb.Spec.FromCOS == nil || bb.Spec.FromCOS.Bucket != "other-bucket" || bb.Spec.Policy != "lost-policy" ||
		bb.Spec.PVCNamespace != v1alpha1.NoNamespace || bb.Spec.PVCName != "pvc-99999999-8888-7777-6666-555555555555" {
		t.Errorf("spec = %+v", bb.Spec)
	}
	var s Session
	_ = json.Unmarshal(body, &s)
	if s.Source != "cos" {
		t.Errorf("reply source = %q", s.Source)
	}
	// The same request reuses the session; a different bucket does not.
	_, again := e.do("POST", "/v1/sessions", "alice-token", req)
	var s2 Session
	_ = json.Unmarshal(again, &s2)
	if s2.Name != s.Name {
		t.Error("the same request did not reuse its session")
	}
	other := bucket()
	other["bucket"] = "third-bucket"
	_, third := e.do("POST", "/v1/sessions", "alice-token", map[string]any{"policy": "lost-policy", "fromCOS": other})
	var s3 Session
	_ = json.Unmarshal(third, &s3)
	if s3.Name == s.Name || len(e.sessions()) != 2 {
		t.Error("a request for another bucket reused the session")
	}
	// The event says where the data came from, and holds no key.
	var found bool
	for _, ev := range e.events() {
		if ev.Reason == "SessionStarted" && strings.Contains(ev.Message, "COS bucket other-bucket") && strings.Contains(ev.Message, "alice") {
			found = true
		}
		for _, secret := range []string{"cos-credentials", "cos-backup-key", "kp-apikey"} {
			if strings.Contains(ev.Message, secret) {
				t.Errorf("event holds the name of a Secret: %q", ev.Message)
			}
		}
	}
	if !found {
		t.Errorf("events = %+v", e.events())
	}
}

func TestANamedBucketIsCheckedBeforeAnyRightIsAsked(t *testing.T) {
	cases := map[string]func(map[string]any, map[string]any){
		"an endpoint that is not COS": func(_, b map[string]any) { b["endpoint"] = "evil.example" },
		"a plain http endpoint":       func(_, b map[string]any) { b["endpoint"] = "http://s3.us-south.cloud-object-storage.appdomain.cloud" },
		"a traversing prefix":         func(_, b map[string]any) { b["prefix"] = "../x" },
		"a volume that is a path":     func(_, b map[string]any) { b["volume"] = "../../etc" },
		"a Secret name with a slash":  func(_, b map[string]any) { b["credentialsSecret"] = "a/b" },
		"a claim given too":           func(r, _ map[string]any) { r["namespace"], r["pvc"] = "apps", "db" },
		"a source that is local":      func(r, _ map[string]any) { r["source"] = "local" },
		"no policy":                   func(r, _ map[string]any) { r["policy"] = "" },
		"a policy that is a path":     func(r, _ map[string]any) { r["policy"] = "../x" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, Config{}, memTree{})
			e.makeAdmin("alice")
			b := bucket()
			req := map[string]any{"policy": "lost-policy", "fromCOS": b}
			mutate(req, b)
			resp, body := e.do("POST", "/v1/sessions", "alice-token", req)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("%d %s", resp.StatusCode, body)
			}
			if n := len(e.sessions()); n != 0 {
				t.Errorf("%d sessions were made", n)
			}
		})
	}
}

func TestANamedBucketSessionKeepsAskingForTheRightOnEveryCall(t *testing.T) {
	e := newEnv(t, Config{}, memTree{})
	e.makeAdmin("alice")
	resp, body := e.do("POST", "/v1/sessions", "alice-token", map[string]any{"policy": "lost-policy", "fromCOS": bucket()})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	var s Session
	_ = json.Unmarshal(body, &s)
	if resp, _ := e.do("GET", "/v1/sessions/"+s.Name, "alice-token", nil); resp.StatusCode != 200 {
		t.Errorf("status with the right: %d", resp.StatusCode)
	}
	// The right is taken away.
	e.authz.allowSecret("alice", driverNS, "kp-apikey", false)
	if resp, _ := e.do("GET", "/v1/sessions/"+s.Name, "alice-token", nil); resp.StatusCode != 403 {
		t.Errorf("status after the right was removed: %d", resp.StatusCode)
	}
	if resp, _ := e.do("POST", "/v1/sessions/"+s.Name+"/list", "alice-token", map[string]any{"path": []string{}}); resp.StatusCode != 403 {
		t.Errorf("list after the right was removed: %d", resp.StatusCode)
	}
	// The right to read the backups of the namespace "-" is not the right.
	e.authz.set("alice", v1alpha1.NoNamespace, true)
	if resp, _ := e.do("GET", "/v1/sessions/"+s.Name, "alice-token", nil); resp.StatusCode != 403 {
		t.Errorf("a right to read backups of the namespace %q must not stand in for the Secrets: %d", v1alpha1.NoNamespace, resp.StatusCode)
	}
}

func TestRestoreSourceFollowsTheSameRules(t *testing.T) {
	e := newRestoreEnv(t)
	e.cosPolicy(true, tm(22, 10), tm(15, 10), true)
	req := newReq()
	req["moment"] = tm(10, 9)
	resp, body := e.do("POST", "/v1/restores", "alice-token", req)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	var r Restore
	_ = json.Unmarshal(body, &r)
	rs := e.restores()
	if len(rs) != 1 || rs[0].Spec.Source != "cos" || rs[0].Spec.FromCOS != nil || r.Source != "cos" {
		t.Errorf("spec = %+v, reply source = %q", rs[0].Spec, r.Source)
	}
	req2 := newReq()
	req2["targetName"] = "db-files-2"
	req2["moment"] = tm(20, 9)
	if resp, _ := e.do("POST", "/v1/restores", "alice-token", req2); resp.StatusCode != http.StatusAccepted {
		t.Fatal(resp.StatusCode)
	}
	for _, fr := range e.restores() {
		if fr.Spec.TargetName == "db-files-2" && fr.Spec.Source != "" {
			t.Errorf("a moment held locally was read from COS: %+v", fr.Spec)
		}
	}
	req3 := newReq()
	req3["targetName"], req3["source"] = "db-files-3", "cos"
	e.cosPolicy(false, nil, tm(15, 10), true)
	pol := &v1alpha1.BackupPolicy{}
	_ = e.c.Get(context.Background(), types.NamespacedName{Name: "nightly"}, pol)
	pol.Spec.Export = nil
	_ = e.c.Update(context.Background(), pol)
	if resp, _ := e.do("POST", "/v1/restores", "alice-token", req3); resp.StatusCode != 409 {
		t.Errorf("COS asked for from a policy without export: %d", resp.StatusCode)
	}
}

func TestRestoreFromANamedBucket(t *testing.T) {
	e := newRestoreEnv(t)
	req := map[string]any{
		"policy": "lost-policy", "fromCOS": bucket(), "paths": []string{"/etc/app.conf"},
		"targetNamespace": "apps", "targetName": "back", "storageClassName": "gold", "sizeGB": 3,
	}
	// Alice may read backups of apps and make claims there, but cannot read the Secrets.
	if resp, _ := e.do("POST", "/v1/restores", "alice-token", req); resp.StatusCode != 403 {
		t.Fatalf("without the Secrets: %d", resp.StatusCode)
	}
	e.makeAdmin("alice")
	resp, body := e.do("POST", "/v1/restores", "alice-token", req)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	rs := e.restores()
	sp := rs[0].Spec
	if len(rs) != 1 || sp.Source != "cos" || sp.FromCOS == nil || sp.FromCOS.Volume != "pvc-99999999-8888-7777-6666-555555555555" ||
		sp.Policy != "lost-policy" || sp.SourceNamespace != v1alpha1.NoNamespace || sp.SourceName != "pvc-99999999-8888-7777-6666-555555555555" ||
		sp.StorageClassName != "gold" || sp.TargetNamespace != "apps" {
		t.Errorf("spec = %+v", sp)
	}
	// The right to write into the target is still needed.
	bad := map[string]any{}
	for k, v := range req {
		bad[k] = v
	}
	bad["targetNamespace"], bad["targetName"] = "other", "x"
	if resp, _ := e.do("POST", "/v1/restores", "alice-token", bad); resp.StatusCode != 403 {
		t.Errorf("a named bucket does not give the right to write elsewhere: %d", resp.StatusCode)
	}
	// A new claim needs a class, because COS does not keep it.
	noClass := map[string]any{}
	for k, v := range req {
		noClass[k] = v
	}
	noClass["targetName"] = "back2"
	delete(noClass, "storageClassName")
	if r, b := e.do("POST", "/v1/restores", "alice-token", noClass); r.StatusCode != 400 || !strings.Contains(string(b), "storageClassName") {
		t.Errorf("no class: %d %s", r.StatusCode, b)
	}
	// The status is shown only while the right holds.
	var r Restore
	_ = json.Unmarshal(body, &r)
	if resp, _ := e.do("GET", "/v1/restores/"+r.Name, "alice-token", nil); resp.StatusCode != 200 {
		t.Errorf("status: %d", resp.StatusCode)
	}
	e.authz.allowSecret("alice", driverNS, "cos-credentials", false)
	if resp, _ := e.do("GET", "/v1/restores/"+r.Name, "alice-token", nil); resp.StatusCode != 403 {
		t.Errorf("status after the right was removed: %d", resp.StatusCode)
	}
}

func TestRestoreFromANamedBucketIsCheckedHard(t *testing.T) {
	cases := map[string]func(map[string]any, map[string]any){
		"an endpoint that is not COS": func(_, b map[string]any) { b["endpoint"] = "evil.example" },
		"a claim given too":           func(r, _ map[string]any) { r["namespace"], r["pvc"] = "apps", "db" },
		"a policy that is a path":     func(r, _ map[string]any) { r["policy"] = "../x" },
		"a volume that is a path":     func(_, b map[string]any) { b["volume"] = "../x" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			e := newRestoreEnv(t)
			e.makeAdmin("alice")
			b := bucket()
			req := map[string]any{"policy": "lost-policy", "fromCOS": b, "paths": []string{"/a"},
				"targetNamespace": "apps", "targetName": "back", "storageClassName": "gold"}
			mutate(req, b)
			resp, body := e.do("POST", "/v1/restores", "alice-token", req)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("%d %s", resp.StatusCode, body)
			}
			if len(e.restores()) != 0 {
				t.Error("a restore was made")
			}
		})
	}
}
