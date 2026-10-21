package vmsnap

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/vmcopy"
)

const dir = "/var/run/kubevirt-private/vmi-disks"

// fakeLauncher simulates virsh and the file commands in a virt-launcher
// compute container. Each PVC disk has a chain of files, newest first.
type fakeLauncher struct {
	mu sync.Mutex

	chains  map[string][]string // volume -> file names, newest first
	targets map[string]string   // volume -> target dev
	files   map[string]string   // path -> mode ("0644", "0444")
	backing map[string]string   // path -> backing file name, as written in the file

	quiesceErr  string // snapshot-create-as --quiesce fails with this
	snapshotErr string // snapshot-create-as fails with this
	commitErr   string // blockcommit fails with this
	log         []string
}

func newFakeLauncher() *fakeLauncher {
	f := &fakeLauncher{
		chains:  map[string][]string{"rootdisk": {"disk.img"}, "data": {"disk.img"}},
		targets: map[string]string{"rootdisk": "vda", "data": "vdb"},
		files:   map[string]string{},
		backing: map[string]string{},
	}
	f.files[dir+"/rootdisk/disk.img"] = "0644"
	f.files[dir+"/data/disk.img"] = "0644"
	return f
}

var testVM = vmcopy.VM{Namespace: "vms", Pod: "virt-launcher-web-abcde", Name: "web"}

func (f *fakeLauncher) Exec(_ context.Context, ns, pod, container string, cmd []string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ns != "vms" || pod != "virt-launcher-web-abcde" || container != "compute" {
		return "", "", fmt.Errorf("unexpected target %s/%s/%s", ns, pod, container)
	}
	f.log = append(f.log, strings.Join(cmd, " "))
	fail := func(msg string) (string, string, error) {
		return "", msg, errors.New("command terminated with exit code 1")
	}
	switch cmd[0] {
	case "virsh":
		return f.virsh(cmd[1:], fail)
	case "qemu-img":
		// qemu-img create -q -f qcow2 -F <fmt> -b <backing> <path>
		if len(cmd) != 10 || cmd[1] != "create" || cmd[2] != "-q" || cmd[4] != "qcow2" || cmd[5] != "-F" || cmd[7] != "-b" {
			return "", "", fmt.Errorf("bad qemu-img args %v", cmd)
		}
		if strings.Contains(cmd[8], "/") {
			return "", "", fmt.Errorf("backing file %q must be named relative to the new file", cmd[8])
		}
		if _, ok := f.files[path.Join(path.Dir(cmd[9]), cmd[8])]; !ok {
			return fail("Could not open backing file")
		}
		f.files[cmd[9]] = "0644"
		f.backing[cmd[9]] = cmd[8]
		return "", "", nil
	case "chmod":
		// chmod <mode> <path>
		if len(cmd) != 3 {
			return "", "", fmt.Errorf("bad chmod args %v", cmd)
		}
		if _, ok := f.files[cmd[2]]; !ok {
			return fail("No such file or directory")
		}
		f.files[cmd[2]] = cmd[1]
		return "", "", nil
	case "rm":
		// rm -f -- <path>
		if len(cmd) != 4 || cmd[1] != "-f" || cmd[2] != "--" {
			return "", "", fmt.Errorf("bad rm args %v", cmd)
		}
		delete(f.files, cmd[3])
		return "", "", nil
	}
	return "", "", fmt.Errorf("unknown command %v", cmd)
}

func (f *fakeLauncher) virsh(args []string, fail func(string) (string, string, error)) (string, string, error) {
	switch args[0] {
	case "dumpxml":
		if len(args) != 2 || args[1] != "vms_web" {
			return "", "", fmt.Errorf("bad dumpxml args %v", args)
		}
		return f.domainXML(), "", nil
	case "snapshot-create-as":
		return f.snapshot(args, fail)
	case "blockcommit":
		return f.commit(args, fail)
	}
	return "", "", fmt.Errorf("unknown virsh command %v", args)
}

func (f *fakeLauncher) volumes() []string {
	var v []string
	for vol := range f.chains {
		v = append(v, vol)
	}
	sort.Strings(v)
	return v
}

