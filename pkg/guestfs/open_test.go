package guestfs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/qcow2"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/replication"
)

func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "gfs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

func backupDisk(t *testing.T) string {
	t.Helper()
	disk := filepath.Join(t.TempDir(), "disk.img")
	if err := os.WriteFile(disk, make([]byte, 1<<20), 0o660); err != nil {
		t.Fatal(err)
	}
	return disk
}

func TestOpen_ServesTheDiskAndListsTheFileSystems(t *testing.T) {
	sock := filepath.Join(shortDir(t), "nbd.sock")
	f := newFake()
	uri := "nbd+unix:///?socket=" + sock
	f.outputs[key([]string{"virt-filesystems", "--format=raw", "-a", uri, "--filesystems", "--extra", "--long", "--csv"})] = []byte(filesystemsCSV)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o, err := Open(ctx, OpenOptions{Disk: backupDisk(t), Socket: sock, Runner: f})
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	b := make([]byte, 8)
	if _, err = conn.Read(b); err != nil || string(b) != "NBDMAGIC" {
		t.Fatalf("the socket does not speak NBD: %q %v", b, err)
	}
	if page, err := o.Tree.List(nil, nil, 10); err != nil || len(page.Entries) == 0 {
		t.Fatalf("%v %+v", err, page)
	}
	if !o.Info.Guest || !o.Info.Latest {
		t.Errorf("info = %+v", o.Info)
	}
}

func TestOpen_FailsInWordsWhenTheAppliancePartFails(t *testing.T) {
	sock := filepath.Join(shortDir(t), "nbd.sock")
	_, err := Open(context.Background(), OpenOptions{Disk: backupDisk(t), Socket: sock, Runner: newFake()})
	if err == nil || !strings.Contains(err.Error(), "file systems") {
		t.Fatalf("err = %v", err)
	}
	if _, serr := os.Stat(sock); serr == nil {
		t.Error("the socket was left behind")
	}
}

func TestOpen_WaitsForACopyToEnd(t *testing.T) {
	dir := t.TempDir()
	disk := filepath.Join(dir, "disk.img")
	if err := os.WriteFile(disk, make([]byte, 1<<20), 0o660); err != nil {
		t.Fatal(err)
	}
	undo := filepath.Join(dir, ".repl-undo")
	if err := os.MkdirAll(undo, 0o770); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(undo, "0000000001.undo.ready")
	if err := os.WriteFile(ready, []byte("x"), 0o660); err != nil {
		t.Fatal(err)
	}
	sleeps := 0
	_, err := Open(context.Background(), OpenOptions{Disk: disk, Socket: filepath.Join(shortDir(t), "s"), Runner: newFake(),
		Wait: time.Minute, Poll: time.Second, Sleep: func(time.Duration) { sleeps++; _ = os.Remove(ready) }})
	if err == nil || strings.Contains(err.Error(), "copy is being written") {
		t.Fatalf("the open must go on after the copy ended (and then fail for the fake runner): %v", err)
	}
	if sleeps != 1 {
		t.Errorf("slept %d times", sleeps)
	}
}

func TestOpen_GivesUpWaitingForACopy(t *testing.T) {
	dir := t.TempDir()
	disk := filepath.Join(dir, "disk.img")
	_ = os.WriteFile(disk, make([]byte, 1<<20), 0o660)
	_ = os.MkdirAll(filepath.Join(dir, ".repl-undo"), 0o770)
	_ = os.WriteFile(filepath.Join(dir, ".repl-undo", "0000000001.undo.ready"), []byte("x"), 0o660)
	_, err := Open(context.Background(), OpenOptions{Disk: disk, Socket: filepath.Join(shortDir(t), "s"), Runner: newFake(),
		Wait: 3 * time.Second, Poll: time.Second, Sleep: func(time.Duration) {}})
	if !errors.Is(err, replication.ErrCopyInProgress) {
		t.Fatalf("err = %v", err)
	}
}

func TestOpen_SocketNameMustBePlain(t *testing.T) {
	for _, s := range []string{"/tmp/a?b", "/tmp/a b", "/tmp/a#b", "/tmp/a%20b", "rel/path", ""} {
		if _, err := Open(context.Background(), OpenOptions{Disk: backupDisk(t), Socket: s, Runner: newFake()}); err == nil {
			t.Errorf("%q must be refused", s)
		}
	}
}

func TestMoments_ListsTheRestorePointsAndTheLatest(t *testing.T) {
	disk := backupDisk(t)
	t0 := time.Date(2026, 10, 20, 10, 0, 0, 500, time.UTC)
	t1 := t0.Add(time.Hour)
	if _, err := replication.ApplyCopy(emptyCopy(1<<20), disk, replication.ApplyOptions{From: t0, To: t1}); err != nil {
		t.Fatal(err)
	}
	names, err := Moments(disk)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "20261020T100000Z" || names[1] != "20261020T110000Z" {
		t.Fatalf("moments = %v", names)
	}
}

func TestAtEndOfSecond(t *testing.T) {
	// A restore point at 10:00:00.5 is picked by the moment 20261020T100000Z.
	at := AtEndOfSecond(time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC))
	if p := time.Date(2026, 10, 20, 10, 0, 0, 500, time.UTC); p.After(at) {
		t.Errorf("%v is after %v", p, at)
	}
	if next := time.Date(2026, 10, 20, 10, 0, 1, 0, time.UTC); !next.After(at) {
		t.Errorf("%v must be after %v", next, at)
	}
	if !AtEndOfSecond(time.Time{}).IsZero() {
		t.Error("the zero time asks for the latest")
	}
}

// emptyCopySource is a copy that writes one block of 0xaa at the start.
type emptyCopySource struct{ size int64 }

func emptyCopy(size int64) replication.CopySource { return &emptyCopySource{size} }

func (e *emptyCopySource) Size() int64 { return e.size }
func (e *emptyCopySource) Walk(fn func(qcow2.Extent) error) error {
	return fn(qcow2.Extent{Offset: 0, Length: 4096, Kind: qcow2.Data})
}
func (e *emptyCopySource) DataReader(qcow2.Extent) io.Reader {
	return bytes.NewReader(bytes.Repeat([]byte{0xaa}, 4096))
}
