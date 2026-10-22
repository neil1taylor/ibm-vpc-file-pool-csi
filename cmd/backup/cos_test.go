package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/pool"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const cosKeySentinel = "SENTINEL-READABLE-BACKUP-KEY"

func newCOSClient(objs ...client.Object) client.Client {
	s := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	_ = appsv1.AddToScheme(s)
	return fakeclient.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.BackupRestore{}).Build()
}

func secret(name string, data map[string]string) *corev1.Secret {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: name}, StringData: map[string]string{}}
	for k, v := range data {
		s.StringData[k] = v
	}
	return s
}

// fakeAdmin stands in for the calls made with the caller's own login.
type fakeAdmin struct {
	mu          sync.Mutex
	wrapped     string
	wrappedErr  error
	markers     int
	cleanupErr  error
	cleanups    []string // endpoint|bucket|prefix
	wrappedRead int
}

func (f *fakeAdmin) RemoveDeleteMarkers(_ context.Context, endpoint, bucket, prefix string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleanups = append(f.cleanups, endpoint+"|"+bucket+"|"+prefix)
	return f.markers, f.cleanupErr
}

func (f *fakeAdmin) ReadWrappedKey(context.Context, string, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wrappedRead++
	return f.wrapped, f.wrappedErr
}

func cosFlags() map[string]string {
	return map[string]string{
		"from-cos": "true", "policy": "nightly", "volume": "pvc-11111111-2222-3333-4444-555555555555",
		"to": "apps/db-restored", "bucket": "pool-backups", "endpoint": "s3.eu-de.cloud-object-storage.appdomain.cloud",
		"prefix": "team-a", "storage-class": "fast", "size-gb": "40",
		"credentials-secret": "cos-credentials", "backup-key-secret": "cos-backup-key",
		"kp-instance": "11111111-2222-3333-4444-555555555555", "kp-region": "eu-de",
		"kp-root-key": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "kp-api-key-secret": "kp-apikey",
		"poll": "1ms",
	}
}

func baseCOSObjects() []client.Object {
	return []client.Object{
		secret("cos-credentials", map[string]string{"access_key_id": "a", "secret_access_key": "b"}),
		secret("kp-apikey", map[string]string{"apikey": "k"}),
	}
}

func restores(t *testing.T, c client.Client) []v1alpha1.BackupRestore {
	t.Helper()
	l := &v1alpha1.BackupRestoreList{}
	if err := c.List(context.Background(), l); err != nil {
		t.Fatal(err)
	}
	return l.Items
}

func hasSecret(c client.Client, name string) (*corev1.Secret, bool) {
	s := &corev1.Secret{}
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "kube-system", Name: name}, s)
	return s, err == nil
}

func TestRestoreFromCOS_MakesTheRequestAndTheWrappedKeySecret(t *testing.T) {
	c := newCOSClient(baseCOSObjects()...)
	admin := &fakeAdmin{wrapped: "WRAPPED", markers: 3}
	var out bytes.Buffer
	if err := runRestoreFromCOS(context.Background(), c, &out, admin, cosFlags(), time.Now); err != nil {
		t.Fatal(err)
	}
	if len(admin.cleanups) != 1 || admin.cleanups[0] != "s3.eu-de.cloud-object-storage.appdomain.cloud|pool-backups|team-a/nightly/" {
		t.Errorf("cleanups = %v; want the area of the policy cleared", admin.cleanups)
	}
	if !strings.Contains(out.String(), "3 delete marker") {
		t.Errorf("output does not say that markers were removed:\n%s", out.String())
	}
	s, ok := hasSecret(c, "cos-backup-key")
	if !ok || string(s.Data["wrappedKey"]) != "WRAPPED" && s.StringData["wrappedKey"] != "WRAPPED" {
		t.Errorf("the wrapped key Secret was not made from the bucket: %+v", s)
	}
	rs := restores(t, c)
	if len(rs) != 1 {
		t.Fatalf("restores = %d, want 1", len(rs))
	}
	sp := rs[0].Spec
	f := sp.FromCOS
	if sp.Policy != "nightly" || sp.TargetNamespace != "apps" || sp.TargetName != "db-restored" || sp.StorageClassName != "fast" ||
		f == nil || f.Bucket != "pool-backups" || f.Prefix != "team-a" || f.SizeGB != 40 || f.BackupKeySecret != "cos-backup-key" ||
		f.KeyProtect == nil || f.KeyProtect.RootKeyID != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" || f.KeyProtect.APIKeySecret != "kp-apikey" ||
		f.Volume != "pvc-11111111-2222-3333-4444-555555555555" || f.EmergencyKeyOnly {
		t.Errorf("spec = %+v / %+v", sp, f)
	}
}

