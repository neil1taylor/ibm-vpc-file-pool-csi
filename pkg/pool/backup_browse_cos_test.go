package pool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const testBrowseNS = "browse-ns"

// browseCOSEnv is a browse environment with a policy that exports to COS, the
// browse namespace apart from the driver namespace, and the COS Secrets in the
// driver namespace.
type browseCOSEnv struct{ *browseEnv }

func newBrowseCOSEnv(t *testing.T) *browseCOSEnv {
	t.Helper()
	e := &browseCOSEnv{newBrowseEnv(t)}
	e.cfg.Namespace, e.cfg.COSNamespace = testBrowseNS, testDriverNS
	e.bc.SetBrowse(e.jobs, e.cfg)
	bp := e.bp("nightly")
	bp.Spec.Export = testBackupExport()
	if err := e.c.Update(e.ctx, bp); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cos-credentials", "cos-backup-key", "kp-apikey"} {
		e.putSecret(testDriverNS, name)
	}
	return e
}

func (e *browseCOSEnv) putSecret(ns, name string) {
	e.t.Helper()
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Data: map[string][]byte{"k": []byte("v")}}
	if err := e.c.Create(e.ctx, s); err != nil {
		e.t.Fatal(err)
	}
}

func (e *browseCOSEnv) secretIn(ns, name string) (*corev1.Secret, error) {
	s := &corev1.Secret{}
	err := e.c.Get(e.ctx, types.NamespacedName{Namespace: ns, Name: name}, s)
	return s, err
}

func (e *browseCOSEnv) openCOS(name string, mutate func(*v1alpha1.BackupBrowse)) {
	e.open(name, func(b *v1alpha1.BackupBrowse) {
		b.Spec.Source = v1alpha1.BackupSourceCOS
		if mutate != nil {
			mutate(b)
		}
	})
}

func TestBrowseCOS_StartsAHelperFromThePolicysExportBlock(t *testing.T) {
	e := newBrowseCOSEnv(t)
	at := metav1.NewTime(time.Date(2026, 10, 20, 9, 30, 0, 0, time.UTC))
	e.openCOS("bb1", func(b *v1alpha1.BackupBrowse) { b.Spec.Moment = &at })
	e.reconcile()

	bb := e.bb("bb1")
	if bb.Status.Phase != "Starting" || bb.Status.StartedTime == nil {
		t.Fatalf("status = %+v", bb.Status)
	}
	req, ok := e.jobs.started["uid-bb1"]
	if !ok || req.COS == nil {
		t.Fatalf("no COS helper was started: %+v", req)
	}
	repo := req.COS.Repo
	if repo.Bucket != "pool-backups" || repo.Prefix != "lab/nightly/" || repo.CredentialsSecret != "cos-credentials" ||
		repo.BackupKeySecret != "cos-backup-key" || repo.KeyProtect.APIKeySecret != "kp-apikey" {
		t.Errorf("repo = %+v", repo)
	}
	if req.COS.Policy != "nightly" || req.COS.Volume != "pvc-11111111-2222-3333-4444-555555555555" {
		t.Errorf("policy %q, volume %q", req.COS.Policy, req.COS.Volume)
	}
	if req.LatestPath != "" || req.MomentsPath != "" || req.Share.MountTargetIP != "" {
		t.Errorf("a COS session names a folder on a share: %+v", req)
	}
	if !req.At.Equal(at.Time) || req.Owner.UID != "uid-bb1" {
		t.Errorf("request = %+v", req)
	}
}

func TestBrowseCOS_TheKeysAreNeverCopiedWhereTheGatewayReads(t *testing.T) {
	e := newBrowseCOSEnv(t)
	e.openCOS("bb1", nil)
	e.reconcile()

	name := browseSecretName("uid-bb1")
	forGateway, err := e.secretIn(testBrowseNS, name)
	if err != nil {
		t.Fatal(err)
	}
	forHelper, err := e.secretIn(testDriverNS, name)
	if err != nil {
		t.Fatalf("the helper has no session Secret in the driver namespace: %v", err)
	}
	for _, k := range []string{"token", "tls.crt", "tls.key"} {
		if len(forHelper.Data[k]) == 0 || string(forHelper.Data[k]) != string(forGateway.Data[k]) {
			t.Errorf("the copy differs in %s", k)
		}
	}
	for _, s := range []*corev1.Secret{forGateway, forHelper} {
		if len(s.OwnerReferences) != 1 || s.OwnerReferences[0].UID != "uid-bb1" || s.Labels[browseLabel] != "uid-bb1" {
			t.Errorf("owner/labels of %s/%s = %+v / %v", s.Namespace, s.Name, s.OwnerReferences, s.Labels)
		}
	}
	// Nothing of the export's Secrets was copied into the browse namespace.
	all := &corev1.SecretList{}
	if err := e.c.List(e.ctx, all); err != nil {
		t.Fatal(err)
	}
	for _, s := range all.Items {
		if s.Namespace == testBrowseNS && s.Name != name {
			t.Errorf("an extra Secret %s in the browse namespace", s.Name)
		}
	}
	// A second pass keeps the same copy.
	e.reconcile()
	again, _ := e.secretIn(testDriverNS, name)
	if string(again.Data["token"]) != string(forGateway.Data["token"]) {
		t.Error("the token changed on a later pass")
	}
}

