// Package vmcopy takes a copy of a running KubeVirt VM's disks at one moment.
//
// It runs commands in the VM's virt-launcher pod, in the compute container:
// KubeVirt's virt-freezer to freeze and thaw the guest file systems, qemu-img to
// make sparse qcow2 staging files, and QMP commands through virsh to make QEMU
// copy every disk into its staging file. One QMP transaction starts the copies
// of all disks, so they all show the same moment.
//
// Red Hat does not support QMP commands sent to a KubeVirt VM. The design and
// lab results are in docs/specs/2026-10-20-vm-safe-replication.md.
package vmcopy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	container = "compute"

	// vmiDisksDir holds one directory per filesystem volume; the disk image
	// is disk.img in it.
	vmiDisksDir = "/var/run/kubevirt-private/vmi-disks"

	// StagingPrefix starts the name of every staging file.
	StagingPrefix = ".repl-staging-"

	cancelPolls = 20
)

// ErrBaseLost means the change list a change copy needs is gone, for example
// after the VM restarted or moved. The caller makes a full copy instead.
var ErrBaseLost = errors.New("the change list of the last copy is gone")

// SnapshotIDPrefix starts the id of every copy that a snapshot makes. Ids
// name the staging file, block nodes, jobs and change list of a copy, so the
// prefix tells snapshot copies and replication copies apart.
const SnapshotIDPrefix = "snap"

// ErrBusy means a copy is still running in the VM, so no other copy can start.
var ErrBusy = errors.New("a copy is already running in the VM")

// bitmapPrefix starts the name of every change list (dirty bitmap) we make.
const bitmapPrefix = "repl-"

var (
	// volumeNameRe is a Kubernetes volume name (DNS-1123 label).
	volumeNameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	copyIDRe     = regexp.MustCompile(`^[a-z0-9]{1,16}$`)
	bitmapNameRe = regexp.MustCompile(`^repl-[a-z0-9]{1,16}$`)
)

// Execer runs a command in a pod container.
type Execer interface {
	Exec(ctx context.Context, namespace, podName, container string, command []string) (stdout, stderr string, err error)
}

// VM names a running VM and its virt-launcher pod.
type VM struct {
	Namespace string
	Pod       string
	Name      string // the VirtualMachineInstance name
}

func (vm VM) domain() string { return vm.Namespace + "_" + vm.Name }

// StartOptions configure a copy.
type StartOptions struct {
	// Speed is the copy speed limit per disk in bytes per second; 0 means no
	// limit.
	Speed int64
	// ID must be unique for the VM and is part of every name the copy uses.
	ID string
	// Base is the change list of the last copy that reached the far site.
	// If set, the copy holds only the blocks changed since that copy. If
	// empty, it is a full copy.
	Base string
}

// Copy is a copy that Start began. The controller keeps it between cycles.
type Copy struct {
	ID string
	// Bitmap is the change list this copy started. The next change copy
	// uses it as its Base.
	Bitmap string
	// Base is the change list this copy read, for a change copy.
	Base                 string
	Incremental          bool
	Moment               time.Time
	FileSystemConsistent bool
	FreezeError          string
	// Paused is how long the guest was frozen, from the freeze command to
	// the end of the thaw command.
	Paused time.Duration
	Disks  []Disk
}

// Disk is one disk of a Copy.
type Disk struct {
	Volume      string // pod volume name
	StagingFile string // file name in the volume's root directory
}

// State is the state of a running Copy.
type State int

const (
	// Running means QEMU is still copying.
	Running State = iota
	// Done means every disk copy finished without error.
	Done
	// Failed means at least one disk copy ended with an error.
	Failed
	// Lost means QEMU has no record of the copy, for example after the VM
	// restarted or moved.
	Lost
)

func (s State) String() string {
	return [...]string{"Running", "Done", "Failed", "Lost"}[s]
}

// Copier runs copies through an Execer.
type Copier struct {
	exec       Execer
	now        func() time.Time
	cancelWait time.Duration
}

// NewCopier returns a Copier.
func NewCopier(e Execer) *Copier {
	return &Copier{exec: e, now: time.Now, cancelWait: 500 * time.Millisecond}
}

func jobID(id string, i int) string  { return fmt.Sprintf("repl-%s-%d", id, i) }
func nodeID(id string, i int) string { return fmt.Sprintf("repl-%s-t%d", id, i) }
func stagingPath(d Disk) string      { return vmiDisksDir + "/" + d.Volume + "/" + d.StagingFile }