func TestRestoreFromCOS_UsesTheWrappedKeySecretThatExists(t *testing.T) {
	c := newCOSClient(append(baseCOSObjects(), secret("cos-backup-key", map[string]string{"wrappedKey": "FROM-CLUSTER"}))...)
	admin := &fakeAdmin{wrapped: "FROM-BUCKET"}
	if err := runRestoreFromCOS(context.Background(), c, &bytes.Buffer{}, admin, cosFlags(), time.Now); err != nil {
		t.Fatal(err)
	}
	if admin.wrappedRead != 0 {
		t.Error("the bucket was read for a key that the cluster already holds")
	}
}

func TestRestoreFromCOS_SkipMarkerCleanup(t *testing.T) {
	c := newCOSClient(baseCOSObjects()...)
	admin := &fakeAdmin{wrapped: "W"}
	f := cosFlags()
	f["skip-marker-cleanup"] = "true"
	if err := runRestoreFromCOS(context.Background(), c, &bytes.Buffer{}, admin, f, time.Now); err != nil {
		t.Fatal(err)
	}
	if len(admin.cleanups) != 0 {
		t.Error("the cleanup ran although it was skipped")
	}
}

func TestRestoreFromCOS_MarkerCleanupFailureStopsBeforeAnythingIsMade(t *testing.T) {
	c := newCOSClient(baseCOSObjects()...)
	admin := &fakeAdmin{wrapped: "W", cleanupErr: errors.New("HTTP 403")}
	err := runRestoreFromCOS(context.Background(), c, &bytes.Buffer{}, admin, cosFlags(), time.Now)
	if err == nil || !strings.Contains(err.Error(), "skip-marker-cleanup") {
		t.Errorf("err = %v; want a message that names --skip-marker-cleanup", err)
	}
	if len(restores(t, c)) != 0 {
		t.Error("a restore was asked for although the bucket could not be cleaned")
	}
}

func TestRestoreFromCOS_MissingSecretsAreNamedBeforeAnythingChanges(t *testing.T) {
	for _, missing := range []string{"cos-credentials", "kp-apikey"} {
		var objs []client.Object
		for _, o := range baseCOSObjects() {
			if o.GetName() != missing {
				objs = append(objs, o)
			}
		}
		c := newCOSClient(objs...)
		admin := &fakeAdmin{wrapped: "W"}
		err := runRestoreFromCOS(context.Background(), c, &bytes.Buffer{}, admin, cosFlags(), time.Now)
		if err == nil || !strings.Contains(err.Error(), missing) {
			t.Errorf("missing %s: err = %v, want one that names it", missing, err)
		}
		if len(admin.cleanups) != 0 || len(restores(t, c)) != 0 {
			t.Errorf("missing %s: something changed before the check", missing)
		}
	}
}

func TestRestoreFromCOS_RefusesBadFlags(t *testing.T) {
	cases := map[string]func(map[string]string){
		"no policy":         func(f map[string]string) { delete(f, "policy") },
		"no volume":         func(f map[string]string) { delete(f, "volume") },
		"bad volume":        func(f map[string]string) { f["volume"] = "../x" },
		"no bucket":         func(f map[string]string) { delete(f, "bucket") },
		"no endpoint":       func(f map[string]string) { delete(f, "endpoint") },
		"no target":         func(f map[string]string) { delete(f, "to") },
		"no storage class":  func(f map[string]string) { delete(f, "storage-class") },
		"no size":           func(f map[string]string) { delete(f, "size-gb") },
		"size not a number": func(f map[string]string) { f["size-gb"] = "big" },
		"no key protect":    func(f map[string]string) { delete(f, "kp-root-key") },
		"bad moment":        func(f map[string]string) { f["moment"] = "yesterday" },
	}
	for name, mutate := range cases {
		f := cosFlags()
		mutate(f)
		c := newCOSClient(baseCOSObjects()...)
		admin := &fakeAdmin{wrapped: "W"}
		if err := runRestoreFromCOS(context.Background(), c, &bytes.Buffer{}, admin, f, time.Now); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if len(restores(t, c)) != 0 || len(admin.cleanups) != 0 {
			t.Errorf("%s: something changed", name)
		}
	}
}

// finishRestores marks every restore Done or Failed, like the controller.
func finishRestores(t *testing.T, c client.Client, phase string) (stop func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case <-time.After(2 * time.Millisecond):
			}
			l := &v1alpha1.BackupRestoreList{}
			if c.List(context.Background(), l) != nil {
				continue
			}
			for i := range l.Items {
				br := &l.Items[i]
				if br.Status.Phase == "" {
					br.Status.Phase, br.Status.Message = phase, "stand-in"
					_ = c.Status().Update(context.Background(), br)
				}
			}
		}
	}()
	return func() { close(done) }
}

