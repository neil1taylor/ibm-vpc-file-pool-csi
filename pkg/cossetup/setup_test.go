package cossetup

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const (
	testRootKey = "crn:v1:bluemix:public:kms:eu-de:a/acct1:kpinst1:key:rootkey1"
	testSaved   = "I saved the backup key\n"
)

// The list from TUTORIAL-09-COS-BACKUP-BUCKET.md, step 3, written out again so
// that a change to the role must change this test too.
var tutorialActions = []string{
	"cloud-object-storage.account.get_account_buckets",
	"cloud-object-storage.bucket.get", "cloud-object-storage.bucket.get_basic", "cloud-object-storage.bucket.head",
	"cloud-object-storage.bucket.get_location", "cloud-object-storage.bucket.get_versioning",
	"cloud-object-storage.bucket.get_versions", "cloud-object-storage.bucket.get_object_lock_configuration",
	"cloud-object-storage.bucket.get_uploads",
	"cloud-object-storage.object.get", "cloud-object-storage.object.get_version", "cloud-object-storage.object.head",
	"cloud-object-storage.object.head_version", "cloud-object-storage.object.put", "cloud-object-storage.object.post",
	"cloud-object-storage.object.post_initiate_upload", "cloud-object-storage.object.put_part",
	"cloud-object-storage.object.post_complete_upload", "cloud-object-storage.object.get_uploads",
	"cloud-object-storage.object.delete_upload", "cloud-object-storage.object.delete",
	"cloud-object-storage.object.get_object_lock_retention", "cloud-object-storage.object.get_object_lock_retention_version",
	"cloud-object-storage.object.put_object_lock_retention", "cloud-object-storage.object.put_object_lock_retention_version",
	"cloud-object-storage.object.post_extend_retention",
}

func TestRoleActionsEqualTheTutorial(t *testing.T) {
	got := append([]string(nil), WriterActions()...)
	want := append([]string(nil), tutorialActions...)
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("role actions differ from the tutorial\n got: %v\nwant: %v", got, want)
	}
}

func TestRoleActionsLeaveOutDeleteAndUnlock(t *testing.T) {
	for _, a := range WriterActions() {
		short := strings.TrimPrefix(a, "cloud-object-storage.")
		if short == "object.delete_version" || short == "object.bypass_governance_retention" ||
			strings.HasPrefix(short, "bucket.put_") || strings.HasPrefix(short, "bucket.delete_") {
			t.Errorf("role must not hold %s", a)
		}
	}
}

type env struct {
	t     *testing.T
	fake  *fakeCloud
	setup *Setup
	out   *bytes.Buffer
	opts  Options
	dir   string
}

// staticAddress answers with a fixed address of the caller.
type staticAddress string

func (a staticAddress) Address(context.Context) (string, error) { return string(a), nil }

func fakeJWT() string {
	payload, _ := json.Marshal(map[string]any{"account": map[string]any{"bss": "acct1"}})
	return "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func newEnv(t *testing.T, input string) *env {
	t.Helper()
	fake := newFakeCloud()
	srv := startFake(fake)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	out := &bytes.Buffer{}
	return &env{t: t, fake: fake, out: out, dir: dir,
		setup: &Setup{
			HTTP: srv.Client(), Token: StaticToken(fakeJWT()), Address: staticAddress("203.0.113.7"),
			Endpoints: Endpoints{
				ResourceController: srv.URL + "/rc", ResourceManager: srv.URL + "/rm", IAM: srv.URL + "/iam", CBR: srv.URL + "/cbr",
				COS:        func(string) string { return srv.URL + "/cos" },
				KeyProtect: func(string) string { return srv.URL + "/kp" },
			},
			Rand: bytes.NewReader(bytes.Repeat([]byte{7}, 64)), In: strings.NewReader(input), Out: out, Interactive: true,
			allowAnyHost: true, // the fake runs on 127.0.0.1; only tests can set this
		},
		opts: Options{
			Region: "eu-de", ResourceGroup: "rg", InstanceName: "pool-csi-backup-cos", Bucket: "pool-csi-test-bucket",
			RootKeyCRN: testRootKey, VPCCRN: "crn:v1:bluemix:public:is:eu-de:a/acct1::vpc:r010-vpc1",
			AdminAddresses: []string{"203.0.113.7", "198.51.100.0/24"}, CBRMode: "enabled", KeepFor: "30d",
			Namespace: "kube-system", KeyFile: filepath.Join(dir, "cos-key.yaml"), SecretFile: filepath.Join(dir, "backup-key-secret.yaml"),
			Yes: true,
		}}
}