// Start freezes the guest, starts a copy of each volume at one moment, and
// thaws the guest. In the same step it starts a new change list on each disk.
// If the freeze fails, the copy goes ahead as crash consistent. If opts.Base is
// set and any disk lacks it, Start returns ErrBaseLost and does nothing.
func (c *Copier) Start(ctx context.Context, vm VM, volumes []string, opts StartOptions) (*Copy, error) {
	id := opts.ID
	if !copyIDRe.MatchString(id) {
		return nil, fmt.Errorf("copy ID %q is not valid", id)
	}
	if len(volumes) == 0 {
		return nil, fmt.Errorf("no volumes to copy")
	}
	for _, v := range volumes {
		if !volumeNameRe.MatchString(v) {
			return nil, fmt.Errorf("volume name %q is not valid", v)
		}
	}

	if opts.Base != "" && !bitmapNameRe.MatchString(opts.Base) {
		return nil, fmt.Errorf("change list name %q is not valid", opts.Base)
	}

	devs, err := c.queryBlock(ctx, vm)
	if err != nil {
		return nil, err
	}
	cp := &Copy{ID: id, Bitmap: bitmapPrefix + id, Base: opts.Base, Incremental: opts.Base != ""}
	type disk struct {
		node string
		size int64
	}
	var found []disk
	for _, v := range volumes {
		d, ok := devs[v]
		if !ok {
			return nil, fmt.Errorf("volume %q is not a disk of VM %s/%s", v, vm.Namespace, vm.Name)
		}
		if opts.Base != "" && !d.bitmaps[opts.Base] {
			return nil, fmt.Errorf("volume %s: %w", v, ErrBaseLost)
		}
		found = append(found, disk{node: d.node, size: d.size})
		cp.Disks = append(cp.Disks, Disk{Volume: v, StagingFile: StagingPrefix + id + ".qcow2"})
	}

	// Prepare everything before the freeze, so the guest pauses only for the
	// transaction.
	fail := func(err error) (*Copy, error) {
		// The first error says what went wrong; a clean-up error would hide it,
		// and RemoveStale clears anything left before the next copy.
		_ = c.cleanup(ctx, vm, cp, true)
		return nil, err
	}
	for i, d := range cp.Disks {
		if _, err = c.run(ctx, vm, "qemu-img", "create", "-q", "-f", "qcow2", stagingPath(d), fmt.Sprint(found[i].size)); err != nil {
			return fail(fmt.Errorf("creating staging file for %s: %w", d.Volume, err))
		}
		if _, err = c.qmp(ctx, vm, "blockdev-add", map[string]any{
			"node-name":     nodeID(id, i),
			"driver":        "qcow2",
			"detect-zeroes": "unmap",
			"discard":       "unmap",
			"file":          map[string]any{"driver": "file", "filename": stagingPath(d)},
			"backing":       nil,
		}); err != nil {
			return fail(fmt.Errorf("opening staging file for %s: %w", d.Volume, err))
		}
	}
	var actions []any
	for i := range cp.Disks {
		actions = append(actions, map[string]any{"type": "block-dirty-bitmap-add", "data": map[string]any{
			"node":       found[i].node,
			"name":       cp.Bitmap,
			"persistent": false,
		}})
		backup := map[string]any{
			"job-id":       jobID(id, i),
			"device":       found[i].node,
			"target":       nodeID(id, i),
			"sync":         "full",
			"speed":        opts.Speed,
			"auto-dismiss": false,
		}
		if opts.Base != "" {
			// "never" keeps the base list as it is, so a failed copy loses
			// nothing: the next copy reads the same list again.
			backup["sync"] = "bitmap"
			backup["bitmap"] = opts.Base
			backup["bitmap-mode"] = "never"
		}
		actions = append(actions, map[string]any{"type": "blockdev-backup", "data": backup})
	}

	txn, err := json.Marshal(map[string]any{"execute": "transaction", "arguments": map[string]any{"actions": actions}})
	if err != nil {
		return fail(err)
	}
	res, err := c.freezeAndStart(ctx, vm, string(txn))
	if err != nil {
		return fail(err)
	}
	cp.Moment, cp.Paused = res.moment, res.paused
	freezeErr, txnErr, thawErr := res.freezeErr, res.txnErr, res.thawErr

	if txnErr != nil {
		return fail(fmt.Errorf("starting the copy: %w", txnErr))
	}
	switch {
	case freezeErr != nil:
		cp.FreezeError = freezeErr.Error()
	case thawErr != nil:
		// The guest thaws by itself after the timeout. The copy shows the
		// frozen moment, so it is still file-system consistent.
		cp.FileSystemConsistent = true
		cp.FreezeError = "thaw: " + thawErr.Error()
	default:
		cp.FileSystemConsistent = true
	}
	return cp, nil
}

