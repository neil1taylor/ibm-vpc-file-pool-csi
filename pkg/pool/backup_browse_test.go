package pool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// fakeBrowseJobs records what the controller asks the runner to do.
type fakeBrowseJobs struct {
	started  map[string]BrowseJobRequest
	state    map[string]BrowseJobState
	stopped  map[string]int
	stopDone bool
	orphans  []string
}

func newFakeBrowseJobs() *fakeBrowseJobs {
	return &fakeBrowseJobs{started: map[string]BrowseJobRequest{}, state: map[string]BrowseJobState{}, stopped: map[string]int{}, stopDone: true}
}

func (f *fakeBrowseJobs) StartBrowse(_ context.Context, req BrowseJobRequest) error {
	f.started[req.ID] = req
	if _, ok := f.state[req.ID]; !ok {
		f.state[req.ID] = BrowseJobState{Exists: true}
	}
	return nil
}
func (f *fakeBrowseJobs) BrowseState(_ context.Context, name string) (BrowseJobState, error) {
	return f.state[name], nil
}
func (f *fakeBrowseJobs) StopBrowse(_ context.Context, name string) (bool, error) {
	f.stopped[name]++
	if f.stopDone {
		delete(f.state, name)
	}
	return f.stopDone, nil
}
func (f *fakeBrowseJobs) BrowseSessions(context.Context) ([]string, error) {
	names := append([]string(nil), f.orphans...)
	for n := range f.state {
		names = append(names, n)
	}
	return names, nil
}
func (f *fakeBrowseJobs) Namespace() string { return testDriverNS }

type browseEnv struct {
	*restoreEnv
	jobs *fakeBrowseJobs
	now  time.Time
	cfg  BrowseConfig
}

func newBrowseEnv(t *testing.T) *browseEnv {
	t.Helper()
	e := &browseEnv{restoreEnv: newRestoreEnv(t), jobs: newFakeBrowseJobs(), now: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)}
	e.cfg = BrowseConfig{Namespace: testDriverNS, Idle: 15 * time.Minute, MaxLife: 2 * time.Hour, MaxSessions: 5, MaxPerNamespace: 2, ClosedKeep: 10 * time.Minute}
	e.bc.SetBrowse(e.jobs, e.cfg)
	e.bc.SetNowFunc(func() time.Time { return e.now })
	return e
}

func (e *browseEnv) open(name string, mutate func(*v1alpha1.BackupBrowse)) {
	e.t.Helper()
	bb := &v1alpha1.BackupBrowse{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("uid-" + name), CreationTimestamp: metav1.NewTime(e.now)},
		Spec:       v1alpha1.BackupBrowseSpec{Policy: "nightly", PVCNamespace: "apps", PVCName: "db", RequestedBy: "alice"},
	}
	if mutate != nil {
		mutate(bb)
	}
	e.create(bb)
}

func (e *browseEnv) bb(name string) *v1alpha1.BackupBrowse {
	e.t.Helper()
	bb := &v1alpha1.BackupBrowse{}
	if err := e.c.Get(e.ctx, types.NamespacedName{Name: name}, bb); err != nil {
		e.t.Fatal(err)
	}
	return bb
}

func (e *browseEnv) secret(name string) (*corev1.Secret, error) {
	s := &corev1.Secret{}
	err := e.c.Get(e.ctx, types.NamespacedName{Namespace: testDriverNS, Name: name}, s)
	return s, err
}

// secretNameOf is the name of the Secret of the session with this UID.
func (e *browseEnv) secretNameOf(uid string) string { return browseSecretName(uid) }

func (e *browseEnv) setRestorePoints(n int32) {
	e.t.Helper()
	bp := e.bp("nightly")
	for i := range bp.Status.Volumes {
		bp.Status.Volumes[i].RestorePoints = n
	}
	if err := e.c.Status().Update(e.ctx, bp); err != nil {
		e.t.Fatal(err)
	}
}

