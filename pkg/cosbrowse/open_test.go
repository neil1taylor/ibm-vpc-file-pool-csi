package cosbrowse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/keyprotect"
)

const (
	testAPIKey  = "ibm-cloud-api-key-value-123"
	testWrapped = "wrapped-key-value-abc"
	testEmerg   = "emergency-readable-key-xyz"
)

type fakeUnwrapper struct {
	err   error
	calls int
	got   string
}

func (u *fakeUnwrapper) Unwrap(_ context.Context, wrapped string) (string, error) {
	u.calls++
	u.got = wrapped
	if u.err != nil {
		return "", u.err
	}
	return testPassword, nil
}

func secretFiles(extra map[string]string) func(string) ([]byte, error) {
	files := map[string]string{
		AccessKeyFile: testAccess + "\n", SecretKeyFile: testSecret + "\n",
		WrappedKeyFile: testWrapped + "\n", APIKeyFile: testAPIKey + "\n",
	}
	for k, v := range extra {
		files[k] = v
	}
	return func(name string) ([]byte, error) {
		v, ok := files[name]
		if !ok {
			return nil, fmt.Errorf("open %s: no such file", name)
		}
		return []byte(v), nil
	}
}

// openWithFake runs Open with a fake Kopia script that has one export.
func openWithFake(t *testing.T, fk *fakeKopia, src Source, read func(string) ([]byte, error), u *fakeUnwrapper, log *bytes.Buffer) (*Opened, error) {
	t.Helper()
	vol := src.Volume
	fk.set(t, "snapshots.json", `[{"id":"s1","source":{"path":"/share0/backup/`+src.Policy+`/pvcs/`+vol+`"},"startTime":"2026-10-22T09:16:00Z",
 "rootEntry":{"obj":"kroot1","mode":"0755","mtime":"2026-10-22T09:00:00Z"},"tags":{"tag:moment":"2026-10-22T09:15:00Z","tag:volume":"`+vol+`"}}]`)
	fk.set(t, "obj-kroot1", `{"stream":"kopia:directory","entries":[{"name":"f","type":"f","mode":"0644","size":1,"mtime":"2026-10-22T09:00:00Z","obj":"kf1"}]}`)
	return Open(context.Background(), src, time.Time{}, OpenOptions{
		Kopia: fk.bin, StateDir: t.TempDir(), ReadFile: read, Log: log,
		Unwrapper: func(keyprotect.Config) (keyprotect.Unwrapper, error) { return u, nil },
	})
}

func TestOpen_KeysReachOnlyWhereTheyBelong(t *testing.T) {
	fk := newFakeKopia(t)
	t.Setenv(sentinelName, sentinelVal)
	u := &fakeUnwrapper{}
	var log bytes.Buffer
	o, err := openWithFake(t, fk, testSource(), secretFiles(nil), u, &log)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = o.Close() }()
	page, err := o.Tree.List(nil, nil, 10)
	if err != nil || len(page.Entries) != 1 {
		t.Fatalf("list = %v, %v", page, err)
	}
	if u.got != testWrapped {
		t.Errorf("Key Protect was given %q", u.got)
	}
	argv, env := fk.read(t, "argv"), fk.read(t, "env")
	for _, secret := range []string{testAccess, testSecret, testPassword, testAPIKey, testWrapped, testEmerg, sentinelVal} {
		if strings.Contains(argv, secret) {
			t.Errorf("a secret is on the command line of Kopia")
		}
		if strings.Contains(log.String(), secret) {
			t.Errorf("a secret is in the log")
		}
	}
	// The child gets the COS key and the backup key, and nothing else secret.
	for _, want := range []string{"KOPIA_PASSWORD=" + testPassword, "AWS_ACCESS_KEY_ID=" + testAccess, "AWS_SECRET_ACCESS_KEY=" + testSecret} {
		if !strings.Contains(env, want) {
			t.Errorf("the child lacks %s", strings.SplitN(want, "=", 2)[0])
		}
	}
	for _, never := range []string{testAPIKey, testWrapped, sentinelVal} {
		if strings.Contains(env, never) {
			t.Errorf("the Kopia child was given a value that it must not see")
		}
	}
}

func TestOpen_KeyProtectRefusalStopsBeforeKopiaAndSaysWhy(t *testing.T) {
	fk := newFakeKopia(t)
	u := &fakeUnwrapper{err: errors.New("Key Protect answered 403: not authorized for this key")}
	_, err := openWithFake(t, fk, testSource(), secretFiles(map[string]string{EmergencyKeyFile: testEmerg}), u, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "backup key could not be unwrapped") || !strings.Contains(err.Error(), "not authorized") {
		t.Fatalf("err = %v", err)
	}
	if fk.read(t, "argv") != "" {
		t.Error("Kopia was run without a key")
	}
	for _, secret := range []string{testAPIKey, testWrapped, testEmerg, testAccess, testSecret} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("a secret is in the error")
		}
	}
}