// freezeScript freezes the guest with a 30 s automatic thaw, sends the QMP transaction and thaws the
// guest, all in one command in the pod, so the guest pauses only as long as
// the three steps take inside the pod. It always thaws, also after a failed
// freeze (a partial freeze is possible) and if the exec connection is cut.
// It always exits 0 and prints, one per line: the three exit codes, the
// transaction time in Unix nanoseconds, the pause in nanoseconds, and the
// base64 output of the three steps. Values come in as positional arguments
// only, never inside the script text.
const freezeScript = `set -u
vmi=$1 ns=$2 dom=$3 txn=$4
thaw() { virt-freezer --unfreeze --name="$vmi" --namespace="$ns" 2>&1; }
trap 'thaw >/dev/null' HUP INT TERM
t0=$(date +%s%N)
fo=$(virt-freezer --freeze --name="$vmi" --namespace="$ns" --unfreezeTimeoutSeconds=30 2>&1); frc=$?
tm=$(date +%s%N)
to=$(virsh qemu-monitor-command "$dom" "$txn" 2>&1); trc=$?
uo=$(thaw); urc=$?
t1=$(date +%s%N)
trap - HUP INT TERM
printf '%s\n' "$frc" "$trc" "$urc" "$tm" "$((t1-t0))"
printf '%s' "$fo" | base64 -w0; echo
printf '%s' "$to" | base64 -w0; echo
printf '%s' "$uo" | base64 -w0; echo
`

type freezeResult struct {
	moment                     time.Time
	paused                     time.Duration
	freezeErr, txnErr, thawErr error
}