func TestBrowseCOS_EndingTheSessionRemovesBothSecrets(t *testing.T) {
	e := newBrowseCOSEnv(t)
	e.openCOS("bb1", nil)
	e.reconcile()
	bb := e.bb("bb1")
	bb.Spec.Close = true
	if err := e.c.Update(e.ctx, bb); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	name := browseSecretName("uid-bb1")
	if _, err := e.secretIn(testBrowseNS, name); err == nil {
		t.Error("the gateway's Secret is still there")
	}
	if _, err := e.secretIn(testDriverNS, name); err == nil {
		t.Error("the helper's Secret is still there")
	}
}

func TestBrowseCOS_OrphanedCopiesAreSwept(t *testing.T) {
	e := newBrowseCOSEnv(t)
	orphan := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testDriverNS, Name: "browse-uid-gone", Labels: map[string]string{browseLabel: "uid-gone"}}}
	other := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testDriverNS, Name: "cloud-api-key"}}
	for _, s := range []*corev1.Secret{orphan, other} {
		if err := e.c.Create(e.ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	e.reconcile()
	if _, err := e.secretIn(testDriverNS, "browse-uid-gone"); err == nil {
		t.Error("the orphaned copy was not removed")
	}
	if _, err := e.secretIn(testDriverNS, "cloud-api-key"); err != nil {
		t.Error("a Secret the controller did not make was removed")
	}
	if _, err := e.secretIn(testDriverNS, "cos-credentials"); err != nil {
		t.Error("an export Secret was removed")
	}
}

func TestBrowseCOS_APolicyWithoutAnExportBlockFailsInWords(t *testing.T) {
	e := newBrowseCOSEnv(t)
	bp := e.bp("nightly")
	bp.Spec.Export = nil
	if err := e.c.Update(e.ctx, bp); err != nil {
		t.Fatal(err)
	}
	e.openCOS("bb1", nil)
	e.reconcile()
	bb := e.bb("bb1")
	if bb.Status.Phase != "Failed" || !strings.Contains(bb.Status.Message, "has no export block") {
		t.Errorf("status = %+v", bb.Status)
	}
	if len(e.jobs.started) != 0 {
		t.Error("a helper was started")
	}
}

func TestBrowseCOS_AMissingSecretFailsInWordsAndStartsNothing(t *testing.T) {
	e := newBrowseCOSEnv(t)
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testDriverNS, Name: "kp-apikey"}}
	if err := e.c.Delete(e.ctx, s); err != nil {
		t.Fatal(err)
	}
	e.openCOS("bb1", nil)
	e.reconcile()
	bb := e.bb("bb1")
	if bb.Status.Phase != "Failed" || !strings.Contains(bb.Status.Message, `"kp-apikey"`) || !strings.Contains(bb.Status.Message, testDriverNS) {
		t.Errorf("status = %+v", bb.Status)
	}
	if len(e.jobs.started) != 0 {
		t.Error("a helper was started")
	}
	if _, err := e.secretIn(testDriverNS, browseSecretName("uid-bb1")); err == nil {
		t.Error("a session Secret was made for a session that cannot start")
	}
}

func TestBrowseCOS_AVolumeThePolicyDoesNotKnowFails(t *testing.T) {
	e := newBrowseCOSEnv(t)
	e.openCOS("bb1", func(b *v1alpha1.BackupBrowse) { b.Spec.PVCName = "nope" })
	e.reconcile()
	if bb := e.bb("bb1"); bb.Status.Phase != "Failed" || !strings.Contains(bb.Status.Message, "has no backup of apps/nope") {
		t.Errorf("status = %+v", bb.Status)
	}
}

