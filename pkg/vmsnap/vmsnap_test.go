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
	// owners is the owner UID of a file; a file not listed is owned by the
	// QEMU user. Only the owner can chmod a file, as on NFS.
	owners  map[string]int
	backing map[string]string // path -> backing file name, as written in the file

	quiesceErr string // snapshot-create-as --quiesce fails with this
	// agentFrozen: someone else has frozen the guest. The guest agent then
	// refuses every freeze, as qemu-ga does.
	agentFrozen bool
	agentDown   bool   // the guest agent does not answer
	snapshotErr string // snapshot-create-as fails with this
	// snapshot-create-as makes the snapshot, then reports this error, as
	// when the exec stream breaks after libvirt has done the work.
	snapshotErrAfter string
	quiesceErrAfter  string // the same, for the --quiesce try only
	commitErr        string // blockcommit fails with this
	chmodFrozenErr   string // chmod to the frozen mode fails with this
	pivotErr         string // blockjob --pivot fails with this
	// jobPolls is how many --info calls a started commit job runs for
	// before it is ready (active) or done (not active).
	jobPolls int
	jobs     map[string]*fakeJob // target -> block job
	aborts   []string            // targets of blockjob --abort
	log      []string
}

// fakeJob is a block job that libvirt runs in the background.
type fakeJob struct {
	kind  string // "Active Block Commit", "Block Commit" or "Block Copy"
	vol   string
	base  string
	top   string
	polls int
}