func TestRestoreFromCOS_EmergencyKeyFileMakesATemporarySecretAndRemovesIt(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key")
	if err := os.WriteFile(keyFile, []byte(cosKeySentinel+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newCOSClient(baseCOSObjects()...)
	admin := &fakeAdmin{wrapped: "W"}
	f := cosFlags()
	f["backup-key-file"] = keyFile
	for _, k := range []string{"kp-instance", "kp-region", "kp-root-key", "kp-api-key-secret", "backup-key-secret"} {
		delete(f, k)
	}
	stop := finishRestores(t, c, "Done")
	defer stop()
	var out bytes.Buffer
	if err := runRestoreFromCOS(context.Background(), c, &out, admin, f, time.Now); err != nil {
		t.Fatal(err)
	}
	rs := restores(t, c)
	if len(rs) != 1 || !rs[0].Spec.FromCOS.EmergencyKeyOnly || rs[0].Spec.FromCOS.KeyProtect != nil {
		t.Fatalf("restores = %+v; want one emergency-only request", rs)
	}
	name := rs[0].Spec.FromCOS.EmergencyKeySecret
	if name == "" {
		t.Fatal("the request names no emergency key Secret")
	}
	if _, ok := hasSecret(c, name); ok {
		t.Error("the Secret with the readable key was left in the cluster after the restore")
	}
	if admin.wrappedRead != 0 {
		t.Error("the wrapped key was read although Key Protect is not used")
	}
	if strings.Contains(out.String(), cosKeySentinel) {
		t.Error("the readable key was printed")
	}
}

func TestRestoreFromCOS_EmergencySecretHoldsTheKeyWhileTheRestoreRuns(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key")
	if err := os.WriteFile(keyFile, []byte(" "+cosKeySentinel+" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newCOSClient(baseCOSObjects()...)
	f := cosFlags()
	f["backup-key-file"] = keyFile
	f["timeout"] = "30ms"
	// No controller: the wait times out and the Secret stays for the Jobs.
	err := runRestoreFromCOS(context.Background(), c, &bytes.Buffer{}, &fakeAdmin{}, f, time.Now)
	if err == nil {
		t.Fatal("want a timeout")
	}
	rs := restores(t, c)
	if len(rs) != 1 {
		t.Fatalf("restores = %d", len(rs))
	}
	s, ok := hasSecret(c, rs[0].Spec.FromCOS.EmergencyKeySecret)
	if !ok {
		t.Fatal("the Secret was removed while the restore still runs")
	}
	if got := s.StringData["backupKey"] + string(s.Data["backupKey"]); got != cosKeySentinel {
		t.Errorf("the Secret holds %q, want the key without spaces", got)
	}
	if !strings.Contains(err.Error(), "kubectl delete secret") {
		t.Errorf("err = %v; want the command that removes the Secret", err)
	}
}

func TestRestoreFromCOS_FailedRestoreIsAnErrorAndRemovesTheEmergencySecret(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key")
	if err := os.WriteFile(keyFile, []byte(cosKeySentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newCOSClient(baseCOSObjects()...)
	f := cosFlags()
	f["backup-key-file"] = keyFile
	stop := finishRestores(t, c, "Failed")
	defer stop()
	err := runRestoreFromCOS(context.Background(), c, &bytes.Buffer{}, &fakeAdmin{}, f, time.Now)
	if err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("err = %v, want the restore failed", err)
	}
	rs := restores(t, c)
	if _, ok := hasSecret(c, rs[0].Spec.FromCOS.EmergencyKeySecret); ok {
		t.Error("the Secret with the readable key was left after a failed restore")
	}
}

func TestRestoreFromCOS_KeyFileMustBeReadable(t *testing.T) {
	c := newCOSClient(baseCOSObjects()...)
	f := cosFlags()
	f["backup-key-file"] = filepath.Join(t.TempDir(), "missing")
	if err := runRestoreFromCOS(context.Background(), c, &bytes.Buffer{}, &fakeAdmin{}, f, time.Now); err == nil {
		t.Error("accepted a key file that does not exist")
	}
	if len(restores(t, c)) != 0 {
		t.Error("a restore was made")
	}
}

// ---- cos-list ----

type fakeLister struct {
	listing pool.COSListing
	err     error
	repo    pool.ExportRepo
	volume  string
	pending int
	image   string
}

func (f *fakeLister) ListCOS(_ context.Context, _ string, repo pool.ExportRepo, volume string) (bool, pool.COSListing, error) {
	f.repo, f.volume = repo, volume
	if f.pending > 0 {
		f.pending--
		return false, pool.COSListing{}, nil
	}
	return true, f.listing, f.err
}

func driverDeployment(image string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "pool-csi-controller", Labels: map[string]string{
			"app.kubernetes.io/name": "ibm-vpc-file-pool-csi", "app.kubernetes.io/component": "controller"}},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "csi-provisioner", Image: "sidecar:1"}, {Name: "csi-controller", Image: image}}}}},
	}
}

