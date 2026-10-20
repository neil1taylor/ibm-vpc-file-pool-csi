package failover

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/qcow2"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/replication"
)

const vmChunk = 64 << 10

var (
	m1 = time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	m2 = m1.Add(time.Hour)
	m3 = m1.Add(2 * time.Hour)
)

// memCopy is a full copy held in memory.
type memCopy struct{ data []byte }

func (m memCopy) Size() int64 { return int64(len(m.data)) }
func (m memCopy) Walk(fn func(qcow2.Extent) error) error {
	for off := 0; off < len(m.data); off += vmChunk {
		if err := fn(qcow2.Extent{Offset: int64(off), Length: vmChunk, Kind: qcow2.Data}); err != nil {
			return err
		}
	}
	return nil
}
func (m memCopy) DataReader(e qcow2.Extent) io.Reader {
	return bytes.NewReader(m.data[e.Offset : e.Offset+e.Length])
}

// content is a two-chunk disk filled with b.
func content(b byte) []byte { return bytes.Repeat([]byte{b}, 2*vmChunk) }

// vmDisk makes a far-site disk folder under root: one full copy per step, in
// order, each with its moment. It returns the plan entry for it.
type step struct {
	at   time.Time
	fill byte
}

func vmDisk(t *testing.T, root, name, group string, steps ...step) FailoverSubVolumePlan {
	t.Helper()
	dir := filepath.Join(root, "pvcs", name)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	disk := filepath.Join(dir, "disk.img")
	var prev time.Time
	for i, s := range steps {
		opts := replication.ApplyOptions{To: s.at, Full: true, Now: s.at}
		if i > 0 {
			opts.From = prev
		}
		if _, err := replication.ApplyCopy(memCopy{content(s.fill)}, disk, opts); err != nil {
			t.Fatal(err)
		}
		if err := replication.WriteMoment(dir, s.at, group); err != nil {
			t.Fatal(err)
		}
		prev = s.at
	}
	return FailoverSubVolumePlan{SubVolumeName: name, PVCName: name, PVCNamespace: "ns", SubPath: "/pvcs/" + name, LocalDir: dir}
}

func resolve(t *testing.T, subs []FailoverSubVolumePlan, opts VMOptions) (*FailoverPlan, error) {
	t.Helper()
	plan := &FailoverPlan{SubVolumes: subs}
	return plan, plan.ResolveVMs(opts)
}

func byName(plan *FailoverPlan, name string) FailoverSubVolumePlan {
	for _, sv := range plan.SubVolumes {
		if sv.SubVolumeName == name {
			return sv
		}
	}
	return FailoverSubVolumePlan{}
}

func TestResolveVMs_DisksAtTheSameMoment(t *testing.T) {
	root := t.TempDir()
	a := vmDisk(t, root, "pvc-a", "ns/vm", step{m1, 1}, step{m2, 2})
	b := vmDisk(t, root, "pvc-b", "ns/vm", step{m1, 1}, step{m2, 2})
	plan, err := resolve(t, []FailoverSubVolumePlan{a, b}, VMOptions{Now: m3})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pvc-a", "pvc-b"} {
		sv := byName(plan, name)
		if !sv.VMMoment.Equal(m2) || sv.Rewind || sv.VMGroup != "ns/vm" {
			t.Errorf("%s: %+v, want moment %v, no rewind", name, sv, m2)
		}
	}
}

