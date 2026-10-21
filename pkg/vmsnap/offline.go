package vmsnap

import (
	"fmt"
	"path"
	"strings"
)

// Scripts for a Job that changes the chain of a stopped VM's disk, and for a
// Job that restores a snapshot into a new disk. Every qemu-img command names
// the format of its image, and every qcow2 file records the format of its
// backing file: the guest can write anything into a raw disk, including a
// qcow2 header that names another file on the share, so qemu-img must never
// guess a format.
//
// Anyone who can write to a volume can put files in its folder, so before
// qemu-img opens a chain, the script checks every link: each backing file is
// disk.img (raw) or a snapshot file (qcow2) in the same folder, named with
// plain characters only, and no file keeps its data in another file. The Job also
// mounts only the volume folders it works on, never a whole share.
//
// The scripts run as the QEMU user in KubeVirt's virt-launcher image, which
// has qemu-img. dir is where the Job mounts the volume folder. Every script is
// safe to run again after it stopped part way.

// checkChainFunc defines the shell function check_chain DIR FILE. It follows
// FILE's backing files in DIR down to disk.img and fails on any link that is
// not a chain file of the right format, or on a chain longer than allowed.
func checkChainFunc() string {
	return fmt.Sprintf(`check_chain() {
  d=$1; f=$2; n=0
  while [ "$f" != %s ]; do
    case "$f" in snap-*.qcow2) ;; *) echo "bad chain file: $f" >&2; return 1 ;; esac
    info=$(qemu-img info -f qcow2 -U --output=json "$d/$f")
    if printf '%%s\n' "$info" | grep -q '"data-file"'; then echo "$f keeps its data in another file" >&2; return 1; fi
    b=$(printf '%%s\n' "$info" | sed -n 's/^ *"backing-filename": "\(.*\)",*$/\1/p')
    bf=$(printf '%%s\n' "$info" | sed -n 's/^ *"backing-filename-format": "\(.*\)",*$/\1/p')
    case "$b" in
      %s) [ "$bf" = raw ] || { echo "$f backs onto $b as $bf" >&2; return 1; } ;;
      snap-*.qcow2) [ "$bf" = qcow2 ] || { echo "$f backs onto $b as $bf" >&2; return 1; } ;;
      *) echo "$f backs onto $b" >&2; return 1 ;;
    esac
    case "$b" in *[!a-z0-9.-]*) echo "$f backs onto $b" >&2; return 1 ;; esac
    f=$b; n=$((n+1))
    [ $n -le %d ] || { echo "chain too long" >&2; return 1; }
  done
}
`, BaseFile, BaseFile, MaxSnapshots+1)
}

// OfflineTakeScript makes newFile the top file of the chain whose top is top,
// and makes top read-only.
func OfflineTakeScript(dir string, top File, newFile string) (string, error) {
	if err := checkScriptArgs(dir, top.Name); err != nil {
		return "", err
	}
	if !FileNameRe.MatchString(newFile) {
		return "", fmt.Errorf("snapshot file name %q is not valid", newFile)
	}
	if top.Format != "raw" && top.Format != "qcow2" {
		return "", fmt.Errorf("format %q is not valid", top.Format)
	}
	var b strings.Builder
	b.WriteString("set -eu\n")
	b.WriteString(checkChainFunc())
	fmt.Fprintf(&b, "check_chain %q %s\n", dir, top.Name)
	fmt.Fprintf(&b, "if [ ! -e %q ]; then\n", dir+"/"+newFile)
	fmt.Fprintf(&b, "  qemu-img create -q -f qcow2 -F %s -b %s %q\n", top.Format, top.Name, dir+"/"+newFile)
	b.WriteString("fi\n")
	fmt.Fprintf(&b, "chmod %s %q\n", modeFrozen, dir+"/"+top.Name)
	return b.String(), nil
}

// OfflineMergeScript merges child into file, the file it backs onto, and
// deletes child. grandchild is the file that backs onto child, or "" when
// child is the top file; it is pointed at file. file stays read-only unless it
// is the top file afterwards.
func OfflineMergeScript(dir, file, child, grandchild string) (string, error) {
	if err := checkScriptArgs(dir, file); err != nil {
		return "", err
	}
	if !FileNameRe.MatchString(child) || (grandchild != "" && !FileNameRe.MatchString(grandchild)) {
		return "", fmt.Errorf("bad file names %q, %q", child, grandchild)
	}
	c, f := dir+"/"+child, dir+"/"+file
	var b strings.Builder
	b.WriteString("set -eu\n")
	b.WriteString(checkChainFunc())
	fmt.Fprintf(&b, "if [ -e %q ]; then\n", c)
	fmt.Fprintf(&b, "  check_chain %q %s\n", dir, child)
	// Refuse to merge into a file that child does not back onto.
	fmt.Fprintf(&b, "  qemu-img info -f qcow2 -U --output=json %q | grep -q '\"backing-filename\": \"%s\"' || { echo %q >&2; exit 1; }\n",
		c, file, child+" does not back onto "+file)
	fmt.Fprintf(&b, "  chmod %s %q\n", modeLive, f)
	fmt.Fprintf(&b, "  qemu-img commit -q -f qcow2 %q\n", c)
	if grandchild != "" {
		fmt.Fprintf(&b, "  qemu-img rebase -u -f qcow2 -F %s -b %s %q\n", FormatOf(file), file, dir+"/"+grandchild)
	}
	fmt.Fprintf(&b, "  rm -f -- %q\n", c)
	b.WriteString("fi\n")
	mode := modeLive
	if grandchild != "" {
		mode = modeFrozen
	}
	fmt.Fprintf(&b, "chmod %s %q\n", mode, f)
	return b.String(), nil
}

// ConvertScript writes the disk that file in srcDir shows, through its
// backing files, as a raw disk.img in dstDir.
func ConvertScript(srcDir, file, dstDir string) (string, error) {
	if err := checkScriptArgs(srcDir, file); err != nil {
		return "", err
	}
	if err := checkScriptArgs(dstDir, BaseFile); err != nil {
		return "", err
	}
	part := dstDir + "/" + BaseFile + ".part"
	var b strings.Builder
	b.WriteString("set -eu\n")
	b.WriteString(checkChainFunc())
	fmt.Fprintf(&b, "check_chain %q %s\n", srcDir, file)
	fmt.Fprintf(&b, "trap 'rm -f -- %q' EXIT\n", part)
	fmt.Fprintf(&b, "qemu-img convert -f %s -O raw %q %q\n", FormatOf(file), srcDir+"/"+file, part)
	fmt.Fprintf(&b, "mv -f -- %q %q\n", part, dstDir+"/"+BaseFile)
	b.WriteString("trap - EXIT\n")
	return b.String(), nil
}

// checkScriptArgs refuses a folder or file name that a script must not use.
func checkScriptArgs(dir, file string) error {
	if !path.IsAbs(dir) || path.Clean(dir) != dir || strings.ContainsAny(dir, "\"'`$\\\n\x00 ") {
		return fmt.Errorf("folder %q is not valid", dir)
	}
	if !isChainFileName(file) {
		return fmt.Errorf("file name %q is not valid", file)
	}
	return nil
}