func newFakeLauncher() *fakeLauncher {
	f := &fakeLauncher{
		chains:  map[string][]string{"rootdisk": {"disk.img"}, "data": {"disk.img"}},
		targets: map[string]string{"rootdisk": "vda", "data": "vdb"},
		files:   map[string]string{},
		owners:  map[string]int{},
		backing: map[string]string{},
		jobs:    map[string]*fakeJob{},
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
		if cmd[1] == modeFrozen && f.chmodFrozenErr != "" {
			return fail(f.chmodFrozenErr)
		}
		if f.ownerOf(cmd[2]) != qemuUID {
			return fail("chmod: changing permissions of '" + cmd[2] + "': Operation not permitted")
		}
		f.files[cmd[2]] = cmd[1]
		return "", "", nil
	case "id":
		// id -u
		if len(cmd) != 2 || cmd[1] != "-u" {
			return "", "", fmt.Errorf("bad id args %v", cmd)
		}
		return fmt.Sprintf("%d\n", qemuUID), "", nil
	case "stat":
		// stat -c %u -- <path>
		if len(cmd) != 5 || cmd[1] != "-c" || cmd[2] != "%u" || cmd[3] != "--" {
			return "", "", fmt.Errorf("bad stat args %v", cmd)
		}
		if _, ok := f.files[cmd[4]]; !ok {
			return fail("stat: cannot statx '" + cmd[4] + "': No such file or directory")
		}
		return fmt.Sprintf("%d\n", f.ownerOf(cmd[4])), "", nil
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

// qemuUID is the user the compute container runs as.
const qemuUID = 107

func (f *fakeLauncher) ownerOf(p string) int {
	if u, ok := f.owners[p]; ok {
		return u
	}
	return qemuUID
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
	case "blockjob":
		return f.blockjob(args, fail)
	case "qemu-agent-command":
		if len(args) != 3 || args[1] != "vms_web" || args[2] != `{"execute":"guest-fsfreeze-status"}` {
			return "", "", fmt.Errorf("bad qemu-agent-command args %v", args)
		}
		if f.agentDown {
			return fail("error: Guest agent is not responding: QEMU guest agent is not connected")
		}
		if f.agentFrozen {
			return `{"return":"frozen"}` + "\n", "", nil
		}
		return `{"return":"thawed"}` + "\n", "", nil
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
	if contains(args, "--quiesce") && f.agentFrozen {
		return fail("error: internal error: unable to execute QEMU agent command 'guest-fsfreeze-freeze': Command guest-fsfreeze-freeze has been disabled: the command is not allowed")
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
	if contains(args, "--quiesce") && f.quiesceErrAfter != "" {
		return fail(f.quiesceErrAfter)
	}
	if f.snapshotErrAfter != "" {
		return fail(f.snapshotErrAfter)
	}
	return "Domain snapshot " + args[2] + " created\n", "", nil
}

func (f *fakeLauncher) commit(args []string, fail func(string) (string, string, error)) (string, string, error) {
	if args[1] != "vms_web" || !contains(args, "--keep-relative") {
		return "", "", fmt.Errorf("bad blockcommit args %v", args)
	}
	vol := f.volumeOfTarget(args[2])
	chain := f.chains[vol]
	base := strings.TrimPrefix(argAfter(args, "--base"), dir+"/"+vol+"/")
	if f.commitErr != "" {
		return fail(f.commitErr)
	}
	if _, busy := f.jobs[args[2]]; busy {
		return fail("error: block copy still active: disk '" + args[2] + "' already in active block job")
	}
	if f.files[dir+"/"+vol+"/"+base] != "0644" {
		return fail("Permission denied: the base file is read-only")
	}
	bi := idx(chain, base)
	if contains(args, "--active") {
		if argAfter(args, "--top") != "" || bi < 0 {
			return "", "", fmt.Errorf("bad active commit %v", args)
		}
		if contains(args, "--wait") != contains(args, "--pivot") {
			return "", "", fmt.Errorf("an active commit pivots by itself only with --wait: %v", args)
		}
		if contains(args, "--wait") {
			f.chains[vol] = chain[bi:]
			return "Successfully pivoted\n", "", nil
		}
		f.jobs[args[2]] = &fakeJob{kind: "Active Block Commit", vol: vol, base: base, polls: f.jobPolls}
		return "Active Block Commit started\n", "", nil
	}
	top := strings.TrimPrefix(argAfter(args, "--top"), dir+"/"+vol+"/")
	ti := idx(chain, top)
	if ti <= 0 || bi != ti+1 {
		return fail(fmt.Sprintf("top %s and base %s are not neighbours below the active file in %v", top, base, chain))
	}
	if contains(args, "--wait") {
		f.finishCommit(&fakeJob{vol: vol, base: base, top: top})
		return "Commit complete\n", "", nil
	}
	f.jobs[args[2]] = &fakeJob{kind: "Block Commit", vol: vol, base: base, top: top, polls: f.jobPolls}
	return "Block Commit started\n", "", nil
}

// finishCommit removes the top file of a commit that is not active.
func (f *fakeLauncher) finishCommit(j *fakeJob) {
	chain := f.chains[j.vol]
	ti := idx(chain, j.top)
	f.chains[j.vol] = append(append([]string{}, chain[:ti]...), chain[ti+1:]...)
	f.backing[dir+"/"+j.vol+"/"+chain[ti-1]] = j.base
}

// blockjob is `virsh blockjob <dom> <target> --info --raw | --pivot | --abort`.
func (f *fakeLauncher) blockjob(args []string, fail func(string) (string, string, error)) (string, string, error) {
	if len(args) < 4 || args[1] != "vms_web" {
		return "", "", fmt.Errorf("bad blockjob args %v", args)
	}
	t := args[2]
	j := f.jobs[t]
	switch {
	case contains(args, "--info"):
		if !contains(args, "--raw") {
			return "", "", fmt.Errorf("blockjob --info without --raw: %v", args)
		}
		if j == nil {
			return "", "", nil
		}
		if j.polls > 0 {
			j.polls--
			return fmt.Sprintf(" type=%s\n bandwidth=0\n cur=1048576\n end=2621440\n", j.kind), "", nil
		}
		if j.kind == "Block Commit" {
			f.finishCommit(j)
			delete(f.jobs, t)
			return "", "", nil
		}
		return fmt.Sprintf(" type=%s\n bandwidth=0\n cur=2621440\n end=2621440\n", j.kind), "", nil
	case contains(args, "--pivot"):
		if j == nil || j.kind != "Active Block Commit" || j.polls > 0 {
			return fail("error: block job is not ready")
		}
		if f.pivotErr != "" {
			return fail(f.pivotErr)
		}
		f.chains[j.vol] = f.chains[j.vol][idx(f.chains[j.vol], j.base):]
		delete(f.jobs, t)
		return "", "", nil
	case contains(args, "--abort"):
		f.aborts = append(f.aborts, t)
		delete(f.jobs, t)
		return "", "", nil
	}
	return "", "", fmt.Errorf("bad blockjob args %v", args)
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

func TestTake_InsideAnotherFreezeIsFileSystemConsistent(t *testing.T) {
	f := newFakeLauncher()
	f.agentFrozen = true // OpenShift froze the guest for a VM snapshot
	s := newTestSnapshotter(f)

	res, err := s.Take(context.Background(), testVM, "rootdisk", "snap-1.qcow2", true)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if !res.FileSystemConsistent || res.FreezeError != "" {
		t.Errorf("the guest is frozen, so the snapshot is file-system consistent, got %+v", res)
	}
	if strings.Contains(strings.Join(f.log, "\n"), "--quiesce") {
		t.Error("must not ask for a second freeze")
	}
}

func TestTake_AgentDownGivesACrashConsistentSnapshot(t *testing.T) {
	f := newFakeLauncher()
	f.agentDown = true
	f.quiesceErr = "error: Guest agent is not responding: QEMU guest agent is not connected"
	s := newTestSnapshotter(f)

	res, err := s.Take(context.Background(), testVM, "rootdisk", "snap-1.qcow2", true)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if res.FileSystemConsistent {
		t.Errorf("Result = %+v, want crash consistent", res)
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

func TestTake_ErrorAfterTheSnapshotKeepsTheLiveFile(t *testing.T) {
	f := newFakeLauncher()
	f.snapshotErrAfter = "error: Disconnected from qemu:///session due to end of file"
	s := newTestSnapshotter(f)

	res, err := s.Take(context.Background(), testVM, "rootdisk", "snap-1.qcow2", false)
	if err != nil {
		t.Fatalf("Take: %v: the snapshot was made, so Take must report it", err)
	}
	if _, ok := f.files[dir+"/rootdisk/snap-1.qcow2"]; !ok {
		t.Fatal("the new file is the VM's live file now and must never be removed")
	}
	if got := fmt.Sprint(f.chains["rootdisk"]); got != "[snap-1.qcow2 disk.img]" {
		t.Errorf("chain = %s", got)
	}
	if res.Frozen != "disk.img" || f.files[dir+"/rootdisk/disk.img"] != "0444" {
		t.Errorf("disk.img holds the snapshot and must be read-only, result %+v", res)
	}
}

func TestTake_ErrorAfterAFrozenSnapshotDoesNotSnapshotAgain(t *testing.T) {
	f := newFakeLauncher()
	f.quiesceErrAfter = "error: Disconnected from qemu:///session due to end of file"
	s := newTestSnapshotter(f)

	res, err := s.Take(context.Background(), testVM, "rootdisk", "snap-1.qcow2", true)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if n := strings.Count(strings.Join(f.log, "\n"), "snapshot-create-as"); n != 1 {
		t.Errorf("snapshot-create-as ran %d times: a second run would stack the live file on itself", n)
	}
	if got := fmt.Sprint(f.chains["rootdisk"]); got != "[snap-1.qcow2 disk.img]" {
		t.Errorf("chain = %s", got)
	}
	if res.FileSystemConsistent {
		t.Error("the freeze is not known to have worked, so the snapshot is only crash consistent")
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

// merge runs Merge until it is done, as the pool manager's retries do.
func merge(t *testing.T, s *Snapshotter, vol, file, child string) ([]File, error) {
	t.Helper()
	for i := 0; i < 10; i++ {
		chain, err := s.Merge(context.Background(), testVM, vol, file, child)
		if !errors.Is(err, ErrMergeRunning) {
			return chain, err
		}
	}
	t.Fatal("the merge never finished")
	return nil, nil
}

func TestMerge_OldestSnapshot(t *testing.T) {
	f := newFakeLauncher()
	s := newTestSnapshotter(f)
	chainOf(t, s, "rootdisk", "snap-a.qcow2", "snap-b.qcow2")
	// Delete the snapshot frozen in disk.img: snap-a merges into it.
	chain, err := merge(t, s, "rootdisk", "disk.img", "snap-a.qcow2")
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
	chain, err := merge(t, s, "rootdisk", "snap-a.qcow2", "snap-b.qcow2")
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
	chain, err := merge(t, s, "rootdisk", "snap-a.qcow2", "snap-b.qcow2")
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
	if _, err := merge(t, s, "rootdisk", "disk.img", "snap-a.qcow2"); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	f.files[dir+"/rootdisk/snap-a.qcow2"] = "0644" // as if the delete had not run
	commits := strings.Count(strings.Join(f.log, "\n"), "blockcommit")
	// The caller could not save that the merge was done and asks again. It
	// must not merge disk.img's new child too: that would delete another
	// snapshot.
	chain, err := merge(t, s, "rootdisk", "disk.img", "snap-a.qcow2")
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

func TestMerge_NewestSnapshotDoesNotWait(t *testing.T) {
	f := newFakeLauncher()
	f.jobPolls = 1
	s := newTestSnapshotter(f)
	chainOf(t, s, "rootdisk", "snap-a.qcow2")
	ctx := context.Background()
	if _, err := s.Merge(ctx, testVM, "rootdisk", "disk.img", "snap-a.qcow2"); !errors.Is(err, ErrMergeRunning) {
		t.Fatalf("Merge: %v, want ErrMergeRunning once the job is started", err)
	}
	log := strings.Join(f.log, "\n")
	if strings.Contains(log, "--wait") || strings.Contains(log, "blockcommit vms_web vda --base "+dir+"/rootdisk/disk.img --keep-relative --active --pivot") {
		t.Errorf("Merge must start the job and return, log:\n%s", log)
	}
	if _, err := s.Merge(ctx, testVM, "rootdisk", "disk.img", "snap-a.qcow2"); !errors.Is(err, ErrMergeRunning) {
		t.Fatalf("Merge: %v, want ErrMergeRunning until the job is ready", err)
	}
	if n := strings.Count(strings.Join(f.log, "\n"), "blockcommit"); n != 1 {
		t.Errorf("a running job must not be started again, blockcommit ran %d times", n)
	}
	chain, err := s.Merge(ctx, testVM, "rootdisk", "disk.img", "snap-a.qcow2")
	if err != nil {
		t.Fatalf("Merge: %v, want done once the job is ready and pivoted", err)
	}
	if got := fmt.Sprint(names(chain)); got != "[disk.img]" {
		t.Errorf("chain = %s", got)
	}
	if f.files[dir+"/rootdisk/disk.img"] != "0644" {
		t.Error("disk.img is the live file again and must be writable")
	}
	if _, left := f.files[dir+"/rootdisk/snap-a.qcow2"]; left {
		t.Error("the merged file must be deleted")
	}
}

func TestMerge_MiddleSnapshotDoesNotWait(t *testing.T) {
	f := newFakeLauncher()
	f.jobPolls = 1
	s := newTestSnapshotter(f)
	chainOf(t, s, "rootdisk", "snap-a.qcow2", "snap-b.qcow2")
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := s.Merge(ctx, testVM, "rootdisk", "disk.img", "snap-a.qcow2"); !errors.Is(err, ErrMergeRunning) {
			t.Fatalf("call %d: %v, want ErrMergeRunning", i, err)
		}
	}
	chain, err := s.Merge(ctx, testVM, "rootdisk", "disk.img", "snap-a.qcow2")
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if got := fmt.Sprint(names(chain)); got != "[snap-b.qcow2 disk.img]" {
		t.Errorf("chain = %s", got)
	}
	if f.files[dir+"/rootdisk/disk.img"] != "0444" {
		t.Error("disk.img holds snap-b's snapshot now and must be read-only")
	}
}

func TestMerge_FailedPivotAbortsTheJob(t *testing.T) {
	f := newFakeLauncher()
	f.pivotErr = "error: internal error: unable to execute QEMU command 'job-complete'"
	s := newTestSnapshotter(f)
	chainOf(t, s, "rootdisk", "snap-a.qcow2")
	ctx := context.Background()
	if _, err := s.Merge(ctx, testVM, "rootdisk", "disk.img", "snap-a.qcow2"); !errors.Is(err, ErrMergeRunning) {
		t.Fatalf("Merge: %v", err)
	}
	_, err := s.Merge(ctx, testVM, "rootdisk", "disk.img", "snap-a.qcow2")
	if err == nil || errors.Is(err, ErrMergeRunning) {
		t.Fatalf("Merge: %v, want the pivot error", err)
	}
	if fmt.Sprint(f.aborts) != "[vda]" {
		t.Errorf("aborts = %v: a failed pivot must stop the job", f.aborts)
	}
	if f.files[dir+"/rootdisk/disk.img"] != "0444" {
		t.Error("disk.img still holds the snapshot and must be read-only again")
	}
	if got := fmt.Sprint(f.chains["rootdisk"]); got != "[snap-a.qcow2 disk.img]" {
		t.Errorf("chain = %s", got)
	}
}

func TestMerge_WaitsForAnotherBlockJob(t *testing.T) {
	f := newFakeLauncher()
	s := newTestSnapshotter(f)
	chainOf(t, s, "rootdisk", "snap-a.qcow2")
	f.jobs["vda"] = &fakeJob{kind: "Block Copy", vol: "rootdisk", polls: 5}
	if _, err := s.Merge(context.Background(), testVM, "rootdisk", "disk.img", "snap-a.qcow2"); err == nil {
		t.Fatal("expected an error while another block job runs on the disk")
	}
	if strings.Contains(strings.Join(f.log, "\n"), "blockcommit") || len(f.aborts) != 0 {
		t.Error("Merge must not start a commit or stop another job")
	}
}

func TestTake_RefusesADiskOwnedByAnotherUserBeforeChangingAnything(t *testing.T) {
	f := newFakeLauncher()
	f.owners[dir+"/rootdisk/disk.img"] = 65534
	s := newTestSnapshotter(f)

	_, err := s.Take(context.Background(), testVM, "rootdisk", "snap-1.qcow2", true)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, ErrDiskNotOwned) {
		t.Errorf("error = %v, want ErrDiskNotOwned", err)
	}
	for _, want := range []string{"disk.img", "65534", "107"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message should name %q: %v", want, err)
		}
	}
	if got := fmt.Sprint(f.chains["rootdisk"]); got != "[disk.img]" {
		t.Errorf("chain = %s", got)
	}
	if _, made := f.files[dir+"/rootdisk/snap-1.qcow2"]; made {
		t.Error("no new file may be made")
	}
	for _, c := range f.log {
		if strings.Contains(c, "snapshot-create-as") || strings.HasPrefix(c, "qemu-img") {
			t.Errorf("nothing may change before the refusal, but ran: %s", c)
		}
	}
}

func TestTake_ChmodFailsAfterTheSnapshotRollsTheNewFileBack(t *testing.T) {
	f := newFakeLauncher()
	f.chmodFrozenErr = "chmod: changing permissions: Operation not permitted"
	s := newTestSnapshotter(f)

	_, err := s.Take(context.Background(), testVM, "rootdisk", "snap-1.qcow2", false)
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := fmt.Sprint(f.chains["rootdisk"]); got != "[disk.img]" {
		t.Errorf("chain = %s, want the chain as it was", got)
	}
	if _, left := f.files[dir+"/rootdisk/snap-1.qcow2"]; left {
		t.Error("the new overlay must be removed")
	}
	if f.files[dir+"/rootdisk/disk.img"] != "0644" {
		t.Errorf("disk.img must stay live and writable, mode %s", f.files[dir+"/rootdisk/disk.img"])
	}
}
