// Package vmsnap takes instant snapshots of a running KubeVirt VM's disks.
//
// A pool VM disk is the file disk.img in the volume's folder. A snapshot
// stops QEMU writing to the disk's top file and starts a new, small qcow2
// file next to it, which holds every later write. The old top file is the
// snapshot from then on, and is made read-only. Deleting a snapshot merges
// the next newer file into the snapshot's file. Nothing is copied to take a
// snapshot. The chain of files is newest first; the last file is disk.img.
//
// Commands run in the VM's virt-launcher pod, in the compute container:
// virsh for the snapshot and merge, qemu-img to make the new file, chmod and
// rm. Every new file names its backing file by file name only, so the chain
// does not depend on where the folder is mounted.
//
// Red Hat does not support changing a KubeVirt VM's disks this way. The
// design and lab results are in VM-INSTANT-SNAPSHOTS.md and
// docs/specs/2026-10-21-instant-vm-snapshots.md.
package vmsnap

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/vmcopy"
)

const (
	container = "compute"

	// vmiDisksDir holds one folder per filesystem volume.
	vmiDisksDir = "/var/run/kubevirt-private/vmi-disks"

	// BaseFile is the disk image KubeVirt opens, and the oldest file of a chain.
	BaseFile = "disk.img"

	// MaxSnapshots is the most snapshot files a disk may have. Every file
	// makes reads that miss the newer files a little slower.
	MaxSnapshots = 16

	modeFrozen = "0444"
	modeLive   = "0644"
)

var (
	// ErrNotADisk means the volume is not a file disk of the VM.
	ErrNotADisk = errors.New("not a disk of the VM")
	// ErrChainFull means the disk has MaxSnapshots snapshots already.
	ErrChainFull = fmt.Errorf("the disk has %d snapshots, the most allowed", MaxSnapshots)

	volumeNameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	// FileNameRe is the name of a snapshot file.
	FileNameRe = regexp.MustCompile(`^snap-[a-z0-9]{1,32}\.qcow2$`)
)

// File is one file of a disk's chain, in the volume's folder.
type File struct {
	Name   string
	Format string // "raw" or "qcow2"
}

// FormatOf returns the format of a chain file from its name: the base is
// raw, every snapshot file is qcow2.
func FormatOf(name string) string {
	if name == BaseFile {
		return "raw"
	}
	return "qcow2"
}

// Snapshotter runs snapshot commands through an Execer.
type Snapshotter struct {
	exec vmcopy.Execer
	now  func() time.Time
}

// New returns a Snapshotter.
func New(e vmcopy.Execer) *Snapshotter {
	return &Snapshotter{exec: e, now: time.Now}
}

// Result is what Take reports.
type Result struct {
	// Moment is when the snapshot was taken. It is zero when Take found the
	// snapshot taken already.
	Moment time.Time
	// Frozen is the file that holds the snapshot from now on.
	Frozen               string
	FileSystemConsistent bool
	// FreezeError says why the guest could not be frozen.
	FreezeError string
	// Chain is the disk's chain after the snapshot, newest first.
	Chain []File
}