func TestOpen_EmergencyKeyIsUsedOnlyWhenKeyProtectIsUnreachable(t *testing.T) {
	src := testSource()
	src.EmergencyKey = true
	files := secretFiles(map[string]string{EmergencyKeyFile: testEmerg + "\n"})

	fk := newFakeKopia(t)
	u := &fakeUnwrapper{err: fmt.Errorf("%w: no route", keyprotect.ErrUnreachable)}
	var log bytes.Buffer
	o, err := openWithFake(t, fk, src, files, u, &log)
	if err != nil {
		t.Fatal(err)
	}
	_ = o.Close()
	if env := fk.read(t, "env"); !strings.Contains(env, "KOPIA_PASSWORD="+testEmerg) {
		t.Error("the emergency key was not used when Key Protect was unreachable")
	}
	if !strings.Contains(log.String(), "emergency key") || strings.Contains(log.String(), testEmerg) {
		t.Errorf("log = %q", log.String())
	}

	// A refusal is not hidden by the emergency key.
	fk2 := newFakeKopia(t)
	if _, err := openWithFake(t, fk2, src, files, &fakeUnwrapper{err: errors.New("403 forbidden")}, &bytes.Buffer{}); err == nil {
		t.Error("a refusal from Key Protect was hidden by the emergency key")
	}

	// Unreachable, and no emergency key: a plain sentence.
	fk3 := newFakeKopia(t)
	_, err = openWithFake(t, fk3, testSource(), secretFiles(nil), &fakeUnwrapper{err: fmt.Errorf("%w: no route", keyprotect.ErrUnreachable)}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "no emergency key") {
		t.Errorf("err = %v", err)
	}
}

func TestOpen_EmergencyOnlyNeverCallsKeyProtect(t *testing.T) {
	src := testSource()
	src.KeyProtect, src.EmergencyKey, src.EmergencyOnly = nil, true, true
	fk := newFakeKopia(t)
	u := &fakeUnwrapper{}
	files := secretFiles(map[string]string{EmergencyKeyFile: testEmerg})
	o, err := openWithFake(t, fk, src, files, u, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	_ = o.Close()
	if u.calls != 0 {
		t.Error("Key Protect was called")
	}
	if !strings.Contains(fk.read(t, "env"), "KOPIA_PASSWORD="+testEmerg) {
		t.Error("the emergency key was not used")
	}
}

func TestOpen_AMissingSecretIsSaidInWords(t *testing.T) {
	for _, missing := range []string{AccessKeyFile, SecretKeyFile, WrappedKeyFile, APIKeyFile} {
		fk := newFakeKopia(t)
		read := secretFiles(nil)
		_, err := openWithFake(t, fk, testSource(), func(name string) ([]byte, error) {
			if name == missing {
				return nil, errors.New("open " + name + ": no such file or directory")
			}
			return read(name)
		}, &fakeUnwrapper{}, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "the Secret is not mounted or is missing its key") {
			t.Errorf("%s: err = %v", missing, err)
		}
		if err != nil && strings.Contains(err.Error(), "/var/run") {
			t.Errorf("%s: the path is in the error: %v", missing, err)
		}
	}
}

func TestOpen_COSFailuresAreSaidInWords(t *testing.T) {
	for _, c := range []struct{ stderr, want string }{
		{"AccessDenied", "COS refused the key"},
		{"invalid repository password", "backup key does not open"},
		{"dial tcp: lookup s3.x: no such host", "COS cannot be reached"},
	} {
		fk := newFakeKopia(t)
		fk.set(t, "stderr", c.stderr)
		fk.set(t, "exit", "1")
		_, err := openWithFake(t, fk, testSource(), secretFiles(nil), &fakeUnwrapper{}, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: err = %v", c.stderr, err)
		}
	}
}

func TestSourceValidate(t *testing.T) {
	good := testSource()
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*Source){
		"bad endpoint":       func(s *Source) { s.Endpoint = "s3.x; ls" },
		"bad bucket":         func(s *Source) { s.Bucket = "UPPER" },
		"traversal prefix":   func(s *Source) { s.Prefix = "../other/" },
		"prefix with space":  func(s *Source) { s.Prefix = "a b/" },
		"bad policy":         func(s *Source) { s.Policy = "../x" },
		"bad volume":         func(s *Source) { s.Volume = "pvc-1" },
		"no key protect":     func(s *Source) { s.KeyProtect = nil },
		"bad region":         func(s *Source) { s.KeyProtect.Region = "EU DE" },
		"emergency-only key": func(s *Source) { s.EmergencyOnly = true },
	} {
		s := testSource()
		kp := *s.KeyProtect
		s.KeyProtect = &kp
		mut(&s)
		if err := s.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
