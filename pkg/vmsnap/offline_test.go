package vmsnap

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The offline scripts run in a Job with qemu-img when the VM is stopped. These
// tests run them with a stub qemu-img that records its calls, so they check
// the order of the steps and that a repeat is safe.

func stubQemuImg(t *testing.T) (binDir, logFile string) {
	t.Helper()
	binDir = t.TempDir()
	logFile = filepath.Join(t.TempDir(), "calls")
	// The stub keeps a "backing" file next to each image it makes, so
	// "info" can report it. commit leaves the top file in place, as qemu-img does.
	stub := `#!/bin/sh
echo "$*" >> "` + logFile + `"
case "$1" in
create) eval last=\${$#}; printf '%s' "$8" > "$last.backing"; : > "$last" ;;
info) eval last=\${$#}; b=$(cat "$last.backing" 2>/dev/null); printf '{"backing-filename": "%s"}\n' "$b" ;;
rebase) eval last=\${$#}; printf '%s' "$6" > "$last.backing" ;;
convert) eval last=\${$#}; : > "$last" ;;
commit) ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "qemu-img"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	return binDir, logFile
}

func runScript(t *testing.T, binDir, script string) (string, error) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func calls(t *testing.T, logFile string) string {
	t.Helper()
	b, _ := os.ReadFile(logFile)
	return string(b)
}

func mode(t *testing.T, p string) string {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil {
		return "missing"
	}
	return st.Mode().Perm().String()
}

func TestOfflineTakeScript(t *testing.T) {
	bin, log := stubQemuImg(t)
	vol := t.TempDir()
	if err := os.WriteFile(filepath.Join(vol, "disk.img"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	script, err := OfflineTakeScript(vol, File{Name: "disk.img", Format: "raw"}, "snap-1.qcow2")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // a repeat is safe
		if out, err := runScript(t, bin, script); err != nil {
			t.Fatalf("run %d: %v\n%s", i, err, out)
		}
	}
	if got := strings.Count(calls(t, log), "create -q -f qcow2 -F raw -b disk.img "+vol+"/snap-1.qcow2"); got != 1 {
		t.Errorf("expected one create naming the backing file by name, calls:\n%s", calls(t, log))
	}
	if m := mode(t, filepath.Join(vol, "disk.img")); m != "-r--r--r--" {
		t.Errorf("disk.img mode %s, want read-only", m)
	}
	if _, err := OfflineTakeScript(vol, File{Name: "disk.img", Format: "raw"}, "../x"); err == nil {
		t.Error("accepted a bad file name")
	}
}

func TestOfflineMergeScript_MiddleFile(t *testing.T) {
	bin, log := stubQemuImg(t)
	vol := t.TempDir()
	for _, f := range []string{"disk.img", "snap-a.qcow2", "snap-b.qcow2", "snap-c.qcow2"} {
		if err := os.WriteFile(filepath.Join(vol, f), nil, 0o444); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(vol, "snap-b.qcow2.backing"), []byte("snap-a.qcow2"), 0o644)
	_ = os.Chmod(filepath.Join(vol, "snap-c.qcow2"), 0o644)
	script, err := OfflineMergeScript(vol, "snap-a.qcow2", "snap-b.qcow2", "snap-c.qcow2")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if out, err := runScript(t, bin, script); err != nil {
			t.Fatalf("run %d: %v\n%s", i, err, out)
		}
	}
	c := calls(t, log)
	if strings.Count(c, "commit -q "+vol+"/snap-b.qcow2") != 1 || strings.Count(c, "rebase -u -F qcow2 -b snap-a.qcow2 "+vol+"/snap-c.qcow2") != 1 {
		t.Errorf("calls:\n%s", c)
	}
	if strings.Index(c, "commit") > strings.Index(c, "rebase") {
		t.Errorf("the commit must come before the rebase:\n%s", c)
	}
	if m := mode(t, filepath.Join(vol, "snap-b.qcow2")); m != "missing" {
		t.Errorf("snap-b must be deleted, mode %s", m)
	}
	if m := mode(t, filepath.Join(vol, "snap-a.qcow2")); m != "-r--r--r--" {
		t.Errorf("snap-a still holds a snapshot, mode %s", m)
	}
}

func TestOfflineMergeScript_TopFile(t *testing.T) {
	bin, _ := stubQemuImg(t)
	vol := t.TempDir()
	_ = os.WriteFile(filepath.Join(vol, "disk.img"), nil, 0o444)
	_ = os.WriteFile(filepath.Join(vol, "snap-a.qcow2"), nil, 0o644)
	_ = os.WriteFile(filepath.Join(vol, "snap-a.qcow2.backing"), []byte("disk.img"), 0o644)
	script, err := OfflineMergeScript(vol, "disk.img", "snap-a.qcow2", "")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := runScript(t, bin, script); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if m := mode(t, filepath.Join(vol, "disk.img")); m != "-rw-r--r--" {
		t.Errorf("disk.img is the live file again, mode %s", m)
	}
}

func TestOfflineMergeScript_RefusesAWrongBackingFile(t *testing.T) {
	bin, log := stubQemuImg(t)
	vol := t.TempDir()
	_ = os.WriteFile(filepath.Join(vol, "disk.img"), nil, 0o444)
	_ = os.WriteFile(filepath.Join(vol, "snap-a.qcow2"), nil, 0o644)
	_ = os.WriteFile(filepath.Join(vol, "snap-a.qcow2.backing"), []byte("snap-x.qcow2"), 0o644)
	script, _ := OfflineMergeScript(vol, "disk.img", "snap-a.qcow2", "")
	if _, err := runScript(t, bin, script); err == nil {
		t.Fatal("expected the script to fail")
	}
	if strings.Contains(calls(t, log), "commit") {
		t.Error("must not commit into a file that is not the backing file")
	}
}

func TestConvertScript(t *testing.T) {
	bin, log := stubQemuImg(t)
	src, dst := t.TempDir(), t.TempDir()
	script, err := ConvertScript(src, "snap-a.qcow2", dst)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := runScript(t, bin, script); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(calls(t, log), "convert -O raw "+src+"/snap-a.qcow2 "+dst+"/disk.img.part") {
		t.Errorf("calls:\n%s", calls(t, log))
	}
	if m := mode(t, filepath.Join(dst, "disk.img")); m == "missing" {
		t.Error("disk.img must exist after the convert")
	}
	if m := mode(t, filepath.Join(dst, "disk.img.part")); m != "missing" {
		t.Error("the part file must be renamed")
	}
}