// withInstance makes the COS instance exist in the fake, as after an earlier run.
func (e *env) withInstance() {
	e.fake.instances[e.opts.InstanceName] = map[string]any{"guid": "guid-old", "name": e.opts.InstanceName}
}

func (e *env) run() error { e.t.Helper(); return e.setup.Run(context.Background(), e.opts) }

func (e *env) readableKey() string {
	return base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
}

func TestFirstRunMakesEverything(t *testing.T) {
	e := newEnv(t, testSaved)
	if err := e.run(); err != nil {
		t.Fatalf("run: %v\n%s", err, e.out)
	}
	f := e.fake
	if len(f.instances) != 1 {
		t.Errorf("want 1 COS instance, got %d", len(f.instances))
	}
	b := f.buckets["pool-csi-test-bucket"]
	if b == nil || !b.versioning || !b.lock {
		t.Fatalf("bucket missing, or without versioning and object lock: %+v", b)
	}
	if b.kpCRN != testRootKey {
		t.Errorf("bucket is not encrypted with the root key: %q", b.kpCRN)
	}
	if _, ok := b.objects[WrappedKeyObject]; !ok {
		t.Errorf("wrapped backup key is not stored next to the backups; objects: %v", b.objects)
	}
	role := f.roles["PoolCsiBackupWriter"]
	if role == nil {
		t.Fatal("custom role not made")
	}
	var actions []string
	raw, _ := role["actions"].([]any)
	for _, a := range raw {
		s, _ := a.(string)
		actions = append(actions, s)
	}
	sort.Strings(actions)
	want := append([]string(nil), tutorialActions...)
	sort.Strings(want)
	if strings.Join(actions, ",") != strings.Join(want, ",") {
		t.Errorf("role actions sent to IAM differ from the tutorial: %v", actions)
	}
	if len(f.serviceID) != 1 || len(f.keys) != 1 {
		t.Errorf("want 1 service ID and 1 HMAC key, got %d and %d", len(f.serviceID), len(f.keys))
	}
	if len(f.zones) != 1 || len(f.rules) != 1 {
		t.Fatalf("want 1 zone and 1 rule, got %d and %d", len(f.zones), len(f.rules))
	}
	rule, _ := json.Marshal(f.rules[0])
	for _, need := range []string{`"enforcement_mode":"enabled"`, "api-type:data-plane", "pool-csi-test-bucket"} {
		if !strings.Contains(string(rule), need) {
			t.Errorf("CBR rule lacks %s: %s", need, rule)
		}
	}
	zone, _ := json.Marshal(f.zones[onlyKey(f.zones)])
	for _, need := range []string{"r010-vpc1", "203.0.113.7", "198.51.100.0/24"} {
		if !strings.Contains(string(zone), need) {
			t.Errorf("CBR zone lacks %s: %s", need, zone)
		}
	}
	// A service ID policy on the bucket, one Reader on the root key and one
	// authorization for COS to use Key Protect.
	pols, _ := json.Marshal(f.policies)
	for _, need := range []string{"customRole:PoolCsiBackupWriter", "serviceRole:Reader", `"authorization"`, "pool-csi-test-bucket", "rootkey1"} {
		if !strings.Contains(string(pols), need) {
			t.Errorf("policies lack %s: %s", need, pols)
		}
	}
	if f.hasWrite("DELETE ") {
		t.Error("the command must never delete anything")
	}
}

func onlyKey(m map[string]map[string]any) string {
	for k := range m {
		return k
	}
	return ""
}

