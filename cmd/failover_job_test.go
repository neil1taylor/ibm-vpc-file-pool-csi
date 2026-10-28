package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/failover"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/pool"
)

// plainShare is a destination share with one volume that is not a VM disk.
func plainShare(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "pvcs", "pvc-plain")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	meta := `{"subVolumeName":"pvc-plain","spec":{"subPath":"/pvcs/pvc-plain","pvcName":"data","pvcNamespace":"ns","requestedGB":5},"lastSyncTime":"2026-10-20T11:00:00Z","replicationPolicy":"p"}`
	if err := os.WriteFile(filepath.Join(dir, ".subvolume-metadata.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

var jobNow = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func TestFailoverJobMessage_PlanGivesAResultTheControllerCanRead(t *testing.T) {
	msg, err := failoverJobMessage(pool.NewRealNFSOperations(), plainShare(t), "Plan", "", "", false, "", jobNow)
	if err != nil {
		t.Fatal(err)
	}
	out, err := failover.DecodeOutcome(msg)
	if err != nil {
		t.Fatal(err)
	}
	if out.Error != "" || out.Result == nil || len(out.Result.Volumes) != 1 || out.Result.Volumes[0].PVCName != "data" {
		t.Errorf("outcome = %+v", out)
	}
}

func TestFailoverJobMessage_AFailureIsReportedInTheMessage(t *testing.T) {
	for name, tc := range map[string]struct {
		action, rewind, vms, expect, want string
	}{
		"bad action":       {"Destroy", "", "", "", "unknown action"},
		"bad rewind time":  {"Plan", "yesterday", "", "", "rewindTo"},
		"bad expectation":  {"Execute", "", "", "{not json", "expect"},
		"plan has changed": {"Execute", "", "", `{"vms":{},"volumes":["pvc-other"]}`, "no longer matches"},
		"unknown VM":       {"Plan", "", "ns/nope, ns/other", "", "ns/nope"},
		"bad VM name":      {"Plan", "", "not-a-vm", "", "namespace/name"},
	} {
		t.Run(name, func(t *testing.T) {
			msg, err := failoverJobMessage(pool.NewRealNFSOperations(), plainShare(t), tc.action, tc.rewind, tc.vms, false, tc.expect, jobNow)
			if err == nil {
				t.Fatal("expected an error")
			}
			out, derr := failover.DecodeOutcome(msg)
			if derr != nil {
				t.Fatal(derr)
			}
			if out.Result != nil || !strings.Contains(out.Error, tc.want) {
				t.Errorf("outcome = %+v, want an error containing %q", out, tc.want)
			}
		})
	}
}