func (c *Copier) freezeAndStart(ctx context.Context, vm VM, txn string) (*freezeResult, error) {
	out, err := c.run(ctx, vm, "bash", "-c", freezeScript, "repl-freeze", vm.Name, vm.Namespace, vm.domain(), txn)
	if err != nil {
		return nil, fmt.Errorf("running the freeze and copy start: %w", err)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 8 {
		return nil, fmt.Errorf("freeze script printed %d lines, want 8: %q", len(lines), out)
	}
	var nums [5]int64
	for i := range nums {
		if nums[i], err = strconv.ParseInt(lines[i], 10, 64); err != nil {
			return nil, fmt.Errorf("freeze script line %d: %w", i+1, err)
		}
	}
	var texts [3]string
	for i := range texts {
		b, err := base64.StdEncoding.DecodeString(lines[5+i])
		if err != nil {
			return nil, fmt.Errorf("freeze script line %d: %w", 6+i, err)
		}
		texts[i] = strings.TrimSpace(string(b))
	}
	res := &freezeResult{moment: time.Unix(0, nums[3]), paused: time.Duration(nums[4])}
	if nums[0] != 0 {
		res.freezeErr = fmt.Errorf("virt-freezer --freeze: exit code %d: %s", nums[0], texts[0])
	}
	if nums[1] != 0 {
		res.txnErr = fmt.Errorf("virsh: exit code %d: %s", nums[1], texts[1])
	} else if _, err := parseQMPReply("transaction", texts[1]); err != nil {
		res.txnErr = err
	}
	if nums[2] != 0 {
		res.thawErr = fmt.Errorf("virt-freezer --unfreeze: exit code %d: %s", nums[2], texts[2])
	}
	return res, nil
}

// Progress reports the state of a Copy. For Failed, the message has QEMU's
// error.
func (c *Copier) Progress(ctx context.Context, vm VM, cp *Copy) (State, string, error) {
	jobs, err := c.queryJobs(ctx, vm)
	if err != nil {
		return Running, "", err
	}
	state := Done
	var errs []string
	for i, d := range cp.Disks {
		j, ok := jobs[jobID(cp.ID, i)]
		switch {
		case !ok:
			return Lost, "QEMU has no record of the copy", nil
		case j.Status != "concluded":
			if state == Done {
				state = Running
			}
		case j.Error != "":
			errs = append(errs, fmt.Sprintf("%s: %s", d.Volume, j.Error))
		}
	}
	if len(errs) > 0 {
		return Failed, strings.Join(errs, "; "), nil
	}
	return state, "", nil
}

// Finish releases what QEMU holds for a finished Copy. If removeStaging is
// true, it also deletes the staging files. It is safe to call more than once.
func (c *Copier) Finish(ctx context.Context, vm VM, cp *Copy, removeStaging bool) error {
	return c.cleanup(ctx, vm, cp, removeStaging)
}

// Commit is called when a copy has reached the far site. It removes the base
// change list, so the copy's own list becomes the base of the next copy. It is
// safe to call more than once.
func (c *Copier) Commit(ctx context.Context, vm VM, cp *Copy) error {
	if cp.Base == "" {
		return nil
	}
	return c.removeBitmaps(ctx, vm, cp.Disks, func(name string) bool { return name == cp.Base })
}

// removeBitmaps removes the change lists that match on the disks of the given
// volumes. A missing list is not an error.
func (c *Copier) removeBitmaps(ctx context.Context, vm VM, disks []Disk, match func(string) bool) error {
	devs, err := c.queryBlock(ctx, vm)
	if err != nil {
		return err
	}
	for _, d := range disks {
		dev, ok := devs[d.Volume]
		if !ok {
			continue
		}
		for name := range dev.bitmaps {
			if !match(name) {
				continue
			}
			if _, err := c.qmp(ctx, vm, "block-dirty-bitmap-remove", map[string]any{"node": dev.node, "name": name}); err != nil {
				return fmt.Errorf("removing change list %s of %s: %w", name, d.Volume, err)
			}
		}
	}
	return nil
}

// Cancel stops a running Copy and deletes its staging files.
func (c *Copier) Cancel(ctx context.Context, vm VM, cp *Copy) error {
	for i := range cp.Disks {
		// A job that already ended cannot be cancelled; that is fine.
		_, _ = c.qmp(ctx, vm, "block-job-cancel", map[string]any{"device": jobID(cp.ID, i)})
	}
	for poll := 0; poll < cancelPolls; poll++ {
		st, _, err := c.Progress(ctx, vm, cp)
		if err != nil {
			return err
		}
		if st != Running {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.cancelWait):
		}
	}
	return c.cleanup(ctx, vm, cp, true)
}

// CheckIdle returns ErrBusy while any copy runs in the VM. It changes nothing,
// so a caller that must not touch other copies' change lists can use it
// instead of RemoveStale.
func (c *Copier) CheckIdle(ctx context.Context, vm VM) error {
	jobs, err := c.queryJobs(ctx, vm)
	if err != nil {
		return err
	}
	for id := range jobs {
		if strings.HasPrefix(id, "repl-") {
			return fmt.Errorf("copy job %s is still in QEMU: %w", id, ErrBusy)
		}
	}
	return nil
}

// RemoveStale deletes staging files, block nodes and change lists that an
// earlier copy left behind, for example after a controller restart. It keeps
// the change list keepBitmap, the base of the next change copy. It refuses
// while any copy runs in the VM.
func (c *Copier) RemoveStale(ctx context.Context, vm VM, volumes []string, keepBitmap string) error {
	return c.removeStale(ctx, vm, volumes, false, keepBitmap)
}

// CleanStaleCopies deletes what earlier snapshot copies left behind: their
// staging files, block nodes and change lists. It touches nothing that
// replication made, and RemoveStale touches nothing that a snapshot made, so
// neither can delete a staging file that the other's Job still needs. A copy
// in that wait has no QEMU job, so QEMU cannot show it. It refuses with
// ErrBusy while any copy runs in the VM.
func (c *Copier) CleanStaleCopies(ctx context.Context, vm VM, volumes []string) error {
	return c.removeStale(ctx, vm, volumes, true, "")
}