func TestSecretsNeverPrintedAndFilesAreSafe(t *testing.T) {
	e := newEnv(t, testSaved)
	if err := e.run(); err != nil {
		t.Fatalf("run: %v\n%s", err, e.out)
	}
	out := e.out.String()
	if strings.Contains(out, "SECRETHMACFAKE") || strings.Contains(out, fakeJWT()) {
		t.Error("output holds the HMAC secret or the IAM token")
	}
	if n := strings.Count(out, e.readableKey()); n != 1 {
		t.Errorf("the readable backup key must be shown exactly once, was shown %d times", n)
	}
	for _, need := range []string{"export:", "bucket: pool-csi-test-bucket", "credentialsSecret: cos-credentials", "backupKeySecret: cos-backup-key", "keepFor: 30d", "lock: Compliance"} {
		if !strings.Contains(out, need) {
			t.Errorf("output lacks %q:\n%s", need, out)
		}
	}
	fi, err := os.Stat(e.opts.KeyFile)
	if err != nil {
		t.Fatalf("key file: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("key file mode is %v, want 0600", fi.Mode().Perm())
	}
	key, _ := os.ReadFile(e.opts.KeyFile)
	if !strings.Contains(string(key), "SECRETHMACFAKE") || !strings.Contains(string(key), "AKIDFAKE") {
		t.Errorf("key file lacks the HMAC key: %s", key)
	}
	sec, err := os.ReadFile(e.opts.SecretFile)
	if err != nil {
		t.Fatalf("secret file: %v", err)
	}
	for _, bad := range []string{e.readableKey(), "SECRETHMACFAKE", "AKIDFAKE", fakeJWT()} {
		if strings.Contains(string(sec), bad) {
			t.Errorf("secret manifest holds %q: it must hold only the wrapped key", bad)
		}
	}
	if !strings.Contains(string(sec), "kind: Secret") || !strings.Contains(string(sec), "kp:") || !strings.Contains(string(sec), "name: cos-backup-key") {
		t.Errorf("secret manifest is not the wrapped key Secret:\n%s", sec)
	}
	for _, v := range e.fake.buckets["pool-csi-test-bucket"].objects {
		if strings.Contains(v, e.readableKey()) {
			t.Error("the readable key reached the bucket")
		}
	}
}

func TestSecondRunChangesNothing(t *testing.T) {
	e := newEnv(t, testSaved)
	if err := e.run(); err != nil {
		t.Fatalf("first run: %v\n%s", err, e.out)
	}
	before, _ := os.ReadFile(e.opts.KeyFile)
	n := e.fake.writeCount()
	e.out.Reset()
	e.setup.In = strings.NewReader("")
	e.setup.Rand = bytes.NewReader(bytes.Repeat([]byte{9}, 64))
	if err := e.run(); err != nil {
		t.Fatalf("second run: %v\n%s", err, e.out)
	}
	if got := e.fake.writeCount(); got != n {
		t.Errorf("second run made %d write calls: %v", got-n, e.fake.writes[n:])
	}
	after, _ := os.ReadFile(e.opts.KeyFile)
	if !bytes.Equal(before, after) {
		t.Error("second run changed the key file")
	}
	if !strings.Contains(e.out.String(), "export:") {
		t.Error("second run should still print the spec.export block")
	}
	if strings.Contains(e.out.String(), base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))) {
		t.Error("second run made and showed a new backup key")
	}
}

func TestDryRunMakesNoWriteCall(t *testing.T) {
	e := newEnv(t, "")
	e.opts.DryRun = true
	if err := e.run(); err != nil {
		t.Fatalf("run: %v\n%s", err, e.out)
	}
	if n := e.fake.writeCount(); n != 0 {
		t.Errorf("dry run made %d write calls: %v", n, e.fake.writes)
	}
	for _, f := range []string{e.opts.KeyFile, e.opts.SecretFile} {
		if _, err := os.Stat(f); err == nil {
			t.Errorf("dry run wrote %s", f)
		}
	}
	for _, need := range []string{"PoolCsiBackupWriter", "pool-csi-test-bucket", "would create", "cloud-object-storage.object.put", "nothing was changed"} {
		if !strings.Contains(e.out.String(), need) {
			t.Errorf("dry run output lacks %q:\n%s", need, e.out)
		}
	}
	if strings.Contains(e.out.String(), e.readableKey()) {
		t.Error("dry run showed a key")
	}
}

