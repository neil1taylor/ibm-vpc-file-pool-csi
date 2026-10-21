package vmcopy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeLauncher simulates the commands the copier runs in a virt-launcher
// compute container: virt-freezer, qemu-img, rm, and QMP through virsh.
type fakeLauncher struct {
	mu sync.Mutex

	disks   map[string]int64           // volume -> virtual size; the VM's disks
	files   map[string]bool            // files that exist in the volume dirs
	nodes   map[string]bool            // block nodes added by blockdev-add
	bitmaps map[string]map[string]bool // dirty bitmaps per disk node
	jobs    map[string]*fakeJob

	moment       time.Time
	frozen       bool
	freezeErr    string // virt-freezer --freeze fails with this
	transErr     string // the transaction fails with this
	log          []string
	frozenAtTxn  bool // the guest was frozen when the transaction ran
	txnActions   []map[string]any
	unfreezeSeen int
}

type fakeJob struct {
	status string // running, concluded
	err    string
}

func newFakeLauncher() *fakeLauncher {
	return &fakeLauncher{
		disks:   map[string]int64{"rootdisk": 21474836480, "datadisk": 10737418240},
		files:   map[string]bool{},
		nodes:   map[string]bool{},
		bitmaps: map[string]map[string]bool{},
		jobs:    map[string]*fakeJob{},

		moment: time.Date(2026, 10, 20, 12, 0, 0, 123, time.UTC),
	}
}

func (f *fakeLauncher) volumeNames() []string {
	var names []string
	for v := range f.disks {
		names = append(names, v)
	}
	sort.Strings(names)
	return names
}

// nodeOf returns the disk node of a volume: libvirt-<n>-format, numbered
// in volume name order.
func (f *fakeLauncher) nodeOf(vol string) string {
	for i, v := range f.volumeNames() {
		if v == vol {
			return fmt.Sprintf("libvirt-%d-format", i+1)
		}
	}
	return ""
}

func diskPath(vol string) string { return "/var/run/kubevirt-private/vmi-disks/" + vol + "/disk.img" }

func (f *fakeLauncher) Exec(_ context.Context, ns, pod, container string, cmd []string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ns != "vms" || pod != "virt-launcher-web-abcde" || container != "compute" {
		return "", "", fmt.Errorf("unexpected target %s/%s/%s", ns, pod, container)
	}
	f.log = append(f.log, strings.Join(cmd, " "))
	switch cmd[0] {
	case "virt-freezer":
		if cmd[1] == "--freeze" {
			if !hasArg(cmd, "--name=web") || !hasArg(cmd, "--namespace=vms") || !hasArg(cmd, "--unfreezeTimeoutSeconds=30") {
				return "", "", fmt.Errorf("bad freeze args %v", cmd)
			}
			if f.freezeErr != "" {
				return "", f.freezeErr, errors.New("command terminated with exit code 1")
			}
			f.frozen = true
			return "", "", nil
		}
		f.unfreezeSeen++
		f.frozen = false
		return "", "", nil
	case "bash":
		// bash -c <freeze script> repl-freeze <vmi> <ns> <domain> <transaction>
		if len(cmd) != 8 || cmd[1] != "-c" || cmd[3] != "repl-freeze" || cmd[4] != "web" || cmd[5] != "vms" || cmd[6] != "vms_web" {
			return "", "", fmt.Errorf("bad freeze script args %v", cmd)
		}
		frc, fout := 0, ""
		if f.freezeErr != "" {
			frc, fout = 1, f.freezeErr
		} else {
			f.frozen = true
		}
		tout, _, _ := f.qmp(cmd[7])
		f.unfreezeSeen++
		f.frozen = false
		b64 := base64.StdEncoding.EncodeToString
		return fmt.Sprintf("%d\n0\n0\n%d\n%d\n%s\n%s\n%s\n", frc, f.moment.UnixNano(), int64(170*time.Millisecond),
			b64([]byte(fout)), b64([]byte(tout)), b64(nil)), "", nil
	case "qemu-img":
		// qemu-img create -q -f qcow2 <path> <size>
		if len(cmd) != 7 || cmd[1] != "create" || cmd[4] != "qcow2" {
			return "", "", fmt.Errorf("bad qemu-img args %v", cmd)
		}
		f.files[cmd[5]] = true
		return "", "", nil
	case "rm":
		// rm -f -- <paths...>
		for _, p := range cmd[3:] {
			delete(f.files, p)
		}
		return "", "", nil
	case "find":
		// find <dir> -maxdepth 1 -name .repl-staging-*.qcow2 -delete
		for p := range f.files {
			if strings.HasPrefix(p, cmd[1]+"/.repl-staging-") {
				delete(f.files, p)
			}
		}
		return "", "", nil
	case "virsh":
		if cmd[1] != "qemu-monitor-command" || cmd[2] != "vms_web" {
			return "", "", fmt.Errorf("bad virsh args %v", cmd)
		}
		return f.qmp(cmd[3])
	}
	return "", "", fmt.Errorf("unknown command %v", cmd)
}

