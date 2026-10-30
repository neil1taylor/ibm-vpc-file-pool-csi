//go:build linux

package guestfs

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A VM that was stopped when it was backed up is copied as plain files: the
// latest copy holds disk.img and each earlier moment is a folder of its own.
func TestOpen_AFileCopyOfADiskReadsTheMomentFolder(t *testing.T) {
	root := t.TempDir()
	latest := filepath.Join(root, "latest")
	moments := filepath.Join(root, "moments")
	for _, d := range []string{latest, filepath.Join(moments, "20261020T100000Z"), filepath.Join(moments, "20261021T100000Z")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "disk.img"), make([]byte, 1<<20), 0o660); err != nil {
			t.Fatal(err)
		}
	}
	sock := filepath.Join(shortDir(t), "nbd.sock")
	f := newFake()
	f.outputs[key([]string{"virt-filesystems", "--format=raw", "-a", "nbd+unix:///?socket=" + sock, "--filesystems", "--extra", "--long", "--csv"})] = []byte(filesystemsCSV)
	o, err := Open(context.Background(), OpenOptions{Disk: filepath.Join(latest, "disk.img"), Moments: moments,
		At: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC), Socket: sock, Runner: f})
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	if o.Info.Moment != "20261020T100000Z" || o.Info.Latest || !o.Info.Guest {
		t.Errorf("info = %+v", o.Info)
	}
	names, err := o.Moments()
	if err != nil || len(names) != 2 {
		t.Errorf("moments = %v, %v", names, err)
	}
}

func TestOpen_AFileCopyWithNoMomentAtThatTimeFails(t *testing.T) {
	root := t.TempDir()
	latest := filepath.Join(root, "latest")
	moments := filepath.Join(root, "moments")
	_ = os.MkdirAll(latest, 0o755)
	_ = os.MkdirAll(filepath.Join(moments, "20261020T100000Z"), 0o755)
	_ = os.WriteFile(filepath.Join(latest, "disk.img"), make([]byte, 4096), 0o660)
	_, err := Open(context.Background(), OpenOptions{Disk: filepath.Join(latest, "disk.img"), Moments: moments,
		At: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), Socket: filepath.Join(shortDir(t), "s"), Runner: newFake()})
	if err == nil {
		t.Fatal("a time before the oldest moment must fail")
	}
}