func TestRefusesWhenCallerNotInZone(t *testing.T) {
	e := newEnv(t, testSaved)
	e.setup.Address = staticAddress("192.0.2.99")
	err := e.run()
	if err == nil || !strings.Contains(err.Error(), "192.0.2.99") {
		t.Fatalf("want a refusal that names the caller's address, got %v", err)
	}
	if n := e.fake.writeCount(); n != 0 {
		t.Errorf("made %d write calls before refusing: %v", n, e.fake.writes)
	}
}

func TestReportModeAllowsAddressOutsideZone(t *testing.T) {
	e := newEnv(t, testSaved)
	e.setup.Address = staticAddress("192.0.2.99")
	e.opts.CBRMode = "report"
	if err := e.run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	rule, _ := json.Marshal(e.fake.rules[0])
	if !strings.Contains(string(rule), `"enforcement_mode":"report"`) {
		t.Errorf("rule is not in report mode: %s", rule)
	}
}

func TestExistingZoneIDIsUsedAndChecked(t *testing.T) {
	e := newEnv(t, testSaved)
	e.fake.zones["zone-x"] = map[string]any{"id": "zone-x", "name": "mine", "addresses": []any{map[string]any{"type": "ipAddress", "value": "192.0.2.1"}}}
	e.opts.ZoneID, e.opts.VPCCRN, e.opts.AdminAddresses = "zone-x", "", nil
	if err := e.run(); err == nil || !strings.Contains(err.Error(), "203.0.113.7") {
		t.Fatalf("caller is not in zone-x, want a refusal naming the address, got %v", err)
	}
	e.fake.zones["zone-x"]["addresses"] = []any{map[string]any{"type": "ipRange", "value": "203.0.113.1-203.0.113.20"}}
	if err := e.run(); err != nil {
		t.Fatalf("caller is in the range: %v", err)
	}
	if len(e.fake.zones) != 1 {
		t.Errorf("made a zone although --zone-id was given: %d zones", len(e.fake.zones))
	}
}

func TestRefusesBucketItDidNotMake(t *testing.T) {
	e := newEnv(t, testSaved)
	e.withInstance()
	e.fake.buckets["pool-csi-test-bucket"] = &fakeBucket{objects: map[string]string{}, versioning: true, lock: true}
	err := e.run()
	if err == nil || !strings.Contains(err.Error(), "--existing-bucket") {
		t.Fatalf("want a refusal that names --existing-bucket, got %v", err)
	}
	if n := e.fake.writeCount(); n != 0 {
		t.Errorf("made %d write calls before refusing: %v", n, e.fake.writes)
	}
	e.opts.ExistingBucket = true
	if err = e.run(); err != nil {
		t.Fatalf("with --existing-bucket: %v\n%s", err, e.out)
	}
	if e.fake.hasWrite("PUT /cos/pool-csi-test-bucket?") || e.fake.hasWrite("PUT /cos/pool-csi-test-bucket HTTP") {
		t.Error("changed settings of a bucket it did not make")
	}
}

func TestExistingBucketWithoutLockIsRefused(t *testing.T) {
	e := newEnv(t, testSaved)
	e.withInstance()
	e.fake.buckets["pool-csi-test-bucket"] = &fakeBucket{objects: map[string]string{}, versioning: true}
	e.opts.ExistingBucket = true
	if err := e.run(); err == nil || !strings.Contains(err.Error(), "object lock") {
		t.Fatalf("want a refusal about object lock, got %v", err)
	}
}

func TestTypedConfirmationIsRequired(t *testing.T) {
	e := newEnv(t, "no\n")
	e.opts.Yes = false
	err := e.run()
	if err == nil || !strings.Contains(err.Error(), "not confirmed") {
		t.Fatalf("want not confirmed, got %v", err)
	}
	if n := e.fake.writeCount(); n != 0 {
		t.Errorf("made %d write calls without a confirmation", n)
	}
	e = newEnv(t, "pool-csi-test-bucket\n"+testSaved)
	e.opts.Yes = false
	if err = e.run(); err != nil {
		t.Fatalf("typed the bucket name: %v\n%s", err, e.out)
	}
}

func TestNonInteractiveNeedsYes(t *testing.T) {
	e := newEnv(t, "")
	e.opts.Yes = false
	e.setup.Interactive = false
	if err := e.run(); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("want a message about --yes, got %v", err)
	}
	if e.fake.writeCount() != 0 {
		t.Error("wrote without a confirmation")
	}
}