func TestResolveVMs_DisksAtDifferentMomentsUseTheLatestCommonOne(t *testing.T) {
	root := t.TempDir()
	a := vmDisk(t, root, "pvc-a", "ns/vm", step{m1, 1}, step{m2, 2}) // the commit reached this disk
	b := vmDisk(t, root, "pvc-b", "ns/vm", step{m1, 1})              // and not this one
	plan, err := resolve(t, []FailoverSubVolumePlan{a, b}, VMOptions{Now: m3})
	if err != nil {
		t.Fatal(err)
	}
	if sa := byName(plan, "pvc-a"); !sa.VMMoment.Equal(m1) || !sa.Rewind {
		t.Errorf("pvc-a: %+v, want a rewind to %v", sa, m1)
	}
	if sb := byName(plan, "pvc-b"); !sb.VMMoment.Equal(m1) || sb.Rewind {
		t.Errorf("pvc-b: %+v, want it used in place at %v", sb, m1)
	}
	if plan.RPO != m3.Sub(m1) {
		t.Errorf("RPO = %v, want %v (the age of the chosen moment)", plan.RPO, m3.Sub(m1))
	}

	if err := plan.MaterializeVMs(); err != nil {
		t.Fatal(err)
	}
	sa := byName(plan, "pvc-a")
	if !strings.HasPrefix(sa.SubPath, "/pvcs/pvc-a.rewind-") {
		t.Fatalf("rewound SubPath = %q", sa.SubPath)
	}
	got, err := os.ReadFile(filepath.Join(root, sa.SubPath, "disk.img"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content(1)) {
		t.Error("rewound disk does not hold the data of the first moment")
	}
	if fi, _ := os.Stat(filepath.Join(root, sa.SubPath, "disk.img")); fi.Mode().Perm()&0o006 != 0o006 {
		t.Errorf("rewound disk mode = %v, want it usable by the QEMU user", fi.Mode().Perm())
	}
	// The latest disk is not changed.
	latest, _ := os.ReadFile(filepath.Join(a.LocalDir, "disk.img"))
	if !bytes.Equal(latest, content(2)) {
		t.Error("the latest disk was changed by the rewind")
	}
	if byName(plan, "pvc-b").SubPath != "/pvcs/pvc-b" {
		t.Error("an in-place disk must keep its folder")
	}

	// Running it again reuses the rewound disk.
	again := &FailoverPlan{SubVolumes: []FailoverSubVolumePlan{a, b}}
	if err := again.ResolveVMs(VMOptions{Now: m3}); err != nil {
		t.Fatal(err)
	}
	if err := again.MaterializeVMs(); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if byName(again, "pvc-a").SubPath != sa.SubPath {
		t.Error("second run chose another folder")
	}
}

func TestResolveVMs_RewindTo(t *testing.T) {
	root := t.TempDir()
	a := vmDisk(t, root, "pvc-a", "ns/vm", step{m1, 1}, step{m2, 2}, step{m3, 3})
	b := vmDisk(t, root, "pvc-b", "ns/vm", step{m1, 1}, step{m2, 2}, step{m3, 3})
	to := m2.Add(10 * time.Minute) // between two moments: the newest one before it
	plan, err := resolve(t, []FailoverSubVolumePlan{a, b}, VMOptions{RewindTo: &to, Now: m3})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pvc-a", "pvc-b"} {
		if sv := byName(plan, name); !sv.VMMoment.Equal(m2) || !sv.Rewind {
			t.Errorf("%s: %+v, want a rewind to %v", name, sv, m2)
		}
	}
	early := m1.Add(-time.Hour)
	if _, err = resolve(t, []FailoverSubVolumePlan{a, b}, VMOptions{RewindTo: &early, Now: m3}); err == nil {
		t.Error("expected an error when no moment is old enough")
	}
}

func TestResolveVMs_NoCommonMoment(t *testing.T) {
	root := t.TempDir()
	a := vmDisk(t, root, "pvc-a", "ns/vm", step{m1, 1}, step{m2, 2})
	b := vmDisk(t, root, "pvc-b", "ns/vm", step{m3, 3})
	_, err := resolve(t, []FailoverSubVolumePlan{a, b}, VMOptions{Now: m3})
	if err == nil || !strings.Contains(err.Error(), "pvc-a") || !strings.Contains(err.Error(), "pvc-b") {
		t.Fatalf("error = %v, want one naming both disks", err)
	}
}