func fromCOSBlock() *v1alpha1.BrowseFromCOS {
	return &v1alpha1.BrowseFromCOS{
		Bucket: "other-bucket", Endpoint: "s3.us-south.cloud-object-storage.appdomain.cloud", Prefix: "team-a",
		CredentialsSecret: "cos-credentials", BackupKeySecret: "cos-backup-key", KeyProtect: &v1alpha1.KeyProtectRef{
			InstanceID: "11111111-2222-3333-4444-555555555555", Region: "us-south", RootKeyID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", APIKeySecret: "kp-apikey"},
		Volume: "pvc-99999999-8888-7777-6666-555555555555",
	}
}

func TestBrowseCOS_ABucketNamedDirectlyNeedsNoPolicyInTheCluster(t *testing.T) {
	e := newBrowseCOSEnv(t)
	e.openCOS("bb1", func(b *v1alpha1.BackupBrowse) {
		b.Spec.Policy, b.Spec.PVCNamespace, b.Spec.PVCName = "lost-policy", v1alpha1.NoNamespace, "pvc-99999999-8888-7777-6666-555555555555"
		b.Spec.FromCOS = fromCOSBlock()
	})
	e.reconcile()
	req, ok := e.jobs.started["uid-bb1"]
	if !ok {
		t.Fatalf("no helper: %+v", e.bb("bb1").Status)
	}
	if req.COS.Repo.Bucket != "other-bucket" || req.COS.Repo.Prefix != "team-a/lost-policy/" || req.COS.Policy != "lost-policy" ||
		req.COS.Volume != "pvc-99999999-8888-7777-6666-555555555555" {
		t.Errorf("COS = %+v", req.COS)
	}
}

func TestBrowseCOS_ABucketNamedDirectlyIsCheckedHard(t *testing.T) {
	cases := map[string]func(*v1alpha1.BackupBrowse){
		"an endpoint that is not COS": func(b *v1alpha1.BackupBrowse) { b.Spec.FromCOS.Endpoint = "evil.example" },
		"a traversing prefix":         func(b *v1alpha1.BackupBrowse) { b.Spec.FromCOS.Prefix = "../x" },
		"a bad volume":                func(b *v1alpha1.BackupBrowse) { b.Spec.FromCOS.Volume = "../../etc" },
		"a bad Secret name":           func(b *v1alpha1.BackupBrowse) { b.Spec.FromCOS.CredentialsSecret = "a/b" },
		"a bad policy name":           func(b *v1alpha1.BackupBrowse) { b.Spec.Policy = "../x" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			e := newBrowseCOSEnv(t)
			e.openCOS("bb1", func(b *v1alpha1.BackupBrowse) {
				b.Spec.Policy, b.Spec.PVCNamespace, b.Spec.PVCName = "lost-policy", v1alpha1.NoNamespace, "pvc-99999999-8888-7777-6666-555555555555"
				b.Spec.FromCOS = fromCOSBlock()
				mutate(b)
			})
			e.reconcile()
			if bb := e.bb("bb1"); bb.Status.Phase != "Failed" {
				t.Errorf("status = %+v", bb.Status)
			}
			if len(e.jobs.started) != 0 {
				t.Error("a helper was started")
			}
		})
	}
}

func TestBrowseCOS_AHelperThatCannotStartShowsItsReason(t *testing.T) {
	e := newBrowseCOSEnv(t)
	e.openCOS("bb1", nil)
	e.reconcile()
	e.jobs.state["uid-bb1"] = BrowseJobState{Exists: true, Finished: true, Reason: "the backup key could not be unwrapped: Key Protect answered 403"}
	e.reconcile()
	bb := e.bb("bb1")
	if bb.Status.Phase != "Failed" || !strings.Contains(bb.Status.Message, "Key Protect answered 403") {
		t.Errorf("status = %+v", bb.Status)
	}
	// The same session with no reason says only that the pod failed.
	e2 := newBrowseCOSEnv(t)
	e2.openCOS("bb1", nil)
	e2.reconcile()
	e2.jobs.state["uid-bb1"] = BrowseJobState{Exists: true, Finished: true}
	e2.reconcile()
	if m := e2.bb("bb1").Status.Message; m != "the helper pod failed" {
		t.Errorf("message = %q", m)
	}
}

func TestBrowseCOS_AHelperThatNeverGetsReadyEndsTheSessionInWords(t *testing.T) {
	e := newBrowseCOSEnv(t)
	e.openCOS("bb1", nil)
	e.reconcile()
	e.now = e.now.Add(4 * time.Minute)
	e.reconcile()
	if bb := e.bb("bb1"); bb.Status.Phase != "Starting" {
		t.Fatalf("after 4 minutes: %+v", bb.Status)
	}
	e.now = e.now.Add(2 * time.Minute)
	e.reconcile()
	bb := e.bb("bb1")
	if bb.Status.Phase != "Failed" || !strings.Contains(bb.Status.Message, "not ready after 5m0s") || !strings.Contains(bb.Status.Message, "wrappedKey") {
		t.Errorf("status = %+v", bb.Status)
	}
	// A folder session has no such limit: it has no Secrets to wait for.
	e2 := newBrowseEnv(t)
	e2.open("bb1", nil)
	e2.reconcile()
	e2.now = e2.now.Add(30 * time.Minute)
	e2.reconcile()
	if bb := e2.bb("bb1"); bb.Status.Phase != "Starting" {
		t.Errorf("a folder session: %+v", bb.Status)
	}
}