// removeStale removes the leftovers of one kind of copy: snapshot copies
// (their copy ids start with SnapshotIDPrefix) or all others.
func (c *Copier) removeStale(ctx context.Context, vm VM, volumes []string, snapshots bool, keepBitmap string) error {
	if err := c.CheckIdle(ctx, vm); err != nil {
		return err
	}
	// ours reports whether a name made by a copy belongs to the kind we clean.
	ours := func(name, prefix string) bool {
		return strings.HasPrefix(name, prefix) && strings.HasPrefix(strings.TrimPrefix(name, prefix), SnapshotIDPrefix) == snapshots
	}
	nodes, err := c.namedNodes(ctx, vm)
	if err != nil {
		return err
	}
	for _, n := range nodes {
		if ours(n, "repl-") {
			if _, err := c.qmp(ctx, vm, "blockdev-del", map[string]any{"node-name": n}); err != nil {
				return fmt.Errorf("removing block node %s: %w", n, err)
			}
		}
	}
	var disks []Disk
	for _, v := range volumes {
		if !volumeNameRe.MatchString(v) {
			return fmt.Errorf("volume name %q is not valid", v)
		}
		disks = append(disks, Disk{Volume: v})
	}
	if err := c.removeBitmaps(ctx, vm, disks, func(name string) bool {
		return ours(name, bitmapPrefix) && name != keepBitmap
	}); err != nil {
		return err
	}
	for _, v := range volumes {
		args := []string{"find", vmiDisksDir + "/" + v, "-maxdepth", "1"}
		if snapshots {
			args = append(args, "-name", StagingPrefix+SnapshotIDPrefix+"*.qcow2")
		} else {
			args = append(args, "-name", StagingPrefix+"*.qcow2", "!", "-name", StagingPrefix+SnapshotIDPrefix+"*")
		}
		if _, err := c.run(ctx, vm, append(args, "-delete")...); err != nil {
			return fmt.Errorf("removing staging files of %s: %w", v, err)
		}
	}
	return nil
}

// cleanup dismisses the copy's jobs, removes its block nodes and, if asked,
// its staging files. Missing jobs and nodes are not errors.
func (c *Copier) cleanup(ctx context.Context, vm VM, cp *Copy, removeStaging bool) error {
	jobs, err := c.queryJobs(ctx, vm)
	if err != nil {
		return err
	}
	nodes, err := c.namedNodes(ctx, vm)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, n := range nodes {
		have[n] = true
	}
	for i := range cp.Disks {
		if j, ok := jobs[jobID(cp.ID, i)]; ok && j.Status == "concluded" {
			if _, err := c.qmp(ctx, vm, "job-dismiss", map[string]any{"id": jobID(cp.ID, i)}); err != nil {
				return fmt.Errorf("dismissing copy job: %w", err)
			}
		}
		if have[nodeID(cp.ID, i)] {
			if _, err := c.qmp(ctx, vm, "blockdev-del", map[string]any{"node-name": nodeID(cp.ID, i)}); err != nil {
				return fmt.Errorf("closing staging file: %w", err)
			}
		}
	}
	if !removeStaging {
		return nil
	}
	// A copy that will not reach the far site must not become a base.
	if cp.Bitmap != "" {
		if err := c.removeBitmaps(ctx, vm, cp.Disks, func(name string) bool { return name == cp.Bitmap }); err != nil {
			return err
		}
	}
	args := []string{"rm", "-f", "--"}
	for _, d := range cp.Disks {
		args = append(args, stagingPath(d))
	}
	if _, err := c.run(ctx, vm, args...); err != nil {
		return fmt.Errorf("removing staging files: %w", err)
	}
	return nil
}

type blockDev struct {
	node    string
	size    int64
	bitmaps map[string]bool
}

// queryBlock maps each filesystem volume of the VM to the top block node of
// its disk and the disk size. The top file is disk.img, or the newest file of
// a snapshot chain in the same folder (see pkg/vmsnap).
func (c *Copier) queryBlock(ctx context.Context, vm VM) (map[string]blockDev, error) {
	out, err := c.qmp(ctx, vm, "query-block", nil)
	if err != nil {
		return nil, err
	}
	type bitmap struct {
		Name string `json:"name"`
	}
	var devs []struct {
		Inserted *struct {
			NodeName string `json:"node-name"`
			File     string `json:"file"`
			Image    struct {
				VirtualSize int64 `json:"virtual-size"`
			} `json:"image"`
			DirtyBitmaps []bitmap `json:"dirty-bitmaps"`
		} `json:"inserted"`
		// Older QEMU reported the bitmaps here.
		DirtyBitmaps []bitmap `json:"dirty-bitmaps"`
	}
	if err := json.Unmarshal(out, &devs); err != nil {
		return nil, fmt.Errorf("parsing query-block reply: %w", err)
	}
	m := map[string]blockDev{}
	for _, d := range devs {
		if d.Inserted != nil {
			bms := map[string]bool{}
			for _, b := range append(d.Inserted.DirtyBitmaps, d.DirtyBitmaps...) {
				bms[b.Name] = true
			}
			if vol := volumeOfFile(d.Inserted.File); vol != "" {
				m[vol] = blockDev{node: d.Inserted.NodeName, size: d.Inserted.Image.VirtualSize, bitmaps: bms}
			}
		}
	}
	return m, nil
}