func TestResolveVMs_DisksOfDifferentVMsAreIndependent(t *testing.T) {
	root := t.TempDir()
	a := vmDisk(t, root, "pvc-a", "ns/vm1", step{m1, 1}, step{m2, 2})
	b := vmDisk(t, root, "pvc-b", "ns/vm2", step{m1, 1})
	plan, err := resolve(t, []FailoverSubVolumePlan{a, b}, VMOptions{Now: m3})
	if err != nil {
		t.Fatal(err)
	}
	if sa := byName(plan, "pvc-a"); !sa.VMMoment.Equal(m2) || sa.Rewind {
		t.Errorf("pvc-a: %+v", sa)
	}
}

func TestResolveVMs_RunningCommitBlocksUnlessForced(t *testing.T) {
	root := t.TempDir()
	a := vmDisk(t, root, "pvc-a", "ns/vm", step{m1, 1})
	if err := os.WriteFile(filepath.Join(a.LocalDir, ".repl-lease"), []byte(m3.Add(time.Hour).Format(time.RFC3339Nano)), 0o660); err != nil {
		t.Fatal(err)
	}
	if _, err := resolve(t, []FailoverSubVolumePlan{a}, VMOptions{Now: m3}); err == nil || !strings.Contains(err.Error(), "pvc-a") {
		t.Fatalf("error = %v, want a refusal that names the disk", err)
	}
	if _, err := resolve(t, []FailoverSubVolumePlan{a}, VMOptions{Now: m3, Force: true}); err != nil {
		t.Fatalf("with Force: %v", err)
	}
	if _, err := resolve(t, []FailoverSubVolumePlan{a}, VMOptions{Now: m3.Add(3 * time.Hour)}); err != nil {
		t.Fatalf("expired lease: %v", err)
	}
}

func TestResolveVMs_IgnoresVolumesThatAreNotVMDisks(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "pvcs", "pvc-files")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sv := FailoverSubVolumePlan{SubVolumeName: "pvc-files", SubPath: "/pvcs/pvc-files", LocalDir: dir}
	plan, err := resolve(t, []FailoverSubVolumePlan{sv}, VMOptions{Now: m3})
	if err != nil {
		t.Fatal(err)
	}
	if got := byName(plan, "pvc-files"); got.VMGroup != "" || got.Rewind || !got.VMMoment.IsZero() {
		t.Errorf("a plain volume was changed: %+v", got)
	}
	if err := plan.MaterializeVMs(); err != nil {
		t.Fatal(err)
	}
}

func TestResolveVMs_UnknownMoment(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "pvcs", "pvc-a")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "disk.img"), content(1), 0o660); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".repl-moment"), []byte(`{"moment":"0001-01-01T00:00:00Z","vmGroup":"ns/vm"}`), 0o660); err != nil {
		t.Fatal(err)
	}
	sv := FailoverSubVolumePlan{SubVolumeName: "pvc-a", SubPath: "/pvcs/pvc-a", LocalDir: dir}
	b := vmDisk(t, root, "pvc-b", "ns/vm", step{m1, 1})
	if _, err := resolve(t, []FailoverSubVolumePlan{sv, b}, VMOptions{Now: m3}); err == nil {
		t.Error("expected an error: one disk of the VM has no known moment")
	}
}

func TestCommonMoments(t *testing.T) {
	root := t.TempDir()
	a := vmDisk(t, root, "pvc-a", "ns/vm", step{m1, 1}, step{m2, 2}, step{m3, 3})
	b := vmDisk(t, root, "pvc-b", "ns/vm", step{m1, 1}, step{m2, 2})
	plan := &FailoverPlan{SubVolumes: []FailoverSubVolumePlan{a, b}}
	got, err := plan.CommonMoments(VMOptions{Now: m3})
	if err != nil {
		t.Fatal(err)
	}
	moments := got["ns/vm"]
	if len(moments) != 2 || !moments[0].Equal(m2) || !moments[1].Equal(m1) {
		t.Errorf("common moments = %v, want [%v %v] (newest first)", moments, m2, m1)
	}
}