func TestBrowse_StartsAHelperForALatestCopy(t *testing.T) {
	e := newBrowseEnv(t)
	e.open("bb1", nil)
	e.reconcile()

	bb := e.bb("bb1")
	if bb.Status.Phase != "Starting" || bb.Status.SecretName != "browse-uid-bb1" {
		t.Fatalf("status = %+v", bb.Status)
	}
	req, ok := e.jobs.started["uid-bb1"]
	if !ok {
		t.Fatal("no helper was started")
	}
	wantLatest := "/backup/nightly" + e.claim.Spec.SubPath
	if req.LatestPath != wantLatest {
		t.Errorf("LatestPath = %q, want %q", req.LatestPath, wantLatest)
	}
	if req.MomentsPath != "" || !req.At.IsZero() {
		t.Errorf("a volume with no earlier moments has no moments folder: %q %v", req.MomentsPath, req.At)
	}
	if req.Share.MountTargetIP != "10.240.9.9" || req.Share.ExportPath != "/export_backups" {
		t.Errorf("share = %+v", req.Share)
	}
	if req.Idle != 15*time.Minute || req.MaxLife != 2*time.Hour || req.SecretName != "browse-uid-bb1" {
		t.Errorf("request = %+v", req)
	}
	if req.Owner.Kind != "BackupBrowse" || req.Owner.UID != "uid-bb1" || req.Owner.Name != "bb1" {
		t.Errorf("owner = %+v", req.Owner)
	}
}

func TestBrowse_AMomentGetsTheMomentsFolder(t *testing.T) {
	e := newBrowseEnv(t)
	e.setRestorePoints(3)
	at := metav1.NewTime(time.Date(2026, 10, 21, 9, 30, 0, 0, time.UTC))
	e.open("bb1", func(b *v1alpha1.BackupBrowse) { b.Spec.Moment = &at })
	e.reconcile()
	req := e.jobs.started["uid-bb1"]
	if want := "/backup/nightly/.moments" + e.claim.Spec.SubPath; req.MomentsPath != want {
		t.Errorf("MomentsPath = %q, want %q", req.MomentsPath, want)
	}
	if !req.At.Equal(at.Time) {
		t.Errorf("At = %v", req.At)
	}
}

func TestBrowse_TheSessionSecretHoldsATokenAndACertificate(t *testing.T) {
	e := newBrowseEnv(t)
	e.open("bb1", nil)
	e.reconcile()
	s, err := e.secret("browse-uid-bb1")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Data["token"]) < 32 {
		t.Errorf("token is %d bytes", len(s.Data["token"]))
	}
	if _, err := browse.ServerTLSConfig(s.Data["tls.crt"], s.Data["tls.key"]); err != nil {
		t.Errorf("certificate and key do not pair: %v", err)
	}
	if _, err := browse.PinnedClient(s.Data["tls.crt"]); err != nil {
		t.Errorf("certificate: %v", err)
	}
	if len(s.OwnerReferences) != 1 || s.OwnerReferences[0].UID != "uid-bb1" || s.OwnerReferences[0].Kind != "BackupBrowse" {
		t.Errorf("owner = %+v", s.OwnerReferences)
	}
	if s.Labels[browseLabel] != "uid-bb1" {
		t.Errorf("labels = %v", s.Labels)
	}
	// Another session gets another token and another certificate.
	e.open("bb2", func(b *v1alpha1.BackupBrowse) { b.Spec.PVCNamespace = "apps" })
	e.reconcile()
	s2, _ := e.secret("browse-uid-bb2")
	if string(s2.Data["token"]) == string(s.Data["token"]) || string(s2.Data["tls.crt"]) == string(s.Data["tls.crt"]) {
		t.Error("two sessions share a secret")
	}
	// A second pass keeps the same secret.
	e.reconcile()
	again, _ := e.secret("browse-uid-bb1")
	if string(again.Data["token"]) != string(s.Data["token"]) {
		t.Error("the token changed on a later pass")
	}
	// The token is nowhere in the resource.
	raw, _ := json.Marshal(e.bb("bb1"))
	if strings.Contains(string(raw), string(s.Data["token"])) {
		t.Error("the token is in the BackupBrowse")
	}
}

func TestBrowse_ReadyWhenTheHelperAnswers(t *testing.T) {
	e := newBrowseEnv(t)
	e.open("bb1", nil)
	e.reconcile()
	e.jobs.state["uid-bb1"] = BrowseJobState{Exists: true, PodIP: "10.130.0.9"}
	e.reconcile()
	if bb := e.bb("bb1"); bb.Status.Phase != "Starting" || bb.Status.PodIP != "" {
		t.Errorf("a pod that is not ready: %+v", bb.Status)
	}
	e.jobs.state["uid-bb1"] = BrowseJobState{Exists: true, PodIP: "10.130.0.9", Ready: true}
	e.reconcile()
	bb := e.bb("bb1")
	if bb.Status.Phase != "Ready" || bb.Status.PodIP != "10.130.0.9" || bb.Status.Port != 8443 {
		t.Errorf("status = %+v", bb.Status)
	}
}