// volumeOfFile returns the volume whose folder holds a disk's top file, or ""
// for a file outside the volume folders. QEMU reports a file that has a
// backing file named relative to it as a json: description; the top file is
// its "file" filename then.
func volumeOfFile(file string) string {
	if desc, ok := strings.CutPrefix(file, "json:"); ok {
		var d struct {
			File struct {
				Filename string `json:"filename"`
			} `json:"file"`
		}
		if json.Unmarshal([]byte(desc), &d) != nil {
			return ""
		}
		file = d.File.Filename
	}
	rest, ok := strings.CutPrefix(file, vmiDisksDir+"/")
	if !ok {
		return ""
	}
	vol, name, ok := strings.Cut(rest, "/")
	if !ok || strings.Contains(name, "/") {
		return ""
	}
	return vol
}

type qemuJob struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Error  string `json:"error"`
}

func (c *Copier) queryJobs(ctx context.Context, vm VM) (map[string]qemuJob, error) {
	out, err := c.qmp(ctx, vm, "query-jobs", nil)
	if err != nil {
		return nil, err
	}
	var jobs []qemuJob
	if err := json.Unmarshal(out, &jobs); err != nil {
		return nil, fmt.Errorf("parsing query-jobs reply: %w", err)
	}
	m := map[string]qemuJob{}
	for _, j := range jobs {
		m[j.ID] = j
	}
	return m, nil
}

func (c *Copier) namedNodes(ctx context.Context, vm VM) ([]string, error) {
	out, err := c.qmp(ctx, vm, "query-named-block-nodes", nil)
	if err != nil {
		return nil, err
	}
	var nodes []struct {
		NodeName string `json:"node-name"`
	}
	if err := json.Unmarshal(out, &nodes); err != nil {
		return nil, fmt.Errorf("parsing query-named-block-nodes reply: %w", err)
	}
	names := make([]string, 0, len(nodes))
	for _, n := range nodes {
		names = append(names, n.NodeName)
	}
	return names, nil
}

// qmp sends one QMP command through virsh and returns the "return" value.
func (c *Copier) qmp(ctx context.Context, vm VM, command string, args map[string]any) (json.RawMessage, error) {
	req := map[string]any{"execute": command}
	if args != nil {
		req["arguments"] = args
	}
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	out, err := c.run(ctx, vm, "virsh", "qemu-monitor-command", vm.domain(), string(b))
	if err != nil {
		return nil, err
	}
	return parseQMPReply(command, out)
}

// parseQMPReply returns the "return" value of a QMP reply, or its error.
func parseQMPReply(command, out string) (json.RawMessage, error) {
	var reply struct {
		Return json.RawMessage `json:"return"`
		Error  *struct {
			Class string `json:"class"`
			Desc  string `json:"desc"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &reply); err != nil {
		return nil, fmt.Errorf("parsing reply to %s: %w", command, err)
	}
	if reply.Error != nil {
		return nil, fmt.Errorf("%s: %s", command, reply.Error.Desc)
	}
	return reply.Return, nil
}

// run executes a command in the compute container. The error includes stderr.
func (c *Copier) run(ctx context.Context, vm VM, args ...string) (string, error) {
	stdout, stderr, err := c.exec.Exec(ctx, vm.Namespace, vm.Pod, container, args)
	if err != nil {
		if s := strings.TrimSpace(stderr); s != "" {
			return stdout, fmt.Errorf("%s: %w: %s", args[0], err, s)
		}
		return stdout, fmt.Errorf("%s: %w", args[0], err)
	}
	return stdout, nil
}
