package replication

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSendVMDiskFile_SendsThenDeletesTheStagingFile(t *testing.T) {
	c := newVMTestClient(t)
	staging := filepath.Join(t.TempDir(), ".repl-staging-a1.qcow2")
	copyTestImage(t, "v3.qcow2", staging)

	res, err := SendVMDiskFile(context.Background(), c.sync(), staging, c.args("copy-one", nil, true, nil, t1))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readDisk(t, c.diskPath()), v3Contents()) {
		t.Error("far-site disk does not match the staging file")
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Errorf("staging file should be deleted, stat err = %v", err)
	}
	if res.SentBytes == 0 {
		t.Error("expected some bytes sent")
	}
}

func TestSendVMDiskFile_KeepsTheStagingFileOnError(t *testing.T) {
	c := newVMTestClient(t)
	staging := filepath.Join(t.TempDir(), ".repl-staging-a1.qcow2")
	copyTestImage(t, "v3.qcow2", staging)
	bad := NewSyncClient(c.srv.URL, "wrong")
	if _, err := SendVMDiskFile(context.Background(), bad, staging, c.args("copy-one", nil, true, nil, t1)); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := os.Stat(staging); err != nil {
		t.Errorf("staging file should be kept: %v", err)
	}
}

func TestSendVMDiskFile_KeepsTheStagingFileOnWrongBase(t *testing.T) {
	c := newVMTestClient(t)
	staging := filepath.Join(t.TempDir(), ".repl-staging-a1.qcow2")
	copyTestImage(t, "v3.qcow2", staging)
	// There is no far-site disk, so a change copy cannot be applied.
	_, err := SendVMDiskFile(context.Background(), c.sync(), staging, c.args("copy-one", nil, false, ptr(t0), t1))
	var wb *WrongBaseError
	if !errors.As(err, &wb) {
		t.Fatalf("error = %v, want WrongBaseError", err)
	}
	if _, err := os.Stat(staging); err != nil {
		t.Errorf("staging file should be kept: %v", err)
	}
}

func TestSendVMDiskFile_RefusesOtherFiles(t *testing.T) {
	c := newVMTestClient(t)
	other := filepath.Join(t.TempDir(), "important.img")
	if err := os.WriteFile(other, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SendVMDiskFile(context.Background(), c.sync(), other, c.args("copy-one", nil, true, nil, t1)); err == nil {
		t.Fatal("expected a refusal")
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("the file must not be touched")
	}
}