func TestBrowse_ClosesWhenTheHelperEndsByItself(t *testing.T) {
	e := newBrowseEnv(t)
	e.open("bb1", nil)
	e.reconcile()
	e.jobs.state["uid-bb1"] = BrowseJobState{Exists: true, Finished: true, Succeeded: true, Reason: "idle"}
	e.reconcile()
	bb := e.bb("bb1")
	if bb.Status.Phase != "Closed" || bb.Status.EndReason != "idle" || bb.Status.EndedTime == nil || bb.Status.PodIP != "" {
		t.Errorf("status = %+v", bb.Status)
	}
	if e.jobs.stopped["uid-bb1"] == 0 {
		t.Error("the finished Job was not removed")
	}
	if _, err := e.secret("browse-uid-bb1"); !apierrors.IsNotFound(err) {
		t.Errorf("the session Secret must be removed: %v", err)
	}
}

func TestBrowse_FailsWhenTheHelperFails(t *testing.T) {
	e := newBrowseEnv(t)
	e.open("bb1", nil)
	e.reconcile()
	e.jobs.state["uid-bb1"] = BrowseJobState{Exists: true, Finished: true}
	e.reconcile()
	bb := e.bb("bb1")
	if bb.Status.Phase != "Failed" || bb.Status.Message == "" || bb.Status.EndedTime == nil {
		t.Errorf("status = %+v", bb.Status)
	}
}

func TestBrowse_FailsWhenTheHelperIsGoneWhileReady(t *testing.T) {
	e := newBrowseEnv(t)
	e.open("bb1", nil)
	e.reconcile()
	e.jobs.state["uid-bb1"] = BrowseJobState{Exists: true, Ready: true, PodIP: "10.130.0.9"}
	e.reconcile()
	delete(e.jobs.state, "uid-bb1")
	e.reconcile()
	if bb := e.bb("bb1"); bb.Status.Phase != "Failed" {
		t.Errorf("status = %+v", bb.Status)
	}
}

func TestBrowse_BadRequestsFail(t *testing.T) {
	cases := map[string]func(*v1alpha1.BackupBrowse){
		"unknown policy": func(b *v1alpha1.BackupBrowse) { b.Spec.Policy = "nope" },
		"unknown claim":  func(b *v1alpha1.BackupBrowse) { b.Spec.PVCName = "nope" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			e := newBrowseEnv(t)
			e.open("bb1", mutate)
			e.reconcile()
			bb := e.bb("bb1")
			if bb.Status.Phase != "Failed" || bb.Status.Message == "" {
				t.Errorf("status = %+v", bb.Status)
			}
			if len(e.jobs.started) != 0 {
				t.Error("a helper was started for a bad request")
			}
			if _, err := e.secret("browse-uid-bb1"); !apierrors.IsNotFound(err) {
				t.Error("a Secret was made for a bad request")
			}
		})
	}
}

