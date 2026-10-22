package keyprotect

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestUnwrapToFile_WritesOnlyTheKeyWithMode0400(t *testing.T) {
	f := newFakeKeyProtect(t)
	wrapped, _ := f.client(testRootKey).Wrap(context.Background(), testBackupKey)
	dir := t.TempDir()
	opts := FileOptions{WrappedKeyFile: writeFile(t, dir, "wrappedKey", wrapped+"\n"), OutFile: filepath.Join(dir, "key")}
	var log bytes.Buffer
	if err := UnwrapToFile(context.Background(), f.client(testRootKey), opts, &log); err != nil {
		t.Fatalf("UnwrapToFile: %v", err)
	}
	got, err := os.ReadFile(opts.OutFile)
	if err != nil || string(got) != testBackupKey {
		t.Fatalf("key file = %q, %v; want exactly the key", got, err)
	}
	st, _ := os.Stat(opts.OutFile)
	if st.Mode().Perm() != 0o400 {
		t.Errorf("key file mode = %v, want 0400", st.Mode().Perm())
	}
	if strings.Contains(log.String(), testBackupKey) || strings.Contains(log.String(), wrapped) {
		t.Errorf("the log holds a key: %q", log.String())
	}
	if strings.Contains(log.String(), base64.StdEncoding.EncodeToString([]byte(testBackupKey))) {
		t.Errorf("the log holds the encoded key")
	}
}

func TestUnwrapToFile_WrongRootKeyWritesNothing(t *testing.T) {
	f := newFakeKeyProtect(t)
	wrapped, _ := f.client(testRootKey).Wrap(context.Background(), testBackupKey)
	dir := t.TempDir()
	opts := FileOptions{WrappedKeyFile: writeFile(t, dir, "wrappedKey", wrapped), OutFile: filepath.Join(dir, "key")}
	if err := UnwrapToFile(context.Background(), f.client("99999999-9999-9999-9999-999999999999"), opts, &bytes.Buffer{}); err == nil {
		t.Fatal("a wrong root key worked")
	}
	if _, err := os.Stat(opts.OutFile); err == nil {
		t.Error("a key file exists after a failure")
	}
}

func TestUnwrapToFile_KeyProtectDownUsesTheEmergencyKey(t *testing.T) {
	f := newFakeKeyProtect(t)
	c := f.client(testRootKey)
	wrapped, _ := c.Wrap(context.Background(), testBackupKey)
	f.kms.Close()
	dir := t.TempDir()
	opts := FileOptions{
		WrappedKeyFile:   writeFile(t, dir, "wrappedKey", wrapped),
		EmergencyKeyFile: writeFile(t, dir, "emergency", testBackupKey+"\n"),
		OutFile:          filepath.Join(dir, "key"),
	}
	var log bytes.Buffer
	if err := UnwrapToFile(context.Background(), c, opts, &log); err != nil {
		t.Fatalf("UnwrapToFile: %v", err)
	}
	got, _ := os.ReadFile(opts.OutFile)
	if string(got) != testBackupKey {
		t.Errorf("key file = %q; want the emergency key", got)
	}
	if !strings.Contains(log.String(), "emergency") || strings.Contains(log.String(), testBackupKey) {
		t.Errorf("the log must say the emergency key was used, and must not hold it: %q", log.String())
	}
}

func TestUnwrapToFile_KeyProtectDownWithoutEmergencyKeyFailsClearly(t *testing.T) {
	f := newFakeKeyProtect(t)
	c := f.client(testRootKey)
	f.kms.Close()
	dir := t.TempDir()
	opts := FileOptions{WrappedKeyFile: writeFile(t, dir, "wrappedKey", "wrapped.x"), OutFile: filepath.Join(dir, "key")}
	err := UnwrapToFile(context.Background(), c, opts, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "emergency") {
		t.Fatalf("err = %v; want a clear message about the missing emergency key", err)
	}
	if _, statErr := os.Stat(opts.OutFile); statErr == nil {
		t.Error("a key file exists after a failure")
	}
}

func TestUnwrapToFile_WillNotOverwriteAFile(t *testing.T) {
	dir := t.TempDir()
	out := writeFile(t, dir, "key", "old")
	opts := FileOptions{WrappedKeyFile: writeFile(t, dir, "wrappedKey", "w"), EmergencyKeyFile: writeFile(t, dir, "e", "k"), OutFile: out}
	if err := UnwrapToFile(context.Background(), stubUnwrapper{key: "k"}, opts, &bytes.Buffer{}); err == nil {
		t.Error("an existing file was overwritten")
	}
}