func hasArg(cmd []string, a string) bool {
	for _, c := range cmd {
		if c == a {
			return true
		}
	}
	return false
}

func qmpErr(desc string) (string, string, error) {
	return fmt.Sprintf(`{"id":"libvirt-1","error":{"class":"GenericError","desc":%q}}`, desc), "", nil
}

func (f *fakeLauncher) qmp(in string) (string, string, error) {
	var req struct {
		Execute   string         `json:"execute"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(in), &req); err != nil {
		return "", "", fmt.Errorf("bad QMP JSON %q: %v", in, err)
	}
	ok := `{"id":"libvirt-1","return":{}}`
	switch req.Execute {
	case "query-block":
		var devs []map[string]any
		for _, vol := range f.volumeNames() {
			node := f.nodeOf(vol)
			var bms []map[string]any
			for name := range f.bitmaps[node] {
				bms = append(bms, map[string]any{"name": name, "recording": true, "granularity": 65536})
			}
			devs = append(devs, map[string]any{
				"device": "", "qdev": "ua-" + vol,
				"inserted": map[string]any{
					"node-name":     node,
					"file":          diskPath(vol),
					"image":         map[string]any{"virtual-size": f.disks[vol]},
					"dirty-bitmaps": bms,
				},
			})
		}
		devs = append(devs, map[string]any{"device": "", "qdev": "ua-cloudinit",
			"inserted": map[string]any{"node-name": "libvirt-9-format", "file": "/var/run/kubevirt-ephemeral-disks/cloud-init-data/vms/web/noCloud.iso", "image": map[string]any{"virtual-size": 374784}}})
		b, _ := json.Marshal(map[string]any{"return": devs})
		return string(b), "", nil
	case "query-named-block-nodes":
		var nodes []map[string]any
		for n := range f.nodes {
			nodes = append(nodes, map[string]any{"node-name": n})
		}
		nodes = append(nodes, map[string]any{"node-name": "libvirt-1-format"})
		b, _ := json.Marshal(map[string]any{"return": nodes})
		return string(b), "", nil
	case "blockdev-add":
		name, _ := req.Arguments["node-name"].(string)
		fileArg, _ := req.Arguments["file"].(map[string]any)
		file, _ := fileArg["filename"].(string)
		if !f.files[file] {
			return qmpErr("Could not open '" + file + "': No such file or directory")
		}
		if req.Arguments["driver"] != "qcow2" || req.Arguments["detect-zeroes"] != "unmap" || req.Arguments["backing"] != nil {
			return qmpErr(fmt.Sprintf("unexpected blockdev-add arguments %v", req.Arguments))
		}
		f.nodes[name] = true
		return ok, "", nil
	case "block-dirty-bitmap-remove":
		node, _ := req.Arguments["node"].(string)
		name, _ := req.Arguments["name"].(string)
		if !f.bitmaps[node][name] {
			return qmpErr("Dirty bitmap '" + name + "' not found")
		}
		delete(f.bitmaps[node], name)
		return ok, "", nil
	case "blockdev-del":
		name, _ := req.Arguments["node-name"].(string)
		if !f.nodes[name] {
			return qmpErr("Failed to find node with node-name='" + name + "'")
		}
		delete(f.nodes, name)
		return ok, "", nil
	case "transaction":
		f.frozenAtTxn = f.frozen
		if f.transErr != "" {
			return qmpErr(f.transErr)
		}
		var txn struct {
			Arguments struct {
				Actions []map[string]any `json:"actions"`
			} `json:"arguments"`
		}
		if err := json.Unmarshal([]byte(in), &txn); err != nil {
			return "", "", err
		}
		for _, act := range txn.Arguments.Actions {
			data, _ := act["data"].(map[string]any)
			f.txnActions = append(f.txnActions, act)
			if act["type"] == "block-dirty-bitmap-add" {
				node, _ := data["node"].(string)
				name, _ := data["name"].(string)
				if data["persistent"] != false {
					return qmpErr("expected a non-persistent bitmap")
				}
				if f.bitmaps[node] == nil {
					f.bitmaps[node] = map[string]bool{}
				}
				f.bitmaps[node][name] = true
				continue
			}
			target, _ := data["target"].(string)
			job, _ := data["job-id"].(string)
			if !f.nodes[target] {
				return qmpErr("target missing")
			}
			if bm, _ := data["bitmap"].(string); bm != "" {
				dev, _ := data["device"].(string)
				if !f.bitmaps[dev][bm] {
					return qmpErr("Bitmap '" + bm + "' could not be found")
				}
			}
			f.jobs[job] = &fakeJob{status: "running"}
		}
		return ok, "", nil
	case "query-jobs":
		var jobs []map[string]any
		for id, j := range f.jobs {
			m := map[string]any{"id": id, "type": "backup", "status": j.status, "current-progress": 1, "total-progress": 2}
			if j.err != "" {
				m["error"] = j.err
			}
			jobs = append(jobs, m)
		}
		b, _ := json.Marshal(map[string]any{"return": jobs})
		return string(b), "", nil
	case "block-job-cancel":
		id, _ := req.Arguments["device"].(string)
		j, ok2 := f.jobs[id]
		if !ok2 {
			return qmpErr("Block job '" + id + "' not found")
		}
		j.status, j.err = "concluded", "Operation canceled"
		return ok, "", nil
	case "job-dismiss":
		id, _ := req.Arguments["id"].(string)
		if j, ok2 := f.jobs[id]; !ok2 || j.status != "concluded" {
			return qmpErr("job not concluded")
		}
		delete(f.jobs, id)
		return ok, "", nil
	}
	return qmpErr("unknown command " + req.Execute)
}

func (f *fakeLauncher) finishJobs(err string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, j := range f.jobs {
		j.status, j.err = "concluded", err
	}
}

var testVM = VM{Namespace: "vms", Pod: "virt-launcher-web-abcde", Name: "web"}

func newTestCopier(f *fakeLauncher) *Copier {
	c := NewCopier(f)
	c.now = func() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC) }
	c.cancelWait = 0
	return c
}

func TestStart_CopiesAllDisksAtOneMoment(t *testing.T) {
	f := newFakeLauncher()
	c := newTestCopier(f)

	cp, err := c.Start(context.Background(), testVM, []string{"rootdisk", "datadisk"}, StartOptions{Speed: 20 << 20, ID: "a1"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !cp.FileSystemConsistent || cp.FreezeError != "" {
		t.Errorf("expected a file-system consistent copy, got %+v", cp)
	}
	if !cp.Moment.Equal(f.moment) {
		t.Errorf("Moment = %v, want the pod's time of the transaction %v", cp.Moment, f.moment)
	}
	if cp.Paused != 170*time.Millisecond {
		t.Errorf("Paused = %v", cp.Paused)
	}
	if !f.frozenAtTxn {
		t.Error("expected the guest to be frozen when the copy started")
	}
	if f.frozen || f.unfreezeSeen != 1 {
		t.Errorf("expected exactly one thaw after the copy started (frozen=%v, thaws=%d)", f.frozen, f.unfreezeSeen)
	}
	backups := 0
	for _, a := range f.txnActions {
		if a["type"] != "blockdev-backup" {
			continue
		}
		backups++
		d, _ := a["data"].(map[string]any)
		if d["sync"] != "full" || d["auto-dismiss"] != false || d["speed"] != float64(20<<20) {
			t.Errorf("unexpected action %v", a)
		}
	}
	if backups != 2 {
		t.Fatalf("expected one transaction with 2 disk copies, got %d", backups)
	}
	if len(cp.Disks) != 2 || cp.Disks[0].Volume != "rootdisk" || cp.Disks[1].Volume != "datadisk" {
		t.Fatalf("Disks = %+v", cp.Disks)
	}
	for _, d := range cp.Disks {
		want := "/var/run/kubevirt-private/vmi-disks/" + d.Volume + "/" + d.StagingFile
		if d.StagingFile != ".repl-staging-a1.qcow2" || !f.files[want] {
			t.Errorf("staging file for %s: %q, exists=%v", d.Volume, d.StagingFile, f.files[want])
		}
	}
	// The freeze comes after the staging files are ready, and freeze,
	// transaction and thaw run in one command in the pod, to keep the pause
	// short.
	freezeAt, lastPrep := -1, -1
	for i, l := range f.log {
		if strings.HasPrefix(l, "virt-freezer") || strings.Contains(l, `"transaction"`) && !strings.HasPrefix(l, "bash") {
			t.Errorf("expected freeze, transaction and thaw in one command, got a separate %q", l)
		}
		if strings.HasPrefix(l, "bash -c") {
			freezeAt = i
		}
		if strings.HasPrefix(l, "qemu-img") || strings.Contains(l, "blockdev-add") {
			lastPrep = i
		}
	}
	if freezeAt < lastPrep {
		t.Errorf("expected the freeze after all preparation, log:\n%s", strings.Join(f.log, "\n"))
	}
}

func TestStart_FreezeFailsGivesCrashConsistentCopy(t *testing.T) {
	f := newFakeLauncher()
	f.freezeErr = "guest agent is not connected"
	c := newTestCopier(f)

	cp, err := c.Start(context.Background(), testVM, []string{"rootdisk"}, StartOptions{Speed: 0, ID: "a1"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if cp.FileSystemConsistent {
		t.Error("expected a crash-consistent copy")
	}
	if !strings.Contains(cp.FreezeError, "guest agent is not connected") {
		t.Errorf("FreezeError = %q", cp.FreezeError)
	}
	if f.unfreezeSeen != 1 {
		t.Errorf("expected a thaw even after a failed freeze, got %d", f.unfreezeSeen)
	}
}

func TestStart_TransactionFailsCleansUp(t *testing.T) {
	f := newFakeLauncher()
	f.transErr = "Bitmap already exists"
	c := newTestCopier(f)

	if _, err := c.Start(context.Background(), testVM, []string{"rootdisk", "datadisk"}, StartOptions{Speed: 0, ID: "a1"}); err == nil || !strings.Contains(err.Error(), "Bitmap already exists") {
		t.Fatalf("Start error = %v", err)
	}
	if f.frozen || f.unfreezeSeen != 1 {
		t.Errorf("expected the guest thawed (frozen=%v, thaws=%d)", f.frozen, f.unfreezeSeen)
	}
	if len(f.nodes) != 0 {
		t.Errorf("expected target nodes removed, got %v", f.nodes)
	}
	if len(f.files) != 0 {
		t.Errorf("expected staging files removed, got %v", f.files)
	}
}

func TestStart_RefusesUnknownVolume(t *testing.T) {
	f := newFakeLauncher()
	c := newTestCopier(f)
	if _, err := c.Start(context.Background(), testVM, []string{"rootdisk", "hotplugged"}, StartOptions{Speed: 0, ID: "a1"}); err == nil || !strings.Contains(err.Error(), "hotplugged") {
		t.Fatalf("Start error = %v, want one naming the volume", err)
	}
	if f.unfreezeSeen != 0 || len(f.files) != 0 {
		t.Errorf("expected nothing done before the check (thaws=%d, files=%v)", f.unfreezeSeen, f.files)
	}
}

func TestStart_RefusesBadNames(t *testing.T) {
	c := newTestCopier(newFakeLauncher())
	for _, tc := range []struct {
		vols []string
		id   string
	}{
		{[]string{"../etc"}, "a1"},
		{[]string{"root disk"}, "a1"},
		{[]string{"rootdisk"}, "A1;rm"},
		{[]string{"rootdisk"}, ""},
		{nil, "a1"},
	} {
		if _, err := c.Start(context.Background(), testVM, tc.vols, StartOptions{Speed: 0, ID: tc.id}); err == nil {
			t.Errorf("Start(%v, %q) succeeded, want an error", tc.vols, tc.id)
		}
	}
}

func TestProgress(t *testing.T) {
	f := newFakeLauncher()
	c := newTestCopier(f)
	ctx := context.Background()
	cp, err := c.Start(ctx, testVM, []string{"rootdisk", "datadisk"}, StartOptions{Speed: 0, ID: "a1"})
	if err != nil {
		t.Fatal(err)
	}

	if st, _, perr := c.Progress(ctx, testVM, cp); perr != nil || st != Running {
		t.Fatalf("Progress = %v, %v; want Running", st, perr)
	}

	f.finishJobs("")
	if st, _, perr := c.Progress(ctx, testVM, cp); perr != nil || st != Done {
		t.Fatalf("Progress = %v, %v; want Done", st, perr)
	}

	f.mu.Lock()
	f.jobs["repl-a1-1"].err = "No space left on device"
	f.mu.Unlock()
	st, msg, err := c.Progress(ctx, testVM, cp)
	if err != nil || st != Failed || !strings.Contains(msg, "No space left on device") {
		t.Fatalf("Progress = %v, %q, %v; want Failed with the QEMU error", st, msg, err)
	}

	f.mu.Lock()
	f.jobs = map[string]*fakeJob{}
	f.mu.Unlock()
	if st, _, err := c.Progress(ctx, testVM, cp); err != nil || st != Lost {
		t.Fatalf("Progress = %v, %v; want Lost when QEMU has no jobs", st, err)
	}
}

func TestFinish_KeepsOrRemovesStaging(t *testing.T) {
	for _, remove := range []bool{false, true} {
		f := newFakeLauncher()
		c := newTestCopier(f)
		ctx := context.Background()
		cp, err := c.Start(ctx, testVM, []string{"rootdisk", "datadisk"}, StartOptions{Speed: 0, ID: "a1"})
		if err != nil {
			t.Fatal(err)
		}
		f.finishJobs("")
		if err := c.Finish(ctx, testVM, cp, remove); err != nil {
			t.Fatalf("Finish(remove=%v): %v", remove, err)
		}
		if len(f.jobs) != 0 || len(f.nodes) != 0 {
			t.Errorf("Finish(remove=%v): jobs %v, nodes %v left", remove, f.jobs, f.nodes)
		}
		if got := len(f.files); (remove && got != 0) || (!remove && got != 2) {
			t.Errorf("Finish(remove=%v): %d staging files left", remove, got)
		}
		// Finish again, as after a controller restart: no error.
		if err := c.Finish(ctx, testVM, cp, remove); err != nil {
			t.Errorf("second Finish(remove=%v): %v", remove, err)
		}
	}
}

func TestCancel(t *testing.T) {
	f := newFakeLauncher()
	c := newTestCopier(f)
	ctx := context.Background()
	cp, err := c.Start(ctx, testVM, []string{"rootdisk", "datadisk"}, StartOptions{Speed: 0, ID: "a1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Cancel(ctx, testVM, cp); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if len(f.jobs) != 0 || len(f.nodes) != 0 || len(f.files) != 0 {
		t.Errorf("after Cancel: jobs %v, nodes %v, files %v", f.jobs, f.nodes, f.files)
	}
}

func TestRemoveStale(t *testing.T) {
	f := newFakeLauncher()
	c := newTestCopier(f)
	ctx := context.Background()
	f.files["/var/run/kubevirt-private/vmi-disks/rootdisk/.repl-staging-old.qcow2"] = true
	f.files["/var/run/kubevirt-private/vmi-disks/rootdisk/disk.img"] = true
	f.nodes["repl-old-t0"] = true

	if err := c.RemoveStale(ctx, testVM, []string{"rootdisk"}, ""); err != nil {
		t.Fatalf("RemoveStale: %v", err)
	}
	if f.files["/var/run/kubevirt-private/vmi-disks/rootdisk/.repl-staging-old.qcow2"] || f.nodes["repl-old-t0"] {
		t.Errorf("expected the stale staging file and node removed: files %v nodes %v", f.files, f.nodes)
	}
	if !f.files["/var/run/kubevirt-private/vmi-disks/rootdisk/disk.img"] {
		t.Error("RemoveStale removed the disk")
	}

	// It must not touch a copy that is still running.
	if _, err := c.Start(ctx, testVM, []string{"rootdisk"}, StartOptions{Speed: 0, ID: "b2"}); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveStale(ctx, testVM, []string{"rootdisk"}, ""); err == nil {
		t.Error("expected RemoveStale to refuse while a copy runs")
	}
	if !f.files["/var/run/kubevirt-private/vmi-disks/rootdisk/.repl-staging-b2.qcow2"] {
		t.Error("RemoveStale removed the staging file of a running copy")
	}
}

func (f *fakeLauncher) hasBitmap(vol, name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bitmaps[f.nodeOf(vol)][name]
}

func TestStart_FullCopyStartsAChangeList(t *testing.T) {
	f := newFakeLauncher()
	c := newTestCopier(f)
	cp, err := c.Start(context.Background(), testVM, []string{"rootdisk", "datadisk"}, StartOptions{ID: "a1"})
	if err != nil {
		t.Fatal(err)
	}
	if cp.Bitmap != "repl-a1" || cp.Base != "" || cp.Incremental {
		t.Errorf("Copy = %+v, want bitmap repl-a1 and a full copy", cp)
	}
	for _, v := range []string{"rootdisk", "datadisk"} {
		if !f.hasBitmap(v, "repl-a1") {
			t.Errorf("expected change list repl-a1 on %s", v)
		}
	}
	// The change lists start in the same transaction as the copies.
	var adds, backups int
	for _, a := range f.txnActions {
		d, _ := a["data"].(map[string]any)
		switch a["type"] {
		case "block-dirty-bitmap-add":
			adds++
		case "blockdev-backup":
			backups++
			if d["sync"] != "full" {
				t.Errorf("full copy action = %v", a)
			}
		}
	}
	if adds != 2 || backups != 2 {
		t.Errorf("transaction had %d bitmap adds and %d backups, want 2 and 2", adds, backups)
	}
}

func TestStart_ChangeCopyUsesTheBaseList(t *testing.T) {
	f := newFakeLauncher()
	c := newTestCopier(f)
	ctx := context.Background()
	first, err := c.Start(ctx, testVM, []string{"rootdisk", "datadisk"}, StartOptions{ID: "a1"})
	if err != nil {
		t.Fatal(err)
	}
	f.finishJobs("")
	if ferr := c.Finish(ctx, testVM, first, false); ferr != nil {
		t.Fatal(ferr)
	}
	f.txnActions = nil

	cp, err := c.Start(ctx, testVM, []string{"rootdisk", "datadisk"}, StartOptions{ID: "b2", Base: "repl-a1"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if cp.Bitmap != "repl-b2" || cp.Base != "repl-a1" || !cp.Incremental {
		t.Errorf("Copy = %+v", cp)
	}
	for _, a := range f.txnActions {
		d, _ := a["data"].(map[string]any)
		if a["type"] == "blockdev-backup" && (d["sync"] != "bitmap" || d["bitmap"] != "repl-a1" || d["bitmap-mode"] != "never") {
			t.Errorf("change copy action = %v, want sync bitmap from repl-a1 that keeps the list", a)
		}
	}
	// Both lists exist until the change copy is committed.
	if !f.hasBitmap("rootdisk", "repl-a1") || !f.hasBitmap("rootdisk", "repl-b2") {
		t.Error("expected both change lists while the change copy is not committed")
	}

	f.finishJobs("")
	if err := c.Finish(ctx, testVM, cp, false); err != nil {
		t.Fatal(err)
	}
	if err := c.Commit(ctx, testVM, cp); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if f.hasBitmap("rootdisk", "repl-a1") || !f.hasBitmap("rootdisk", "repl-b2") {
		t.Error("after Commit, expected only the new change list")
	}
	if err := c.Commit(ctx, testVM, cp); err != nil {
		t.Errorf("a second Commit must succeed, got %v", err)
	}
}

func TestStart_BaseListLost(t *testing.T) {
	f := newFakeLauncher()
	c := newTestCopier(f)
	_, err := c.Start(context.Background(), testVM, []string{"rootdisk"}, StartOptions{ID: "b2", Base: "repl-a1"})
	if !errors.Is(err, ErrBaseLost) {
		t.Fatalf("Start error = %v, want ErrBaseLost", err)
	}
	if len(f.files) != 0 || f.unfreezeSeen != 0 {
		t.Errorf("expected nothing done (files %v, thaws %d)", f.files, f.unfreezeSeen)
	}
}

func TestFailedChangeCopyKeepsTheBaseList(t *testing.T) {
	for _, how := range []string{"finish", "cancel"} {
		f := newFakeLauncher()
		c := newTestCopier(f)
		ctx := context.Background()
		first, err := c.Start(ctx, testVM, []string{"rootdisk"}, StartOptions{ID: "a1"})
		if err != nil {
			t.Fatal(err)
		}
		f.finishJobs("")
		if ferr := c.Finish(ctx, testVM, first, false); ferr != nil {
			t.Fatal(ferr)
		}
		cp, err := c.Start(ctx, testVM, []string{"rootdisk"}, StartOptions{ID: "b2", Base: "repl-a1"})
		if err != nil {
			t.Fatal(err)
		}
		if how == "finish" {
			f.finishJobs("No space left on device")
			err = c.Finish(ctx, testVM, cp, true)
		} else {
			err = c.Cancel(ctx, testVM, cp)
		}
		if err != nil {
			t.Fatalf("%s: %v", how, err)
		}
		if !f.hasBitmap("rootdisk", "repl-a1") || f.hasBitmap("rootdisk", "repl-b2") {
			t.Errorf("%s: expected the base list kept and the new list removed, got %v", how, f.bitmaps)
		}
	}
}

func TestRemoveStale_KeepsTheBaseList(t *testing.T) {
	f := newFakeLauncher()
	c := newTestCopier(f)
	node := f.nodeOf("rootdisk")
	f.bitmaps[node] = map[string]bool{"repl-a1": true, "repl-old": true, "backup-tool": true}
	if err := c.RemoveStale(context.Background(), testVM, []string{"rootdisk"}, "repl-a1"); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"repl-a1": true, "backup-tool": true}
	if fmt.Sprint(f.bitmaps[node]) != fmt.Sprint(want) {
		t.Errorf("bitmaps = %v, want %v", f.bitmaps[node], want)
	}
}

func TestCheckIdle_ReportsABusyVMAndTouchesNothing(t *testing.T) {
	f := newFakeLauncher()
	c := newTestCopier(f)
	ctx := context.Background()
	f.files["/var/run/kubevirt-private/vmi-disks/rootdisk/.repl-staging-old.qcow2"] = true
	f.nodes["repl-old-t0"] = true
	if err := c.CheckIdle(ctx, testVM); err != nil {
		t.Fatalf("CheckIdle on an idle VM: %v", err)
	}
	if !f.files["/var/run/kubevirt-private/vmi-disks/rootdisk/.repl-staging-old.qcow2"] || !f.nodes["repl-old-t0"] {
		t.Error("CheckIdle must only look: it removed something")
	}
	if _, err := c.Start(ctx, testVM, []string{"rootdisk"}, StartOptions{ID: "b2"}); err != nil {
		t.Fatal(err)
	}
	if err := c.CheckIdle(ctx, testVM); !errors.Is(err, ErrBusy) {
		t.Errorf("CheckIdle while a copy runs = %v, want ErrBusy", err)
	}
}

func TestRemoveStale_RefusalIsErrBusy(t *testing.T) {
	f := newFakeLauncher()
	c := newTestCopier(f)
	ctx := context.Background()
	if _, err := c.Start(ctx, testVM, []string{"rootdisk"}, StartOptions{ID: "b2"}); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveStale(ctx, testVM, []string{"rootdisk"}, ""); !errors.Is(err, ErrBusy) {
		t.Errorf("RemoveStale while a copy runs = %v, want ErrBusy", err)
	}
}

func TestCleanStaleCopies_KeepsEveryChangeList(t *testing.T) {
	f := newFakeLauncher()
	c := newTestCopier(f)
	ctx := context.Background()
	node := f.nodeOf("rootdisk")
	f.bitmaps[node] = map[string]bool{"repl-a1": true, "repl-old": true}
	f.files["/var/run/kubevirt-private/vmi-disks/rootdisk/.repl-staging-old.qcow2"] = true
	f.files["/var/run/kubevirt-private/vmi-disks/rootdisk/disk.img"] = true
	f.nodes["repl-old-t0"] = true

	if err := c.CleanStaleCopies(ctx, testVM, []string{"rootdisk"}); err != nil {
		t.Fatalf("CleanStaleCopies: %v", err)
	}
	if f.files["/var/run/kubevirt-private/vmi-disks/rootdisk/.repl-staging-old.qcow2"] || f.nodes["repl-old-t0"] {
		t.Errorf("the stale staging file and node must go: files %v nodes %v", f.files, f.nodes)
	}
	if !f.files["/var/run/kubevirt-private/vmi-disks/rootdisk/disk.img"] {
		t.Error("the disk was removed")
	}
	if want := map[string]bool{"repl-a1": true, "repl-old": true}; fmt.Sprint(f.bitmaps[node]) != fmt.Sprint(want) {
		t.Errorf("bitmaps = %v, want all kept: the change list of replication is not ours to remove", f.bitmaps[node])
	}

	if _, err := c.Start(ctx, testVM, []string{"rootdisk"}, StartOptions{ID: "b2"}); err != nil {
		t.Fatal(err)
	}
	if err := c.CleanStaleCopies(ctx, testVM, []string{"rootdisk"}); !errors.Is(err, ErrBusy) {
		t.Errorf("CleanStaleCopies while a copy runs = %v, want ErrBusy", err)
	}
	if !f.files["/var/run/kubevirt-private/vmi-disks/rootdisk/.repl-staging-b2.qcow2"] {
		t.Error("the staging file of a running copy was removed")
	}
}