// Take makes newFile the top file of volume's disk. The file that was on top
// holds the snapshot from then on. With freeze, the guest agent freezes the
// guest file systems for the moment of the snapshot; if it cannot, the
// snapshot is crash consistent. Without freeze, the caller has frozen the
// guest. Take is safe to repeat with the same newFile.
func (s *Snapshotter) Take(ctx context.Context, vm vmcopy.VM, volume, newFile string, freeze bool) (*Result, error) {
	if !FileNameRe.MatchString(newFile) {
		return nil, fmt.Errorf("snapshot file name %q is not valid", newFile)
	}
	if err := checkNames(volume, newFile); err != nil {
		return nil, err
	}
	disks, err := s.disks(ctx, vm)
	if err != nil {
		return nil, err
	}
	d, ok := disks.byVolume[volume]
	if !ok {
		return nil, fmt.Errorf("volume %q: %w %s/%s", volume, ErrNotADisk, vm.Namespace, vm.Name)
	}
	folder := vmiDisksDir + "/" + volume
	if d.chain[0].Name == newFile {
		// Taken already; make sure the frozen file is read-only.
		if _, err = s.run(ctx, vm, "chmod", modeFrozen, folder+"/"+d.chain[1].Name); err != nil {
			return nil, err
		}
		return &Result{Frozen: d.chain[1].Name, FileSystemConsistent: !freeze, Chain: d.chain}, nil
	}
	if indexOf(d.chain, newFile) >= 0 {
		return nil, fmt.Errorf("%s is already in the chain of %s", newFile, volume)
	}
	if len(d.chain) > MaxSnapshots {
		return nil, ErrChainFull
	}
	top := d.chain[0]
	newPath := folder + "/" + newFile
	if _, err = s.run(ctx, vm, "qemu-img", "create", "-q", "-f", "qcow2", "-F", top.Format, "-b", top.Name, newPath); err != nil {
		return nil, fmt.Errorf("making %s: %w", newFile, err)
	}
	args := []string{"virsh", "snapshot-create-as", vm.Domain(), strings.TrimSuffix(newFile, ".qcow2"),
		"--disk-only", "--atomic", "--no-metadata", "--reuse-external"}
	for _, t := range disks.targets {
		if t == d.target {
			args = append(args, "--diskspec", t+",file="+newPath)
		} else {
			args = append(args, "--diskspec", t+",snapshot=no")
		}
	}
	res := &Result{Frozen: top.Name, FileSystemConsistent: true}
	if freeze {
		_, err = s.run(ctx, vm, append(args, "--quiesce")...)
		if err != nil {
			res.FileSystemConsistent, res.FreezeError = false, err.Error()
			_, err = s.run(ctx, vm, args...)
		}
	} else {
		_, err = s.run(ctx, vm, args...)
	}
	if err != nil {
		// The new file is not in use. A failed delete leaves a small file
		// that the next Take with this name makes again.
		_, _ = s.run(ctx, vm, "rm", "-f", "--", newPath)
		return nil, fmt.Errorf("taking the snapshot: %w", err)
	}
	res.Moment = s.now()
	res.Chain = append([]File{{Name: newFile, Format: "qcow2"}}, d.chain...)
	if _, err = s.run(ctx, vm, "chmod", modeFrozen, folder+"/"+top.Name); err != nil {
		return nil, fmt.Errorf("making %s read-only: %w", top.Name, err)
	}
	return res, nil
}

// Merge deletes the snapshot held in file: child, the next newer file of the
// chain, is merged into file, and child is deleted. From then on file holds
// what child held: the next snapshot, or the live disk if child was on top.
// Merge is safe to repeat: when child is not in the chain any more, it only
// cleans up.
func (s *Snapshotter) Merge(ctx context.Context, vm vmcopy.VM, volume, file, child string) ([]File, error) {
	if err := checkNames(volume, ""); err != nil {
		return nil, err
	}
	if !isChainFileName(file) || !FileNameRe.MatchString(child) {
		return nil, fmt.Errorf("bad file names %q, %q", file, child)
	}
	disks, err := s.disks(ctx, vm)
	if err != nil {
		return nil, err
	}
	d, ok := disks.byVolume[volume]
	if !ok {
		return nil, fmt.Errorf("volume %q: %w %s/%s", volume, ErrNotADisk, vm.Namespace, vm.Name)
	}
	folder := vmiDisksDir + "/" + volume
	fi, ci := indexOf(d.chain, file), indexOf(d.chain, child)
	switch {
	case fi < 0:
		return nil, fmt.Errorf("%s is not in the chain of %s (%v)", file, volume, names(d.chain))
	case ci < 0:
		// Merged already. file may be the live file now.
		if err = s.setMode(ctx, vm, folder+"/"+file, fi == 0); err != nil {
			return nil, err
		}
		if _, err = s.run(ctx, vm, "rm", "-f", "--", folder+"/"+child); err != nil {
			return nil, err
		}
		return d.chain, nil
	case fi == 0:
		return nil, fmt.Errorf("%s is the live file of %s; it holds no snapshot", file, volume)
	case ci != fi-1:
		return nil, fmt.Errorf("%s is not the next file after %s in %v", child, file, names(d.chain))
	}
	if _, err = s.run(ctx, vm, "chmod", modeLive, folder+"/"+file); err != nil {
		return nil, err
	}
	args := []string{"virsh", "blockcommit", vm.Domain(), d.target, "--base", folder + "/" + file, "--keep-relative", "--wait"}
	if ci == 0 {
		args = append(args, "--active", "--pivot")
	} else {
		args = append(args, "--top", folder+"/"+child)
	}
	if _, err = s.run(ctx, vm, args...); err != nil {
		if _, modeErr := s.run(ctx, vm, "chmod", modeFrozen, folder+"/"+file); modeErr != nil {
			return nil, fmt.Errorf("merging %s into %s: %w (and %s could not be made read-only again: %v)", child, file, err, file, modeErr)
		}
		return nil, fmt.Errorf("merging %s into %s: %w", child, file, err)
	}
	chain := append(append([]File{}, d.chain[:ci]...), d.chain[fi:]...)
	if err = s.setMode(ctx, vm, folder+"/"+file, ci == 0); err != nil {
		return nil, err
	}
	if _, err = s.run(ctx, vm, "rm", "-f", "--", folder+"/"+child); err != nil {
		return nil, err
	}
	return chain, nil
}