func TestBrowseCOS_ReadyLikeAnyOtherSession(t *testing.T) {
	e := newBrowseCOSEnv(t)
	e.openCOS("bb1", nil)
	e.reconcile()
	e.jobs.state["uid-bb1"] = BrowseJobState{Exists: true, Ready: true, PodIP: "10.1.2.3"}
	e.reconcile()
	bb := e.bb("bb1")
	if bb.Status.Phase != "Ready" || bb.Status.PodIP != "10.1.2.3" || bb.Status.Port != browsePort {
		t.Errorf("status = %+v", bb.Status)
	}
	raw, _ := json.Marshal(bb)
	for _, secret := range []string{"cos-backup-key", "kp-apikey", "cos-credentials"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("the name of Secret %s is in the BackupBrowse status or spec", secret)
		}
	}
}

func TestBrowseCOS_NotTurnedOnSaysSo(t *testing.T) {
	e := newBrowseCOSEnv(t)
	e.cfg.COSNamespace = ""
	e.bc.SetBrowse(e.jobs, e.cfg)
	e.openCOS("bb1", nil)
	e.reconcile()
	if bb := e.bb("bb1"); bb.Status.Phase != "Failed" || !strings.Contains(bb.Status.Message, "not turned on") {
		t.Errorf("status = %+v", bb.Status)
	}
}

func TestBrowseCOS_ARunnerWithoutCOSSaysSo(t *testing.T) {
	e := newBrowseCOSEnv(t)
	e.bc.SetBrowse(&BrowseRouter{Folder: e.jobs}, e.cfg)
	e.openCOS("bb1", nil)
	e.reconcile()
	if bb := e.bb("bb1"); bb.Status.Phase != "Failed" || !strings.Contains(bb.Status.Message, "not turned on") {
		t.Errorf("status = %+v", bb.Status)
	}
}

// The router sends a COS session to the runner in the driver namespace, and
// a folder session to the one in the browse namespace.
func TestBrowseRouter(t *testing.T) {
	folder, cos := newFakeBrowseJobs(), newFakeBrowseJobs()
	r := &BrowseRouter{Folder: folder, COS: cos}
	ctx := context.Background()
	if err := r.StartBrowse(ctx, BrowseJobRequest{ID: "f1"}); err != nil {
		t.Fatal(err)
	}
	if err := r.StartBrowse(ctx, BrowseJobRequest{ID: "c1", COS: &BrowseCOSRequest{}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := folder.started["f1"]; !ok || len(folder.started) != 1 {
		t.Errorf("folder runner started %v", folder.started)
	}
	if _, ok := cos.started["c1"]; !ok || len(cos.started) != 1 {
		t.Errorf("COS runner started %v", cos.started)
	}
	for _, id := range []string{"f1", "c1"} {
		if st, err := r.BrowseState(ctx, id); err != nil || !st.Exists {
			t.Errorf("state of %s = %+v, %v", id, st, err)
		}
	}
	if st, _ := r.BrowseState(ctx, "unknown"); st.Exists {
		t.Error("an unknown session exists")
	}
	names, _ := r.BrowseSessions(ctx)
	if strings.Join(names, ",") != "c1,f1" {
		t.Errorf("sessions = %v", names)
	}
	if done, err := r.StopBrowse(ctx, "c1"); err != nil || !done {
		t.Errorf("stop = %v, %v", done, err)
	}
	if cos.stopped["c1"] != 1 || folder.stopped["c1"] != 1 {
		t.Errorf("stop calls: folder %v, cos %v (a session can be in either place, so both are asked)", folder.stopped, cos.stopped)
	}
	cos.stopDone = false
	if done, _ := r.StopBrowse(ctx, "c1"); done {
		t.Error("stop is done while the COS helper is still going")
	}
	if r.Namespace() != testDriverNS {
		t.Errorf("namespace = %q", r.Namespace())
	}
	if err := (&BrowseRouter{Folder: folder}).StartBrowse(ctx, BrowseJobRequest{ID: "c2", COS: &BrowseCOSRequest{}}); !errors.Is(err, errNoBrowseCOS) {
		t.Errorf("err = %v", err)
	}
}
