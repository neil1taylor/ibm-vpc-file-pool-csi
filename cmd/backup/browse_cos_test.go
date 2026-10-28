package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
)

func bucketArgs(extra ...string) map[string]string {
	args := append([]string{"--from-cos", "--policy", "lost-policy", "--bucket", "my-bucket",
		"--endpoint", "s3.eu-de.cloud-object-storage.appdomain.cloud", "--volume", "pvc-11111111-2222-3333-4444-555555555555",
		"--kp-instance", "inst-1", "--kp-region", "eu-de", "--kp-root-key", "root-1"}, extra...)
	return parseBrowseArgs(args)
}

func TestFromCOSAloneReadsTheBucketOfThePolicy(t *testing.T) {
	flags := parseBrowseArgs([]string{"--from-cos", "--policy", "nightly", "--pvc", "apps/db", "--moment", "2026-10-01T09:00:00Z"})
	tg, err := parseTarget(flags)
	if err != nil {
		t.Fatal(err)
	}
	if tg.source != "cos" || tg.bucket != nil || tg.namespace != "apps" || tg.pvc != "db" || tg.moment == nil {
		t.Errorf("target = %+v", tg)
	}
	// Without the flag the gateway chooses.
	flags = parseBrowseArgs([]string{"--policy", "nightly", "--pvc", "apps/db"})
	tg, err = parseTarget(flags)
	if err != nil || tg.source != "" || tg.bucket != nil {
		t.Errorf("target = %+v, %v", tg, err)
	}
}

func TestFromCOSWithABucketTakesTheFlagsOfRestoreFromCOS(t *testing.T) {
	tg, err := parseTarget(bucketArgs("--prefix", "team-a", "--credentials-secret", "my-cos", "--kp-api-key-secret", "my-kp"))
	if err != nil {
		t.Fatal(err)
	}
	b := tg.bucket
	if tg.source != "cos" || b == nil || tg.policy != "lost-policy" || tg.pvc != "" || tg.namespace != "" {
		t.Fatalf("target = %+v", tg)
	}
	if b.Bucket != "my-bucket" || b.Prefix != "team-a" || b.CredentialsSecret != "my-cos" || b.BackupKeySecret != "cos-backup-key" ||
		b.KeyProtect == nil || b.KeyProtect.APIKeySecret != "my-kp" || b.KeyProtect.RootKeyID != "root-1" || b.Volume != "pvc-11111111-2222-3333-4444-555555555555" {
		t.Errorf("bucket = %+v", b)
	}
	// The defaults are those of restore --from-cos.
	tg, err = parseTarget(bucketArgs())
	if err != nil || tg.bucket.CredentialsSecret != "cos-credentials" || tg.bucket.KeyProtect.APIKeySecret != "keyprotect-apikey" {
		t.Errorf("defaults = %+v, %v", tg.bucket, err)
	}
}

func TestFromCOSEmergencyKeyNeedsASecretAndNeverAFile(t *testing.T) {
	flags := parseBrowseArgs([]string{"--from-cos", "--policy", "p", "--bucket", "my-bucket", "--endpoint", "s3.eu-de.cloud-object-storage.appdomain.cloud",
		"--volume", "pvc-11111111-2222-3333-4444-555555555555", "--emergency-key-only", "--emergency-key-secret", "my-key"})
	tg, err := parseTarget(flags)
	if err != nil || !tg.bucket.EmergencyKeyOnly || tg.bucket.EmergencyKeySecret != "my-key" || tg.bucket.KeyProtect != nil {
		t.Fatalf("bucket = %+v, %v", tg.bucket, err)
	}
	flags["emergency-key-secret"] = ""
	if _, err = parseTarget(flags); err == nil || !strings.Contains(err.Error(), "--emergency-key-secret") {
		t.Errorf("err = %v", err)
	}
	_, err = parseTarget(bucketArgs("--backup-key-file", "/tmp/key"))
	if err == nil || !strings.Contains(err.Error(), "--emergency-key-secret") {
		t.Errorf("a key file: %v", err)
	}
	// With Key Protect, an emergency Secret is an extra.
	tg, err = parseTarget(bucketArgs("--emergency-key-secret", "my-key"))
	if err != nil || tg.bucket.EmergencyKeySecret != "my-key" || tg.bucket.EmergencyKeyOnly || tg.bucket.KeyProtect == nil {
		t.Errorf("bucket = %+v, %v", tg.bucket, err)
	}
}