// Flatten merges every file of volume's chain into disk.img and deletes the
// other files. Every snapshot of the disk is gone after it.
func (s *Snapshotter) Flatten(ctx context.Context, vm vmcopy.VM, volume string) error {
	if err := checkNames(volume, ""); err != nil {
		return err
	}
	disks, err := s.disks(ctx, vm)
	if err != nil {
		return err
	}
	d, ok := disks.byVolume[volume]
	if !ok {
		return fmt.Errorf("volume %q: %w %s/%s", volume, ErrNotADisk, vm.Namespace, vm.Name)
	}
	folder := vmiDisksDir + "/" + volume
	if _, err = s.run(ctx, vm, "chmod", modeLive, folder+"/"+BaseFile); err != nil {
		return err
	}
	if len(d.chain) == 1 {
		return nil
	}
	if _, err = s.run(ctx, vm, "virsh", "blockcommit", vm.Domain(), d.target, "--base", folder+"/"+BaseFile,
		"--keep-relative", "--wait", "--active", "--pivot"); err != nil {
		return fmt.Errorf("merging the chain of %s: %w", volume, err)
	}
	for _, f := range d.chain[:len(d.chain)-1] {
		if _, err = s.run(ctx, vm, "rm", "-f", "--", folder+"/"+f.Name); err != nil {
			return err
		}
	}
	return nil
}

// Chain returns the files of volume's disk, newest first.
func (s *Snapshotter) Chain(ctx context.Context, vm vmcopy.VM, volume string) ([]File, error) {
	if err := checkNames(volume, ""); err != nil {
		return nil, err
	}
	disks, err := s.disks(ctx, vm)
	if err != nil {
		return nil, err
	}
	d, ok := disks.byVolume[volume]
	if !ok {
		return nil, fmt.Errorf("volume %q: %w %s/%s", volume, ErrNotADisk, vm.Namespace, vm.Name)
	}
	return d.chain, nil
}

type disk struct {
	target string
	chain  []File
}

type domainDisks struct {
	byVolume map[string]disk
	targets  []string // every disk of the domain, sorted
}

// domainXML is the part of `virsh dumpxml` that names the disks and chains.
type domainXML struct {
	Disks []struct {
		Type   string `xml:"type,attr"`
		Driver struct {
			Type string `xml:"type,attr"`
		} `xml:"driver"`
		Source struct {
			File string `xml:"file,attr"`
		} `xml:"source"`
		Backing *backingXML `xml:"backingStore"`
		Target  struct {
			Dev string `xml:"dev,attr"`
		} `xml:"target"`
	} `xml:"devices>disk"`
}

