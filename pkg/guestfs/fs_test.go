package guestfs

import (
	"strings"
	"testing"
)

const filesystemsCSV = `Name,Type,VFS,Label,MBR,Size,Parent
/dev/sda1,filesystem,xfs,boot,,1073741824,/dev/sda1
/dev/sda2,filesystem,LVM2_member,,,53687091200,
/dev/rhel/root,filesystem,xfs,,,40000000000,/dev/rhel/root
/dev/rhel/swap,filesystem,swap,,,2147483648,
/dev/sdb1,filesystem,ntfs,Data,,107374182400,/dev/sdb1
/dev/sdc1,filesystem,crypto_LUKS,,,1000000,
/dev/sdd1,filesystem,BitLocker,,,1000000,
/dev/sde1,filesystem,unknown,,,1000000,
`

func TestParseFilesystems(t *testing.T) {
	fss, err := ParseFilesystems([]byte(filesystemsCSV))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]FS{}
	for _, f := range fss {
		got[f.ID] = f
	}
	if _, ok := got["sda2"]; ok {
		t.Error("an LVM physical volume holds no files of its own and is not listed")
	}
	for id, readable := range map[string]bool{"sda1": true, "rhel-root": true, "sdb1": true, "rhel-swap": false, "sdc1": false, "sdd1": false, "sde1": false} {
		f, ok := got[id]
		if !ok {
			t.Errorf("%s is missing: %v", id, fss)
			continue
		}
		if f.Readable != readable {
			t.Errorf("%s readable = %v (%s)", id, f.Readable, f.Reason)
		}
		if !f.Readable && f.Reason == "" {
			t.Errorf("%s is unreadable and says nothing about why", id)
		}
	}
	if f := got["sdc1"]; !strings.Contains(f.Reason, "LUKS") || !strings.Contains(f.Reason, "whole disk") {
		t.Errorf("LUKS reason = %q", f.Reason)
	}
	if f := got["sdd1"]; !strings.Contains(f.Reason, "BitLocker") {
		t.Errorf("BitLocker reason = %q", f.Reason)
	}
	if f := got["sda1"]; f.Device != "/dev/sda1" || f.VFS != "xfs" || f.Label != "boot" || f.Size != 1073741824 {
		t.Errorf("sda1 = %+v", f)
	}
}

func TestParseFilesystems_DeviceNamesAreChecked(t *testing.T) {
	// A logical volume name comes from the disk. A colon would add a mount
	// point or options to the -m argument, so such a device is never used.
	bad := []string{"/dev/vg/root:/:rw", "/dev/vg/ro ot", "/dev/../etc", "sda1", "/dev/vg/-x\n", "/dev/" + strings.Repeat("a", 300)}
	for _, name := range bad {
		in := "Name,Type,VFS,Label,MBR,Size,Parent\n\"" + strings.ReplaceAll(name, `"`, `""`) + "\",filesystem,ext4,,,1000,\n"
		fss, err := ParseFilesystems([]byte(in))
		if err != nil {
			t.Fatalf("%q: %v", name, err)
		}
		for _, f := range fss {
			if f.Readable {
				t.Errorf("%q must not be readable: %+v", name, f)
			}
			if f.Device != "" {
				t.Errorf("%q must not be kept as a device: %+v", name, f)
			}
		}
		if len(fss) == 0 {
			t.Errorf("%q was dropped without a word", name)
		}
	}
}

func TestParseFilesystems_IDsAreUniqueAndSafe(t *testing.T) {
	in := "Name,Type,VFS,Label,MBR,Size,Parent\n" +
		"/dev/mapper/a-b,filesystem,ext4,,,1,\n/dev/a/b,filesystem,ext4,,,1,\n/dev/a/b,filesystem,ext4,,,1,\n"
	fss, err := ParseFilesystems([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, f := range fss {
		if seen[f.ID] || strings.ContainsAny(f.ID, "/ \x00") || f.ID == "" {
			t.Errorf("bad or repeated id %q in %v", f.ID, fss)
		}
		seen[f.ID] = true
	}
	if len(fss) != 3 {
		t.Errorf("got %d", len(fss))
	}
}

func TestParseFilesystems_Strict(t *testing.T) {
	for _, in := range []string{"", "nothing here", "Name,Type\n", "Name,Type,VFS,Label,MBR,Size,Parent\n\"unterminated"} {
		fss, _ := ParseFilesystems([]byte(in))
		for _, f := range fss {
			if f.Readable {
				t.Errorf("%q gave a readable file system: %v", in, fss)
			}
		}
	}
	// A size that is not a number makes that line unreadable, not the helper crash.
	fss, err := ParseFilesystems([]byte("Name,Type,VFS,Label,MBR,Size,Parent\n/dev/sda1,filesystem,ext4,,,lots,\n"))
	if err != nil || len(fss) != 1 || fss[0].Size != 0 {
		t.Errorf("%v %v", fss, err)
	}
}

func TestParseFilesystems_TextForDisplayIsCleaned(t *testing.T) {
	in := "Name,Type,VFS,Label,MBR,Size,Parent\n/dev/sda1,filesystem,ext4,\"evil\x1b[31m\nlabel" + strings.Repeat("x", 200) + "\",,1,\n"
	fss, err := ParseFilesystems([]byte(in))
	if err != nil || len(fss) != 1 {
		t.Fatal(fss, err)
	}
	if strings.ContainsAny(fss[0].Label, "\x1b\n") || len(fss[0].Label) > 64 {
		t.Errorf("label = %q", fss[0].Label)
	}
}

func FuzzParseFilesystems(f *testing.F) {
	f.Add([]byte(filesystemsCSV))
	f.Add([]byte("Name\n\"\x00\"\n"))
	f.Fuzz(func(t *testing.T, in []byte) {
		fss, _ := ParseFilesystems(in)
		seen := map[string]bool{}
		for _, fs := range fss {
			if fs.ID == "" || seen[fs.ID] || strings.ContainsAny(fs.ID, "/\x00") {
				t.Fatalf("bad id %q", fs.ID)
			}
			seen[fs.ID] = true
			if fs.Readable && !validDevice(fs.Device) {
				t.Fatalf("a readable file system with the device %q", fs.Device)
			}
		}
	})
}