func listFlags() map[string]string {
	f := cosFlags()
	for _, k := range []string{"from-cos", "to", "storage-class", "size-gb", "volume"} {
		delete(f, k)
	}
	return f
}

func TestCOSList_PrintsVolumesAndMoments(t *testing.T) {
	c := newCOSClient(append(baseCOSObjects(), secret("cos-backup-key", map[string]string{"wrappedKey": "W"}), driverDeployment("reg/driver:1"))...)
	m1 := time.Date(2026, 10, 21, 9, 30, 0, 0, time.UTC)
	l := &fakeLister{pending: 2, listing: pool.COSListing{Volumes: []pool.COSVolume{
		{Name: "pvc-11111111-2222-3333-4444-555555555555", Moments: []time.Time{m1, m1.Add(time.Hour)}}}}}
	var out bytes.Buffer
	var gotImage string
	err := runCOSList(context.Background(), c, &out, &fakeAdmin{}, func(image string) cosLister { gotImage = image; return l }, listFlags())
	if err != nil {
		t.Fatal(err)
	}
	if gotImage != "reg/driver:1" {
		t.Errorf("image = %q, want the image of the controller container", gotImage)
	}
	for _, want := range []string{"pvc-11111111-2222-3333-4444-555555555555", "2026-10-21T09:30:00Z", "2026-10-21T10:30:00Z"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if l.repo.Prefix != "team-a/nightly/" || l.repo.Bucket != "pool-backups" {
		t.Errorf("repo = %+v", l.repo)
	}
}

func TestCOSList_TruncatedListingTellsHowToSeeAll(t *testing.T) {
	c := newCOSClient(append(baseCOSObjects(), secret("cos-backup-key", map[string]string{"wrappedKey": "W"}), driverDeployment("reg/driver:1"))...)
	l := &fakeLister{listing: pool.COSListing{Truncated: true, Volumes: []pool.COSVolume{{Name: "pvc-1"}}}}
	var out bytes.Buffer
	if err := runCOSList(context.Background(), c, &out, &fakeAdmin{}, func(string) cosLister { return l }, listFlags()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "--volume") {
		t.Errorf("output does not say how to see the rest:\n%s", out.String())
	}
}

func TestCOSList_DriverImageFlagAndMissingDeployment(t *testing.T) {
	objs := append(baseCOSObjects(), secret("cos-backup-key", map[string]string{"wrappedKey": "W"}))
	var gotImage string
	factory := func(image string) cosLister { gotImage = image; return &fakeLister{} }

	f := listFlags()
	f["driver-image"] = "mine/driver:2"
	if err := runCOSList(context.Background(), newCOSClient(objs...), &bytes.Buffer{}, &fakeAdmin{}, factory, f); err != nil || gotImage != "mine/driver:2" {
		t.Errorf("err = %v image = %q; want the flag used", err, gotImage)
	}
	err := runCOSList(context.Background(), newCOSClient(objs...), &bytes.Buffer{}, &fakeAdmin{}, factory, listFlags())
	if err == nil || !strings.Contains(err.Error(), "--driver-image") {
		t.Errorf("err = %v; want a message that names --driver-image", err)
	}
}

func TestCOSList_PassesTheVolumeFilter(t *testing.T) {
	c := newCOSClient(append(baseCOSObjects(), secret("cos-backup-key", map[string]string{"wrappedKey": "W"}), driverDeployment("reg/driver:1"))...)
	l := &fakeLister{}
	f := listFlags()
	f["volume"] = "pvc-11111111-2222-3333-4444-555555555555"
	if err := runCOSList(context.Background(), c, &bytes.Buffer{}, &fakeAdmin{}, func(string) cosLister { return l }, f); err != nil {
		t.Fatal(err)
	}
	if l.volume != f["volume"] {
		t.Errorf("volume = %q", l.volume)
	}
}

func TestCOSList_RefusesWhenTheKeySecretsAreMissing(t *testing.T) {
	c := newCOSClient(driverDeployment("reg/driver:1"))
	err := runCOSList(context.Background(), c, &bytes.Buffer{}, &fakeAdmin{}, func(string) cosLister { return &fakeLister{} }, listFlags())
	if err == nil || !strings.Contains(err.Error(), "cos-credentials") {
		t.Errorf("err = %v, want one that names the missing Secret", err)
	}
}
