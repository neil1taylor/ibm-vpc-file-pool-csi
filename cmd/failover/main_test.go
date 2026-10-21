package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/failover"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/qcow2"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/replication"
)

func TestParseFlags_BooleanFlagsNeedNoValue(t *testing.T) {
	f := parseFlags([]string{"--list-moments", "--nfs-mount-path", "/mnt", "--force", "--dry-run", "--rewind-to=2026-10-20T10:00:00Z"})
	for _, k := range []string{"list-moments", "force", "dry-run"} {
		if f[k] != "true" {
			t.Errorf("flag %s = %q, want true", k, f[k])
		}
	}
	if f["nfs-mount-path"] != "/mnt" || f["rewind-to"] != "2026-10-20T10:00:00Z" {
		t.Errorf("flags = %v", f)
	}
	// A boolean flag must not swallow the next flag's value.
	f = parseFlags([]string{"--force", "/mnt/x"})
	if f["force"] != "true" {
		t.Errorf("force = %q", f["force"])
	}
}

func TestVMOptionsFromFlags(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	o, err := vmOptionsFromFlags(map[string]string{"rewind-to": "2026-10-20T10:30:00Z", "force": "true"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if o.RewindTo == nil || !o.RewindTo.Equal(time.Date(2026, 10, 20, 10, 30, 0, 0, time.UTC)) || !o.Force || !o.Now.Equal(now) {
		t.Errorf("options = %+v", o)
	}
	if o, err = vmOptionsFromFlags(map[string]string{}, now); err != nil || o.RewindTo != nil || o.Force {
		t.Errorf("empty flags: %+v %v", o, err)
	}
	if _, err = vmOptionsFromFlags(map[string]string{"rewind-to": "yesterday"}, now); err == nil {
		t.Error("expected an error for a bad --rewind-to")
	}
}

func TestPrintVMPlan(t *testing.T) {
	m1 := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	m2 := m1.Add(time.Hour)
	plan := &failover.FailoverPlan{SubVolumes: []failover.FailoverSubVolumePlan{
		{SubVolumeName: "pvc-a", VMGroup: "ns/vm", VMMoment: m1, Rewind: true},
		{SubVolumeName: "pvc-b", VMGroup: "ns/vm", VMMoment: m1},
		{SubVolumeName: "pvc-files"},
	}}
	var out bytes.Buffer
	printVMPlan(&out, plan)
	got := out.String()
	for _, want := range []string{"ns/vm", "2026-10-20T10:00:00Z", "pvc-a", "rewind", "pvc-b", "in place"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "pvc-files") {
		t.Errorf("a plain volume must not be listed:\n%s", got)
	}
	out.Reset()
	printMoments(&out, map[string][]time.Time{"ns/vm": {m2, m1}})
	if !strings.Contains(out.String(), "2026-10-20T11:00:00Z") || strings.Index(out.String(), "11:00:00") > strings.Index(out.String(), "10:00:00") {
		t.Errorf("moments not listed newest first:\n%s", out.String())
	}
}

// testCopy is a full copy held in memory.
type testCopy struct{ data []byte }

func (m testCopy) Size() int64 { return int64(len(m.data)) }
func (m testCopy) Walk(fn func(qcow2.Extent) error) error {
	return fn(qcow2.Extent{Offset: 0, Length: int64(len(m.data)), Kind: qcow2.Data})
}
func (m testCopy) DataReader(qcow2.Extent) io.Reader { return bytes.NewReader(m.data) }

func TestExecute_PrepareOnlyRewindsWithoutACluster(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "pvcs", "pvc-a")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	meta := `{"subVolumeName":"pvc-a","spec":{"subPath":"/pvcs/pvc-a","pvcName":"a","pvcNamespace":"ns","requestedGB":1},"lastSyncTime":"2026-10-20T11:00:00Z","replicationPolicy":"p"}`
	if err := os.WriteFile(filepath.Join(dir, ".subvolume-metadata.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	m1 := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	m2 := m1.Add(time.Hour)
	disk := filepath.Join(dir, "disk.img")
	old, now := bytes.Repeat([]byte{1}, 4096), bytes.Repeat([]byte{2}, 4096)
	if _, err := replication.ApplyCopy(testCopy{old}, disk, replication.ApplyOptions{To: m1, Full: true, Now: m1}); err != nil {
		t.Fatal(err)
	}
	if _, err := replication.ApplyCopy(testCopy{now}, disk, replication.ApplyOptions{From: m1, To: m2, Full: true, Now: m2}); err != nil {
		t.Fatal(err)
	}
	if err := replication.WriteMoment(dir, m2, "ns/vm"); err != nil {
		t.Fatal(err)
	}

	err := runExecute([]string{"--nfs-mount-path", root, "--prepare-only", "--rewind-to", m1.Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir+".rewind-"+strconv.FormatInt(m1.Unix(), 10), "disk.img"))
	if err != nil || !bytes.Equal(got, old) {
		t.Fatalf("rewound disk: %v, matches the old data: %v", err, bytes.Equal(got, old))
	}
	if latest, _ := os.ReadFile(disk); !bytes.Equal(latest, now) {
		t.Error("the latest disk was changed")
	}
}