func formatOf(name string) string {
	if strings.HasSuffix(name, ".qcow2") {
		return "qcow2"
	}
	return "raw"
}

// domainXML is a trimmed copy of what libvirt prints in a KubeVirt pod.
func (f *fakeLauncher) domainXML() string {
	var b strings.Builder
	b.WriteString("<domain type='kvm'>\n  <name>vms_web</name>\n  <devices>\n")
	for _, vol := range f.volumes() {
		chain := f.chains[vol]
		fmt.Fprintf(&b, "    <disk type='file' device='disk' model='virtio-non-transitional'>\n")
		fmt.Fprintf(&b, "      <driver name='qemu' type='%s' cache='none' error_policy='stop' discard='unmap'/>\n", formatOf(chain[0]))
		fmt.Fprintf(&b, "      <source file='%s/%s/%s' index='%d'/>\n", dir, vol, chain[0], 10+len(chain))
		for i, name := range chain[1:] {
			fmt.Fprintf(&b, "      <backingStore type='file' index='%d'>\n        <format type='%s'/>\n        <source file='%s/%s/%s'/>\n", i+1, formatOf(name), dir, vol, name)
		}
		b.WriteString("      <backingStore/>\n")
		for range chain[1:] {
			b.WriteString("      </backingStore>\n")
		}
		fmt.Fprintf(&b, "      <target dev='%s' bus='virtio'/>\n      <alias name='ua-%s'/>\n    </disk>\n", f.targets[vol], vol)
	}
	b.WriteString("    <disk type='file' device='disk'>\n      <driver name='qemu' type='raw'/>\n      <source file='/var/run/kubevirt-ephemeral-disks/cloud-init-data/vms/web/noCloud.iso'/>\n      <target dev='vdc' bus='virtio'/>\n    </disk>\n")
	b.WriteString("  </devices>\n</domain>\n")
	return b.String()
}

func (f *fakeLauncher) volumeOfTarget(t string) string {
	for vol, tt := range f.targets {
		if tt == t {
			return vol
		}
	}
	return ""
}

func (f *fakeLauncher) snapshot(args []string, fail func(string) (string, string, error)) (string, string, error) {
	if args[1] != "vms_web" {
		return "", "", fmt.Errorf("bad domain %v", args)
	}
	for _, flag := range []string{"--disk-only", "--atomic", "--no-metadata", "--reuse-external"} {
		if !contains(args, flag) {
			return "", "", fmt.Errorf("snapshot-create-as without %s: %v", flag, args)
		}
	}
	if contains(args, "--quiesce") && f.quiesceErr != "" {
		return fail(f.quiesceErr)
	}
	if f.snapshotErr != "" {
		return fail(f.snapshotErr)
	}
	specs := map[string]string{}
	for i, a := range args {
		if a == "--diskspec" {
			t, rest, _ := strings.Cut(args[i+1], ",")
			specs[t] = rest
		}
	}
	for _, t := range []string{"vda", "vdb", "vdc"} {
		if _, ok := specs[t]; !ok {
			return "", "", fmt.Errorf("no --diskspec for %s: libvirt would snapshot it too", t)
		}
	}
	for t, spec := range specs {
		if spec == "snapshot=no" {
			continue
		}
		file, ok := strings.CutPrefix(spec, "file=")
		if !ok {
			return "", "", fmt.Errorf("bad diskspec %q", spec)
		}
		vol := f.volumeOfTarget(t)
		if path.Dir(file) != dir+"/"+vol {
			return "", "", fmt.Errorf("new file %s is not in the folder of %s", file, vol)
		}
		if _, exists := f.files[file]; !exists {
			return fail("--reuse-external: file does not exist")
		}
		if f.backing[file] != f.chains[vol][0] {
			return "", "", fmt.Errorf("new file %s backs onto %q, want the top file %q", file, f.backing[file], f.chains[vol][0])
		}
		f.chains[vol] = append([]string{path.Base(file)}, f.chains[vol]...)
	}
	return "Domain snapshot " + args[2] + " created\n", "", nil
}

