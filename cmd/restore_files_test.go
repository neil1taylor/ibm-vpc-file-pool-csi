package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/filerestore"
)

func TestRestoreFilesMainRefusesABadSpec(t *testing.T) {
	log := filepath.Join(t.TempDir(), "termination-log")
	if code := restoreFilesMain("not json", "/paths", log); code != 2 {
		t.Fatalf("exit code %d", code)
	}
	if code := restoreFilesMain(`{"mode":"measure","latest":"/src"}`, "", log); code != 2 {
		t.Fatalf("exit code %d without a paths file", code)
	}
}

func TestRestoreFilesMainWritesTheReasonForAFailure(t *testing.T) {
	log := filepath.Join(t.TempDir(), "termination-log")
	spec := `{"mode":"measure","latest":"/src"}`
	if code := restoreFilesMain(spec, filepath.Join(t.TempDir(), "missing"), log); code != 1 {
		t.Fatalf("exit code %d", code)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	var out filerestore.Outcome
	if err := json.Unmarshal(b, &out); err != nil || out.Error == "" {
		t.Fatalf("termination message %q: %v", b, err)
	}
}
