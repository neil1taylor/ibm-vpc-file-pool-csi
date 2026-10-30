// Package guestfs reads the file systems inside a VM disk without starting the
// VM. It drives the libguestfs tools (virt-filesystems, virt-ls, virt-cat),
// which run a small read-only appliance VM, and shows the result as a
// browse.Tree: the top level lists the guest's file systems, and below each one
// are its files. Everything the tools print comes from a disk that someone else
// wrote, so each parser is strict and no name is ever put into a shell, a script
// or a mount option. See docs/specs/2026-10-22-backup-file-restore.md, section 3.
package guestfs

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// FS is one file system (or a volume that holds none that can be read) found
// inside the disk.
type FS struct {
	// ID is the name shown as the top level folder: sda1, rhel-root.
	ID string
	// Device is the libguestfs device, like /dev/sda1 or /dev/rhel/root. It is
	// empty when the name was not safe to hand to a tool.
	Device string
	// VFS is the file system type: xfs, ext4, ntfs.
	VFS   string
	Label string
	Size  int64
	// Readable is true if the file system can be listed and read.
	Readable bool
	// Reason says, in a sentence, why a volume cannot be read.
	Reason string
}

// maxFilesystems limits what one disk may list, so a hostile partition table
// cannot make an endless list.
const maxFilesystems = 256

var deviceRe = regexp.MustCompile(`^/dev/[A-Za-z0-9][A-Za-z0-9._+/-]{0,127}$`)

// validDevice reports whether a device name is safe as part of an -m argument:
// no colon (which would add a mount point or options), no space, no "..".
func validDevice(d string) bool {
	return deviceRe.MatchString(d) && !strings.Contains(d, "..") && !strings.Contains(d, "//") && !strings.HasSuffix(d, "/")
}

// readable file system types. The appliance's kernel reads these; the rest is
// said in words.
var readableVFS = map[string]bool{"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true, "ntfs": true, "vfat": true}

const wholeDisk = "Restore the whole disk instead (the restore of a whole VM disk works as before)."

// unreadableReasons are the volumes that are known and cannot be read.
var unreadableReasons = map[string]string{
	"crypto_luks":       "This volume is encrypted with LUKS. Nobody can read it without the guest's key. " + wholeDisk,
	"bitlocker":         "This volume is encrypted with BitLocker. Nobody can read it without the guest's key. " + wholeDisk,
	"refs":              "ReFS cannot be read here: Linux has no safe reader for it. " + wholeDisk,
	"zfs_member":        "ZFS cannot be read here. " + wholeDisk,
	"zfs":               "ZFS cannot be read here. " + wholeDisk,
	"swap":              "This is swap space. It holds no files.",
	"linux_raid_member": "This is part of a software RAID set, which is not assembled here. " + wholeDisk,
}

// ParseFilesystems reads the output of
// virt-filesystems --filesystems --extra --long --csv. It never fails on one
// bad line: that line becomes a volume that cannot be read. A header that is not
// there is an error, since then the output is not what was expected.
func ParseFilesystems(out []byte) ([]FS, error) {
	r := csv.NewReader(bytes.NewReader(out))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	header, err := r.Read()
	if errors.Is(err, io.EOF) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("the list of file systems is not readable: %w", err)
	}
	col := map[string]int{}
	for i, h := range header {
		col[strings.ToLower(strings.TrimSpace(h))] = i
	}
	if _, ok := col["name"]; !ok {
		return nil, errors.New("the list of file systems has no Name column")
	}
	field := func(rec []string, name string) string {
		if i, ok := col[name]; ok && i < len(rec) {
			return rec[i]
		}
		return ""
	}
	var fss []FS
	used := map[string]bool{}
	for {
		rec, rerr := r.Read()
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			break // a broken quote ends the list; what was read stays
		}
		if len(fss) >= maxFilesystems {
			break
		}
		name := field(rec, "name")
		vfs := strings.ToLower(strings.TrimSpace(field(rec, "vfs")))
		if vfs == "lvm2_member" {
			continue // a physical volume: its logical volumes are listed on their own
		}
		fs := FS{VFS: clean(vfs), Label: clean(field(rec, "label"))}
		if n, perr := strconv.ParseInt(strings.TrimSpace(field(rec, "size")), 10, 64); perr == nil && n >= 0 {
			fs.Size = n
		}
		switch {
		case !validDevice(name):
			fs.ID = uniqueID(used, "volume")
			fs.Reason = "This volume has a name that cannot be used safely, so it is not read."
		default:
			fs.ID = uniqueID(used, idOf(name))
			fs.Device = name
			switch reason, known := unreadableReasons[vfs]; {
			case known:
				fs.Reason = reason
			case readableVFS[vfs]:
				fs.Readable = true
			case vfs == "" || vfs == "unknown":
				fs.Reason = "No file system that the reader knows was found on this volume."
			default:
				fs.Reason = fmt.Sprintf("The file system type %q cannot be read here. %s", clean(vfs), wholeDisk)
			}
		}
		fss = append(fss, fs)
	}
	return fss, nil
}

// idOf makes the folder name of a device: /dev/sda1 is sda1, /dev/rhel/root is
// rhel-root.
func idOf(device string) string {
	d := strings.TrimPrefix(device, "/dev/")
	d = strings.TrimPrefix(d, "mapper/")
	d = strings.ReplaceAll(d, "/", "-")
	var b strings.Builder
	for _, r := range d {
		if r < 0x80 && (unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("._+-", r)) {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	s := b.String()
	if s == "" || s == "." || s == ".." {
		return "volume"
	}
	return s
}

func uniqueID(used map[string]bool, id string) string {
	cand := id
	for n := 2; used[cand]; n++ {
		cand = id + "-" + strconv.Itoa(n)
	}
	used[cand] = true
	return cand
}

// clean makes text from the guest fit to show: no control characters, 64
// characters at most.
func clean(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n >= 64 {
			break
		}
		if unicode.IsPrint(r) {
			b.WriteRune(r)
			n++
		}
	}
	return b.String()
}