func TestFromCOSMistakesAreSaidBeforeAnythingIsSent(t *testing.T) {
	cases := map[string]struct {
		args []string
		want string
	}{
		"bucket flags without --from-cos": {[]string{"--policy", "p", "--pvc", "a/b", "--bucket", "x"}, "add --from-cos"},
		"credentials secret alone": {[]string{"--from-cos", "--policy", "p", "--pvc", "a/b", "--credentials-secret", "x"},
			"name a bucket"},
		"no volume": {[]string{"--from-cos", "--policy", "p", "--bucket", "b", "--endpoint", "s3.eu-de.cloud-object-storage.appdomain.cloud",
			"--kp-instance", "i", "--kp-region", "eu-de", "--kp-root-key", "r"}, "--volume is required"},
		"no endpoint": {[]string{"--from-cos", "--policy", "p", "--bucket", "bkt", "--volume", "pvc-11111111-2222-3333-4444-555555555555"}, "--endpoint is required"},
		"no Key Protect flags": {[]string{"--from-cos", "--policy", "p", "--bucket", "bkt", "--endpoint", "s3.eu-de.cloud-object-storage.appdomain.cloud",
			"--volume", "pvc-11111111-2222-3333-4444-555555555555"}, "--kp-instance is required"},
		"an endpoint that is not COS": {[]string{"--from-cos", "--policy", "p", "--bucket", "bkt", "--endpoint", "evil.example", "--volume", "pvc-11111111-2222-3333-4444-555555555555",
			"--kp-instance", "i", "--kp-region", "eu-de", "--kp-root-key", "r"}, "fromCOS.endpoint"},
		"a volume that is a path": {[]string{"--from-cos", "--policy", "p", "--bucket", "bkt", "--endpoint", "s3.eu-de.cloud-object-storage.appdomain.cloud",
			"--volume", "../../etc", "--kp-instance", "i", "--kp-region", "eu-de", "--kp-root-key", "r"}, "fromCOS.volume"},
		"a bucket and a claim": {[]string{"--from-cos", "--policy", "p", "--pvc", "a/b", "--bucket", "bkt", "--endpoint", "s3.eu-de.cloud-object-storage.appdomain.cloud",
			"--volume", "pvc-11111111-2222-3333-4444-555555555555", "--kp-instance", "i", "--kp-region", "eu-de", "--kp-root-key", "r"}, "no --pvc"},
		"a bucket and no policy": {[]string{"--from-cos", "--bucket", "bkt", "--endpoint", "s3.eu-de.cloud-object-storage.appdomain.cloud",
			"--volume", "pvc-11111111-2222-3333-4444-555555555555", "--kp-instance", "i", "--kp-region", "eu-de", "--kp-root-key", "r"}, "--policy"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parseTarget(parseBrowseArgs(c.args))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want one with %q", err, c.want)
			}
		})
	}
}

func TestSessionRequestCarriesTheSource(t *testing.T) {
	stub, gw := newStub(t)
	stub.pages[""] = browse.ListPage{}
	var out bytes.Buffer
	args := []string{"--from-cos", "--policy", "nightly", "--pvc", "apps/db", "/"}
	if err := runLs(context.Background(), gw, &out, args); err != nil {
		t.Fatal(err)
	}
	args = []string{"--from-cos", "--policy", "lost-policy", "--bucket", "my-bucket", "--endpoint", "s3.eu-de.cloud-object-storage.appdomain.cloud",
		"--volume", "pvc-11111111-2222-3333-4444-555555555555", "--kp-instance", "i", "--kp-region", "eu-de", "--kp-root-key", "r", "/"}
	if err := runLs(context.Background(), gw, &out, args); err != nil {
		t.Fatal(err)
	}
	if err := runLs(context.Background(), gw, &out, []string{"--policy", "nightly", "--pvc", "apps/db", "/"}); err != nil {
		t.Fatal(err)
	}
	if len(stub.sessions) != 3 {
		t.Fatalf("sessions = %+v", stub.sessions)
	}
	if s := stub.sessions[0]; s.Source != "cos" || s.FromCOS != nil || s.Namespace != "apps" || s.PVC != "db" {
		t.Errorf("policy form = %+v", s)
	}
	if s := stub.sessions[1]; s.Source != "cos" || s.FromCOS == nil || s.FromCOS.Bucket != "my-bucket" || s.Namespace != "" || s.PVC != "" || s.Policy != "lost-policy" {
		t.Errorf("bucket form = %+v", s)
	}
	if s := stub.sessions[2]; s.Source != "" || s.FromCOS != nil {
		t.Errorf("without the flag the gateway must choose: %+v", s)
	}
}

func TestRestoreFilesFromCOS(t *testing.T) {
	fastPoll(t)
	stub, gw := newRestoreStub(t)
	var out bytes.Buffer
	// The policy form.
	err := runRestoreFiles(context.Background(), gw, &out, []string{"--from-cos", "--policy", "nightly", "--pvc", "apps/db",
		"--path", "/etc/app.conf", "--to", "apps/back"})
	if err != nil {
		t.Fatal(err)
	}
	if r := stub.requests[0]; r.Source != "cos" || r.FromCOS != nil || r.Namespace != "apps" {
		t.Errorf("request = %+v", r)
	}
	// The bucket form.
	out.Reset()
	args := []string{"--from-cos", "--policy", "lost-policy", "--bucket", "my-bucket", "--endpoint", "s3.eu-de.cloud-object-storage.appdomain.cloud",
		"--volume", "pvc-11111111-2222-3333-4444-555555555555", "--kp-instance", "i", "--kp-region", "eu-de", "--kp-root-key", "r",
		"--path", "/etc/app.conf", "--to", "apps/back", "--storage-class", "gold"}
	if err = runRestoreFiles(context.Background(), gw, &out, args); err != nil {
		t.Fatal(err)
	}
	r := stub.requests[1]
	if r.FromCOS == nil || r.Source != "cos" || r.Namespace != "" || r.PVC != "" || r.StorageClassName != "gold" || r.Policy != "lost-policy" {
		t.Errorf("request = %+v", r)
	}
	if !strings.Contains(out.String(), "volume pvc-11111111-2222-3333-4444-555555555555 in the COS bucket my-bucket") {
		t.Errorf("output = %q", out.String())
	}
	// A new claim needs a class when the bucket is named.
	noClass := args[:len(args)-2]
	if err = runRestoreFiles(context.Background(), gw, &out, noClass); err == nil || !strings.Contains(err.Error(), "--storage-class") {
		t.Errorf("no class: %v", err)
	}
	// An existing claim does not.
	existing := append(append([]string{}, noClass...), "--existing")
	if err = runRestoreFiles(context.Background(), gw, &out, existing); err != nil {
		t.Errorf("existing claim: %v", err)
	}
}