func TestBrowse_AMomentBeforeTheFirstBackupStillOpensTheLatestCopy(t *testing.T) {
	// With no earlier moments the only copy is the latest one, as in a restore.
	e := newBrowseEnv(t)
	at := metav1.NewTime(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	e.open("bb1", func(b *v1alpha1.BackupBrowse) { b.Spec.Moment = &at })
	e.reconcile()
	if req := e.jobs.started["uid-bb1"]; req.MomentsPath != "" {
		t.Errorf("request = %+v", req)
	}
}

func TestBrowse_WaitsWhenTheSnapshotPoolHasNoReadyShare(t *testing.T) {
	e := newBrowseEnv(t)
	pool := &v1alpha1.FileSharePool{}
	if err := e.c.Get(e.ctx, types.NamespacedName{Name: "backups"}, pool); err != nil {
		t.Fatal(err)
	}
	pool.Status.Shares[0].State = "failed"
	if err := e.c.Update(e.ctx, pool); err != nil {
		t.Fatal(err)
	}
	e.open("bb1", nil)
	e.reconcile()
	bb := e.bb("bb1")
	if bb.Status.Phase != "Pending" || !strings.Contains(bb.Status.Message, "ready") {
		t.Errorf("status = %+v", bb.Status)
	}
}

func TestBrowse_HardLimitClosesASession(t *testing.T) {
	e := newBrowseEnv(t)
	e.open("bb1", nil)
	e.reconcile()
	e.jobs.state["uid-bb1"] = BrowseJobState{Exists: true, Ready: true, PodIP: "10.130.0.9"}
	e.reconcile()
	e.now = e.now.Add(2*time.Hour + time.Minute)
	e.reconcile()
	bb := e.bb("bb1")
	if bb.Status.Phase != "Closed" || bb.Status.EndReason != "max-life" {
		t.Errorf("status = %+v", bb.Status)
	}
	if e.jobs.stopped["uid-bb1"] == 0 {
		t.Error("the helper was not stopped")
	}
}

func TestBrowse_AnOldSessionIsClosedAfterARestartBeforeItStarts(t *testing.T) {
	e := newBrowseEnv(t)
	e.open("old", func(b *v1alpha1.BackupBrowse) { b.CreationTimestamp = metav1.NewTime(e.now.Add(-3 * time.Hour)) })
	e.reconcile()
	if _, started := e.jobs.started["uid-old"]; started {
		t.Error("a session past its hard limit must not start")
	}
	if bb := e.bb("old"); bb.Status.Phase != "Closed" || bb.Status.EndReason != "max-life" {
		t.Errorf("status = %+v", bb.Status)
	}
}

func TestBrowse_CloseRequestEndsASession(t *testing.T) {
	e := newBrowseEnv(t)
	e.open("bb1", nil)
	e.reconcile()
	bb := e.bb("bb1")
	bb.Spec.Close = true
	if err := e.c.Update(e.ctx, bb); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	got := e.bb("bb1")
	if got.Status.Phase != "Closed" || got.Status.EndReason != "closed" {
		t.Errorf("status = %+v", got.Status)
	}
}

func TestBrowse_ClosingWaitsUntilTheHelperIsGone(t *testing.T) {
	e := newBrowseEnv(t)
	e.jobs.stopDone = false
	e.open("bb1", nil)
	e.reconcile()
	bb := e.bb("bb1")
	bb.Spec.Close = true
	_ = e.c.Update(e.ctx, bb)
	e.reconcile()
	if got := e.bb("bb1"); got.Status.Phase == "Closed" {
		t.Error("closed while the helper pod still runs")
	}
	e.jobs.stopDone = true
	e.reconcile()
	if got := e.bb("bb1"); got.Status.Phase != "Closed" {
		t.Errorf("status = %+v", got.Status)
	}
}

func TestBrowse_LimitOnOpenSessions(t *testing.T) {
	e := newBrowseEnv(t)
	e.cfg.MaxSessions, e.cfg.MaxPerNamespace = 2, 5
	e.bc.SetBrowse(e.jobs, e.cfg)
	for i, n := range []string{"a", "b", "c"} {
		n := n
		e.open(n, func(b *v1alpha1.BackupBrowse) {
			b.CreationTimestamp = metav1.NewTime(e.now.Add(time.Duration(i) * time.Second))
		})
	}
	e.reconcile()
	if len(e.jobs.started) != 2 || e.jobs.started["uid-c"].ID != "" {
		t.Fatalf("started %v, want a and b only (oldest first)", e.jobs.started)
	}
	c := e.bb("c")
	if c.Status.Phase != "Pending" || !strings.Contains(c.Status.Message, "2") {
		t.Errorf("c = %+v", c.Status)
	}
	// a closes; c may start.
	e.jobs.state["uid-a"] = BrowseJobState{Exists: true, Finished: true, Succeeded: true, Reason: "idle"}
	e.reconcile()
	e.reconcile()
	if _, ok := e.jobs.started["uid-c"]; !ok {
		t.Error("c did not start after a slot was freed")
	}
}

func TestBrowse_LimitPerNamespaceOfTheClaim(t *testing.T) {
	e := newBrowseEnv(t)
	e.cfg.MaxPerNamespace = 1
	e.bc.SetBrowse(e.jobs, e.cfg)
	// a second backed-up claim in another namespace
	other := e.addClaim("shop", "cart", "data-a", yes)
	other.Spec.SubPath = "/pvcs/pvc-66666666-7777-8888-9999-000000000000"
	_ = e.c.Update(e.ctx, other)
	e.reconcile()
	e.open("a", nil)
	e.open("b", nil)
	e.open("c", func(bb *v1alpha1.BackupBrowse) { bb.Spec.PVCNamespace, bb.Spec.PVCName = "shop", "cart" })
	e.reconcile()
	if _, ok := e.jobs.started["uid-a"]; !ok {
		t.Error("a should start")
	}
	if _, ok := e.jobs.started["uid-b"]; ok {
		t.Error("b is the second session of namespace apps and must wait")
	}
	if _, ok := e.jobs.started["uid-c"]; !ok {
		t.Error("c is in another namespace and must start")
	}
}

func TestBrowse_ClosedRecordsAreRemovedAfterAWhile(t *testing.T) {
	e := newBrowseEnv(t)
	e.open("bb1", nil)
	e.reconcile()
	e.jobs.state["uid-bb1"] = BrowseJobState{Exists: true, Finished: true, Succeeded: true, Reason: "idle"}
	e.reconcile()
	e.now = e.now.Add(5 * time.Minute)
	e.reconcile()
	_ = e.bb("bb1") // still there
	e.now = e.now.Add(6 * time.Minute)
	e.reconcile()
	if err := e.c.Get(e.ctx, types.NamespacedName{Name: "bb1"}, &v1alpha1.BackupBrowse{}); !apierrors.IsNotFound(err) {
		t.Errorf("the closed session is still there: %v", err)
	}
}

func TestBrowse_OrphansAreCleanedUp(t *testing.T) {
	e := newBrowseEnv(t)
	// a helper Job whose session is gone, and a Secret whose session is gone
	e.jobs.orphans = []string{"ghost"}
	e.jobs.state["uid-ghost"] = BrowseJobState{Exists: true}
	e.create(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "browse-ghost", Namespace: testDriverNS, Labels: map[string]string{browseLabel: "ghost"}}})
	e.open("live", nil)
	e.reconcile()
	if e.jobs.stopped["uid-ghost"] == 0 {
		t.Error("the helper of a vanished session was not stopped")
	}
	if _, err := e.secret("browse-uid-ghost"); !apierrors.IsNotFound(err) {
		t.Errorf("the Secret of a vanished session remains: %v", err)
	}
	if e.jobs.stopped["uid-live"] != 0 {
		t.Error("a live session was stopped")
	}
	if _, err := e.secret("browse-uid-live"); err != nil {
		t.Errorf("the live session lost its Secret: %v", err)
	}
}