type backingXML struct {
	Format struct {
		Type string `xml:"type,attr"`
	} `xml:"format"`
	Source struct {
		File string `xml:"file,attr"`
	} `xml:"source"`
	Backing *backingXML `xml:"backingStore"`
}

// disks reads the VM's disks and their chains from libvirt.
func (s *Snapshotter) disks(ctx context.Context, vm vmcopy.VM) (*domainDisks, error) {
	out, err := s.run(ctx, vm, "virsh", "dumpxml", vm.Domain())
	if err != nil {
		return nil, err
	}
	var dom domainXML
	if err = xml.Unmarshal([]byte(out), &dom); err != nil {
		return nil, fmt.Errorf("parsing the domain of %s/%s: %w", vm.Namespace, vm.Name, err)
	}
	res := &domainDisks{byVolume: map[string]disk{}}
	for _, dx := range dom.Disks {
		if dx.Target.Dev == "" {
			continue
		}
		res.targets = append(res.targets, dx.Target.Dev)
		vol, name, ok := volumeFile(dx.Source.File)
		if !ok {
			continue
		}
		chain := []File{{Name: name, Format: dx.Driver.Type}}
		for b := dx.Backing; b != nil && b.Source.File != ""; b = b.Backing {
			bvol, bname, bok := volumeFile(b.Source.File)
			if !bok || bvol != vol {
				return nil, fmt.Errorf("disk %s of %s/%s has a backing file outside its folder: %s", dx.Target.Dev, vm.Namespace, vm.Name, b.Source.File)
			}
			chain = append(chain, File{Name: bname, Format: b.Format.Type})
		}
		if chain[len(chain)-1].Name != BaseFile {
			return nil, fmt.Errorf("the chain of %s does not end in %s: %v", vol, BaseFile, names(chain))
		}
		res.byVolume[vol] = disk{target: dx.Target.Dev, chain: chain}
	}
	sort.Strings(res.targets)
	return res, nil
}

// volumeFile splits a path in a volume folder into the volume and file names.
func volumeFile(p string) (string, string, bool) {
	rest, ok := strings.CutPrefix(p, vmiDisksDir+"/")
	if !ok {
		return "", "", false
	}
	vol, name, ok := strings.Cut(rest, "/")
	if !ok || strings.Contains(name, "/") || !volumeNameRe.MatchString(vol) {
		return "", "", false
	}
	return vol, path.Clean(name), true
}

func (s *Snapshotter) setMode(ctx context.Context, vm vmcopy.VM, p string, live bool) error {
	mode := modeFrozen
	if live {
		mode = modeLive
	}
	_, err := s.run(ctx, vm, "chmod", mode, p)
	return err
}

func checkNames(volume, file string) error {
	if !volumeNameRe.MatchString(volume) {
		return fmt.Errorf("volume name %q is not valid", volume)
	}
	if file != "" && !FileNameRe.MatchString(file) {
		return fmt.Errorf("snapshot file name %q is not valid", file)
	}
	return nil
}

func isChainFileName(name string) bool { return name == BaseFile || FileNameRe.MatchString(name) }

func indexOf(chain []File, name string) int {
	for i, f := range chain {
		if f.Name == name {
			return i
		}
	}
	return -1
}

func names(chain []File) []string {
	n := make([]string, len(chain))
	for i, f := range chain {
		n[i] = f.Name
	}
	return n
}

// run executes a command in the compute container. The error includes stderr.
func (s *Snapshotter) run(ctx context.Context, vm vmcopy.VM, args ...string) (string, error) {
	stdout, stderr, err := s.exec.Exec(ctx, vm.Namespace, vm.Pod, container, args)
	if err != nil {
		if e := strings.TrimSpace(stderr); e != "" {
			return stdout, fmt.Errorf("%s: %w: %s", args[0], err, e)
		}
		return stdout, fmt.Errorf("%s: %w", args[0], err)
	}
	return stdout, nil
}