func TestBackupKeyNeedsAPersonToSeeIt(t *testing.T) {
	e := newEnv(t, "")
	e.setup.Interactive = false
	if err := e.run(); err == nil || !strings.Contains(err.Error(), "interactive") {
		t.Fatalf("want a refusal about the backup key needing an interactive shell, got %v", err)
	}
	if e.fake.writeCount() != 0 {
		t.Error("made changes in a shell that cannot show the backup key")
	}
}

func TestUnconfirmedKeyIsNotStored(t *testing.T) {
	e := newEnv(t, "no\n")
	err := e.run()
	if err == nil || !strings.Contains(err.Error(), "backup key") {
		t.Fatalf("want an error about the backup key, got %v", err)
	}
	if _, err := os.Stat(e.opts.SecretFile); err == nil {
		t.Error("secret manifest written without a confirmation that the key is saved")
	}
	if _, ok := e.fake.buckets["pool-csi-test-bucket"].objects[WrappedKeyObject]; ok {
		t.Error("wrapped key stored in the bucket without a confirmation")
	}
}

func TestKeepForAbove90DaysNeedsTypedConfirmation(t *testing.T) {
	e := newEnv(t, testSaved)
	e.opts.KeepFor = "120d"
	if err := e.run(); err == nil || !strings.Contains(err.Error(), "120d") {
		t.Fatalf("want a refusal that names the keep time, got %v", err)
	}
	if e.fake.writeCount() != 0 {
		t.Error("wrote before the long keep time was confirmed")
	}
	e = newEnv(t, "keep for 120d\n"+testSaved)
	e.opts.KeepFor = "120d"
	e.setup.Interactive = true
	if err := e.run(); err != nil {
		t.Fatalf("typed the confirmation: %v\n%s", err, e.out)
	}
	e = newEnv(t, testSaved)
	e.opts.KeepFor, e.opts.ConfirmKeepFor = "120d", "120d"
	if err := e.run(); err != nil {
		t.Fatalf("with --confirm-keep-for: %v", err)
	}
}

func TestKeepForBelowMinimumIsRefused(t *testing.T) {
	e := newEnv(t, testSaved)
	e.opts.KeepFor = "24h"
	if err := e.run(); err == nil || !strings.Contains(err.Error(), "25h") {
		t.Fatalf("want a refusal that names 25h, got %v", err)
	}
	if e.fake.writeCount() != 0 {
		t.Error("wrote before refusing")
	}
}

func TestRoleWithOtherActionsIsNotChanged(t *testing.T) {
	e := newEnv(t, testSaved)
	e.fake.roles["PoolCsiBackupWriter"] = map[string]any{"name": "PoolCsiBackupWriter", "crn": "crn:x", "actions": []any{"cloud-object-storage.object.delete_version"}}
	err := e.run()
	if err == nil || !strings.Contains(err.Error(), "PoolCsiBackupWriter") {
		t.Fatalf("want a refusal about the existing role, got %v", err)
	}
	if e.fake.hasWrite("/iam/v2/roles") {
		t.Error("changed an existing role")
	}
}

func TestBroadServiceIDPolicyIsReported(t *testing.T) {
	e := newEnv(t, testSaved)
	e.fake.broadPolicyOnKey = true
	err := e.run()
	if err == nil || !strings.Contains(err.Error(), "whole") {
		t.Fatalf("want an error that the service ID has access to more than the bucket, got %v", err)
	}
}

func TestUnknownRootKeyIsRefusedBeforeAnyChange(t *testing.T) {
	e := newEnv(t, testSaved)
	e.opts.RootKeyCRN = "crn:v1:bluemix:public:kms:eu-de:a/acct1:kpinst1:key:nokey"
	if err := e.run(); err == nil {
		t.Fatal("want an error for a root key that does not exist")
	}
	if e.fake.writeCount() != 0 {
		t.Error("wrote before checking the root key")
	}
}

func TestRequiresRootKey(t *testing.T) {
	e := newEnv(t, testSaved)
	e.opts.RootKeyCRN = ""
	if err := e.run(); err == nil || !strings.Contains(err.Error(), "root key") {
		t.Fatalf("want a message about the root key, got %v", err)
	}
}
