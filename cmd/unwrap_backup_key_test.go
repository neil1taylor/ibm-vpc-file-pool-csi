package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/keyprotect"
)

type recordingUnwrapper struct{ key string }

func (r recordingUnwrapper) Unwrap(context.Context, string) (string, error) { return r.key, nil }

func writeTemp(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestUnwrapBackupKey_BuildsTheClientFromTheFlagsAndTheAPIKeyFileAndWritesTheKey(t *testing.T) {
	dir := t.TempDir()
	opts := unwrapOptions{
		region: "eu-de", instance: "inst-1", rootKey: "root-1",
		wrappedKeyFile: writeTemp(t, dir, "wrapped", "wrapped-key"),
		apiKeyFile:     writeTemp(t, dir, "apikey", "the-api-key\n"),
		keyOut:         filepath.Join(dir, "key"),
	}
	var got keyprotect.Config
	factory := func(cfg keyprotect.Config) (keyprotect.Unwrapper, error) {
		got = cfg
		return recordingUnwrapper{key: "READABLE-KEY"}, nil
	}
	var log bytes.Buffer
	if err := unwrapBackupKey(context.Background(), opts, factory, &log); err != nil {
		t.Fatal(err)
	}
	if got != (keyprotect.Config{Region: "eu-de", InstanceID: "inst-1", RootKeyID: "root-1", APIKey: "the-api-key"}) {
		t.Errorf("config = %+v", got)
	}
	b, _ := os.ReadFile(opts.keyOut)
	if string(b) != "READABLE-KEY" {
		t.Errorf("key file = %q", b)
	}
	if st, _ := os.Stat(opts.keyOut); st.Mode().Perm() != 0o400 {
		t.Errorf("mode = %v, want 0400", st.Mode().Perm())
	}
	if strings.Contains(log.String(), "READABLE-KEY") || strings.Contains(log.String(), "the-api-key") {
		t.Errorf("the log holds a secret: %q", log.String())
	}
}

func TestUnwrapBackupKey_RequiresItsFlags(t *testing.T) {
	dir := t.TempDir()
	full := unwrapOptions{
		region: "eu-de", instance: "i", rootKey: "r",
		wrappedKeyFile: writeTemp(t, dir, "w", "w"), apiKeyFile: writeTemp(t, dir, "a", "a"), keyOut: filepath.Join(dir, "k"),
	}
	factory := func(keyprotect.Config) (keyprotect.Unwrapper, error) { return recordingUnwrapper{key: "k"}, nil }
	for name, mutate := range map[string]func(*unwrapOptions){
		"region":  func(o *unwrapOptions) { o.region = "" },
		"wrapped": func(o *unwrapOptions) { o.wrappedKeyFile = "" },
		"api key": func(o *unwrapOptions) { o.apiKeyFile = "" },
		"out":     func(o *unwrapOptions) { o.keyOut = "" },
	} {
		o := full
		o.keyOut = filepath.Join(dir, "k-"+strings.ReplaceAll(name, " ", "-"))
		mutate(&o)
		if err := unwrapBackupKey(context.Background(), o, factory, &bytes.Buffer{}); err == nil {
			t.Errorf("without %s: no error", name)
		}
	}
}