func (f *fakeLauncher) commit(args []string, fail func(string) (string, string, error)) (string, string, error) {
	if args[1] != "vms_web" || !contains(args, "--keep-relative") || !contains(args, "--wait") {
		return "", "", fmt.Errorf("bad blockcommit args %v", args)
	}
	vol := f.volumeOfTarget(args[2])
	chain := f.chains[vol]
	base := strings.TrimPrefix(argAfter(args, "--base"), dir+"/"+vol+"/")
	if f.commitErr != "" {
		return fail(f.commitErr)
	}
	if f.files[dir+"/"+vol+"/"+base] != "0644" {
		return fail("Permission denied: the base file is read-only")
	}
	bi := idx(chain, base)
	if contains(args, "--active") {
		if !contains(args, "--pivot") || argAfter(args, "--top") != "" {
			return "", "", fmt.Errorf("bad active commit %v", args)
		}
		if bi < 0 {
			return fail("base not in chain")
		}
		f.chains[vol] = chain[bi:]
		return "Successfully pivoted\n", "", nil
	}
	top := strings.TrimPrefix(argAfter(args, "--top"), dir+"/"+vol+"/")
	ti := idx(chain, top)
	if ti <= 0 || bi != ti+1 {
		return fail(fmt.Sprintf("top %s and base %s are not neighbours below the active file in %v", top, base, chain))
	}
	f.chains[vol] = append(append([]string{}, chain[:ti]...), chain[bi:]...)
	f.backing[dir+"/"+vol+"/"+chain[ti-1]] = base
	return "Commit complete\n", "", nil
}

func contains(s []string, v string) bool { return idx(s, v) >= 0 }