func TestBrowse_DoesNothingUntilTurnedOn(t *testing.T) {
	e := newRestoreEnv(t)
	bb := &v1alpha1.BackupBrowse{ObjectMeta: metav1.ObjectMeta{Name: "bb1"}, Spec: v1alpha1.BackupBrowseSpec{Policy: "nightly", PVCNamespace: "apps", PVCName: "db"}}
	e.create(bb)
	e.reconcile()
	got := &v1alpha1.BackupBrowse{}
	_ = e.c.Get(e.ctx, types.NamespacedName{Name: "bb1"}, got)
	if got.Status.Phase != "" {
		t.Errorf("status = %+v", got.Status)
	}
}

// A BackupBrowse that is deleted and made again with the same name, for another
// claim, must never get the old session's Secret or helper.
func TestBrowse_AReusedNameNeverGetsTheOldSessionsSecretOrHelper(t *testing.T) {
	e := newBrowseEnv(t)
	other := e.addClaim("shop", "cart", "data-a", yes)
	other.Spec.SubPath = "/pvcs/pvc-66666666-7777-8888-9999-000000000000"
	_ = e.c.Update(e.ctx, other)
	e.reconcile()

	e.open("x", nil) // uid-x, the apps/db volume
	e.reconcile()
	oldSecret, err := e.secret(e.secretNameOf("uid-x"))
	if err != nil {
		t.Fatal(err)
	}
	oldToken := string(oldSecret.Data["token"])
	oldPath := e.jobs.started["uid-x"].LatestPath

	// The record is deleted before the controller tidies up, and a new one with
	// the same name is made for another namespace's volume.
	if derr := e.c.Delete(e.ctx, e.bb("x")); derr != nil {
		t.Fatal(derr)
	}
	e.open("x", func(b *v1alpha1.BackupBrowse) {
		b.UID = "uid-y"
		b.Spec.PVCNamespace, b.Spec.PVCName = "shop", "cart"
	})
	e.reconcile()

	req, ok := e.jobs.started["uid-y"]
	if !ok {
		t.Fatalf("the new session started no helper of its own: %v", e.jobs.started)
	}
	if req.LatestPath == oldPath || !strings.Contains(req.LatestPath, "66666666") {
		t.Errorf("the new helper reads %q; it must read the new volume, not %q", req.LatestPath, oldPath)
	}
	newSecret, err := e.secret(e.secretNameOf("uid-y"))
	if err != nil {
		t.Fatal(err)
	}
	if string(newSecret.Data["token"]) == oldToken || string(newSecret.Data["tls.crt"]) == string(oldSecret.Data["tls.crt"]) {
		t.Error("the new session reused the old token or certificate")
	}
	if req.SecretName == oldSecret.Name {
		t.Errorf("the new helper mounts the old Secret %s", req.SecretName)
	}
	if e.jobs.stopped["uid-x"] == 0 {
		t.Error("the old helper was not stopped")
	}
	if _, err := e.secret(oldSecret.Name); !apierrors.IsNotFound(err) {
		t.Errorf("the old Secret remains: %v", err)
	}
}