func idx(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

func argAfter(args []string, flag string) string {
	if i := idx(args, flag); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

var moment = time.Date(2026, 10, 21, 14, 4, 0, 0, time.UTC)

func newTestSnapshotter(f *fakeLauncher) *Snapshotter {
	s := New(f)
	s.now = func() time.Time { return moment }
	return s
}

func TestChain(t *testing.T) {
	f := newFakeLauncher()
	f.chains["rootdisk"] = []string{"snap-b.qcow2", "snap-a.qcow2", "disk.img"}
	s := newTestSnapshotter(f)

	chain, err := s.Chain(context.Background(), testVM, "rootdisk")
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	want := []File{{"snap-b.qcow2", "qcow2"}, {"snap-a.qcow2", "qcow2"}, {"disk.img", "raw"}}
	if fmt.Sprint(chain) != fmt.Sprint(want) {
		t.Errorf("Chain = %v, want %v", chain, want)
	}
	if _, err := s.Chain(context.Background(), testVM, "nosuch"); !errors.Is(err, ErrNotADisk) {
		t.Errorf("Chain of an unknown volume: %v, want ErrNotADisk", err)
	}
}

func TestTake_MakesANewTopFileAndFreezesTheOldOne(t *testing.T) {
	f := newFakeLauncher()
	s := newTestSnapshotter(f)

	res, err := s.Take(context.Background(), testVM, "rootdisk", "snap-0a1b.qcow2", true)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if got := fmt.Sprint(f.chains["rootdisk"]); got != "[snap-0a1b.qcow2 disk.img]" {
		t.Errorf("rootdisk chain = %s", got)
	}
	if got := fmt.Sprint(f.chains["data"]); got != "[disk.img]" {
		t.Errorf("the other disk must not change, chain = %s", got)
	}
	if res.Frozen != "disk.img" || !res.Moment.Equal(moment) || !res.FileSystemConsistent || res.FreezeError != "" {
		t.Errorf("Result = %+v", res)
	}
	if got := fmt.Sprint(names(res.Chain)); got != "[snap-0a1b.qcow2 disk.img]" {
		t.Errorf("Result chain = %s", got)
	}
	if f.files[dir+"/rootdisk/disk.img"] != "0444" {
		t.Errorf("the frozen file must be read-only, mode %s", f.files[dir+"/rootdisk/disk.img"])
	}
	if f.files[dir+"/rootdisk/snap-0a1b.qcow2"] != "0644" {
		t.Errorf("the new top file must stay writable")
	}
	if !strings.Contains(strings.Join(f.log, "\n"), "--quiesce") {
		t.Errorf("expected a freeze when asked, log:\n%s", strings.Join(f.log, "\n"))
	}
}

func TestTake_WithoutFreezeWhenTheGuestIsFrozenAlready(t *testing.T) {
	f := newFakeLauncher()
	s := newTestSnapshotter(f)

	res, err := s.Take(context.Background(), testVM, "data", "snap-1.qcow2", false)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if strings.Contains(strings.Join(f.log, "\n"), "--quiesce") {
		t.Error("must not ask for a freeze")
	}
	if !res.FileSystemConsistent {
		t.Error("a snapshot inside the caller's freeze is file-system consistent")
	}
}

func TestTake_FreezeFailsGivesACrashConsistentSnapshot(t *testing.T) {
	f := newFakeLauncher()
	f.quiesceErr = "error: Guest agent is not responding: QEMU guest agent is not connected"
	s := newTestSnapshotter(f)

	res, err := s.Take(context.Background(), testVM, "rootdisk", "snap-1.qcow2", true)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if res.FileSystemConsistent || !strings.Contains(res.FreezeError, "guest agent is not connected") {
		t.Errorf("Result = %+v", res)
	}
	if got := fmt.Sprint(f.chains["rootdisk"]); got != "[snap-1.qcow2 disk.img]" {
		t.Errorf("chain = %s", got)
	}
}

func TestTake_FailureLeavesTheChainAsItWas(t *testing.T) {
	f := newFakeLauncher()
	f.snapshotErr = "error: internal error: unable to execute QEMU command"
	s := newTestSnapshotter(f)

	if _, err := s.Take(context.Background(), testVM, "rootdisk", "snap-1.qcow2", false); err == nil {
		t.Fatal("expected an error")
	}
	if got := fmt.Sprint(f.chains["rootdisk"]); got != "[disk.img]" {
		t.Errorf("chain = %s", got)
	}
	if f.files[dir+"/rootdisk/disk.img"] != "0644" {
		t.Error("the live file must stay writable when no snapshot was taken")
	}
	if _, left := f.files[dir+"/rootdisk/snap-1.qcow2"]; left {
		t.Error("the unused new file must be removed")
	}
}

func TestTake_IsSafeToRepeat(t *testing.T) {
	f := newFakeLauncher()
	s := newTestSnapshotter(f)
	if _, err := s.Take(context.Background(), testVM, "rootdisk", "snap-1.qcow2", false); err != nil {
		t.Fatalf("Take: %v", err)
	}
	// The caller could not save the result, so it asks again. Nothing new is
	// made, and the frozen file stays read-only.
	f.files[dir+"/rootdisk/disk.img"] = "0644"
	res, err := s.Take(context.Background(), testVM, "rootdisk", "snap-1.qcow2", false)
	if err != nil {
		t.Fatalf("second Take: %v", err)
	}
	if got := fmt.Sprint(f.chains["rootdisk"]); got != "[snap-1.qcow2 disk.img]" {
		t.Errorf("chain = %s", got)
	}
	if res.Frozen != "disk.img" || !res.Moment.IsZero() {
		t.Errorf("a repeat reports the frozen file and no new moment, got %+v", res)
	}
	if f.files[dir+"/rootdisk/disk.img"] != "0444" {
		t.Error("a repeat must make the frozen file read-only")
	}
}

func TestTake_RefusesALongChain(t *testing.T) {
	f := newFakeLauncher()
	chain := []string{"disk.img"}
	for i := 0; i < MaxSnapshots; i++ {
		chain = append([]string{fmt.Sprintf("snap-%d.qcow2", i)}, chain...)
	}
	f.chains["rootdisk"] = chain
	s := newTestSnapshotter(f)
	if _, err := s.Take(context.Background(), testVM, "rootdisk", "snap-new.qcow2", false); !errors.Is(err, ErrChainFull) {
		t.Errorf("Take: %v, want ErrChainFull", err)
	}
}

func TestTake_RefusesBadNames(t *testing.T) {
	s := newTestSnapshotter(newFakeLauncher())
	for _, name := range []string{"disk.img", "../x.qcow2", "snap-1", "snap-A.qcow2", "snap-1.qcow2/x", ""} {
		if _, err := s.Take(context.Background(), testVM, "rootdisk", name, false); err == nil {
			t.Errorf("Take accepted file name %q", name)
		}
	}
	if _, err := s.Take(context.Background(), testVM, "../etc", "snap-1.qcow2", false); err == nil {
		t.Error("Take accepted a bad volume name")
	}
}

// chainOf builds a chain by taking snapshots, so files and backing names are real.
func chainOf(t *testing.T, s *Snapshotter, vol string, files ...string) {
	t.Helper()
	for _, f := range files {
		if _, err := s.Take(context.Background(), testVM, vol, f, false); err != nil {
			t.Fatalf("Take %s: %v", f, err)
		}
	}
}

func TestMerge_OldestSnapshot(t *testing.T) {
	f := newFakeLauncher()
	s := newTestSnapshotter(f)
	chainOf(t, s, "rootdisk", "snap-a.qcow2", "snap-b.qcow2")
	// Delete the snapshot frozen in disk.img: snap-a merges into it.
	chain, err := s.Merge(context.Background(), testVM, "rootdisk", "disk.img", "snap-a.qcow2")
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if got := fmt.Sprint(names(chain)); got != "[snap-b.qcow2 disk.img]" {
		t.Errorf("chain = %s", got)
	}
	if f.files[dir+"/rootdisk/disk.img"] != "0444" {
		t.Error("disk.img now holds the next snapshot and must be read-only again")
	}
	if _, left := f.files[dir+"/rootdisk/snap-a.qcow2"]; left {
		t.Error("the merged file must be deleted")
	}
}

func TestMerge_NewestSnapshotMergesTheLiveFile(t *testing.T) {
	f := newFakeLauncher()
	s := newTestSnapshotter(f)
	chainOf(t, s, "rootdisk", "snap-a.qcow2", "snap-b.qcow2")
	// The newest snapshot is frozen in snap-a; the live file snap-b merges into it.
	chain, err := s.Merge(context.Background(), testVM, "rootdisk", "snap-a.qcow2", "snap-b.qcow2")
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if got := fmt.Sprint(names(chain)); got != "[snap-a.qcow2 disk.img]" {
		t.Errorf("chain = %s", got)
	}
	if f.files[dir+"/rootdisk/snap-a.qcow2"] != "0644" {
		t.Error("snap-a is the live file now and must be writable")
	}
	if f.files[dir+"/rootdisk/disk.img"] != "0444" {
		t.Error("the older snapshot must stay read-only")
	}
	if !strings.Contains(strings.Join(f.log, "\n"), "--active") {
		t.Error("merging the live file needs an active commit")
	}
}

func TestMerge_MiddleSnapshot(t *testing.T) {
	f := newFakeLauncher()
	s := newTestSnapshotter(f)
	chainOf(t, s, "rootdisk", "snap-a.qcow2", "snap-b.qcow2", "snap-c.qcow2")
	chain, err := s.Merge(context.Background(), testVM, "rootdisk", "snap-a.qcow2", "snap-b.qcow2")
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if got := fmt.Sprint(names(chain)); got != "[snap-c.qcow2 snap-a.qcow2 disk.img]" {
		t.Errorf("chain = %s", got)
	}
	if f.backing[dir+"/rootdisk/snap-c.qcow2"] != "snap-a.qcow2" {
		t.Errorf("snap-c must back onto snap-a by name, backs onto %q", f.backing[dir+"/rootdisk/snap-c.qcow2"])
	}
	if f.files[dir+"/rootdisk/snap-a.qcow2"] != "0444" {
		t.Error("snap-a still holds a snapshot and must be read-only")
	}
}

func TestMerge_FailureKeepsTheFileReadOnly(t *testing.T) {
	f := newFakeLauncher()
	s := newTestSnapshotter(f)
	chainOf(t, s, "rootdisk", "snap-a.qcow2", "snap-b.qcow2")
	f.commitErr = "error: block copy still active: disk 'vda' already in active block job"
	if _, err := s.Merge(context.Background(), testVM, "rootdisk", "disk.img", "snap-a.qcow2"); err == nil {
		t.Fatal("expected an error")
	}
	if f.files[dir+"/rootdisk/disk.img"] != "0444" {
		t.Error("a failed merge must leave the frozen file read-only")
	}
	if got := fmt.Sprint(f.chains["rootdisk"]); got != "[snap-b.qcow2 snap-a.qcow2 disk.img]" {
		t.Errorf("chain = %s", got)
	}
}

func TestMerge_DoneAlreadyOnlyCleansUp(t *testing.T) {
	f := newFakeLauncher()
	s := newTestSnapshotter(f)
	chainOf(t, s, "rootdisk", "snap-a.qcow2", "snap-b.qcow2")
	if _, err := s.Merge(context.Background(), testVM, "rootdisk", "disk.img", "snap-a.qcow2"); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	f.files[dir+"/rootdisk/snap-a.qcow2"] = "0644" // as if the delete had not run
	commits := strings.Count(strings.Join(f.log, "\n"), "blockcommit")
	// The caller could not save that the merge was done and asks again. It
	// must not merge disk.img's new child too: that would delete another
	// snapshot.
	chain, err := s.Merge(context.Background(), testVM, "rootdisk", "disk.img", "snap-a.qcow2")
	if err != nil {
		t.Fatalf("second Merge: %v", err)
	}
	if strings.Count(strings.Join(f.log, "\n"), "blockcommit") != commits {
		t.Error("a repeat must not merge again")
	}
	if got := fmt.Sprint(names(chain)); got != "[snap-b.qcow2 disk.img]" {
		t.Errorf("chain = %s", got)
	}
	if _, left := f.files[dir+"/rootdisk/snap-a.qcow2"]; left {
		t.Error("a repeat must delete the merged file")
	}
}

func TestMerge_RefusesFilesThatAreNotNeighbours(t *testing.T) {
	f := newFakeLauncher()
	s := newTestSnapshotter(f)
	chainOf(t, s, "rootdisk", "snap-a.qcow2", "snap-b.qcow2")
	for _, pair := range [][2]string{
		{"disk.img", "snap-b.qcow2"},     // not the next file
		{"snap-b.qcow2", "snap-a.qcow2"}, // wrong way round
		{"snap-x.qcow2", "snap-b.qcow2"}, // not in the chain
		{"snap-b.qcow2", "disk.img"},     // the live file holds no snapshot
	} {
		if _, err := s.Merge(context.Background(), testVM, "rootdisk", pair[0], pair[1]); err == nil {
			t.Errorf("Merge(%s, %s) was accepted", pair[0], pair[1])
		}
	}
	if got := fmt.Sprint(f.chains["rootdisk"]); got != "[snap-b.qcow2 snap-a.qcow2 disk.img]" {
		t.Errorf("chain = %s", got)
	}
}

func TestFlatten(t *testing.T) {
	f := newFakeLauncher()
	s := newTestSnapshotter(f)
	chainOf(t, s, "rootdisk", "snap-a.qcow2", "snap-b.qcow2")
	if err := s.Flatten(context.Background(), testVM, "rootdisk"); err != nil {
		t.Fatalf("Flatten: %v", err)
	}
	if got := fmt.Sprint(f.chains["rootdisk"]); got != "[disk.img]" {
		t.Errorf("chain = %s", got)
	}
	if f.files[dir+"/rootdisk/disk.img"] != "0644" {
		t.Error("disk.img is the live file again and must be writable")
	}
	for _, n := range []string{"snap-a.qcow2", "snap-b.qcow2"} {
		if _, left := f.files[dir+"/rootdisk/"+n]; left {
			t.Errorf("%s must be deleted", n)
		}
	}
	// Flattening a plain disk does nothing.
	if err := s.Flatten(context.Background(), testVM, "data"); err != nil {
		t.Fatalf("Flatten plain disk: %v", err)
	}
}
