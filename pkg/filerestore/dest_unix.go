//go:build unix

package filerestore

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// DirDest writes into a folder on disk.
//
// Nothing is opened by a path that could pass through a link. The root is a
// folder this process made (or one the operator named), and every name below it
// is made by this process with mkdirat, symlinkat, or openat with O_EXCL and
// O_NOFOLLOW, in a folder reached one name at a time with O_NOFOLLOW. Mode,
// owner and times are set through the open file or folder, or with
// AT_SYMLINK_NOFOLLOW for a link. So a link that a person puts into the
// destination while the restore runs (an existing claim is open to its users)
// makes an operation fail, and cannot make it write elsewhere.
type DirDest struct {
	root int
	// parent is the folder that holds the root, and rootName the root's name in
	// it. Set when the root is a staging folder that can be renamed or removed.
	parent   int
	rootName string
	// wipeOnAbort removes the content of the root on abort, and not the root.
	wipeOnAbort bool

	capacity int64 // bytes the destination may hold; 0 means only the share limit
	usedOnce sync.Once
	used     int64

	mu       sync.Mutex
	cacheKey string
	cacheFd  int
}

const (
	dirFlags  = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	fileMode  = 0o600
	folderMod = 0o700
)

func retry(fn func() error) error {
	for {
		if err := fn(); !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

func openDirAt(dirfd int, name string) (int, error) {
	var fd int
	err := retry(func() (e error) { fd, e = unix.Openat(dirfd, name, dirFlags, 0); return })
	return fd, err
}

// openVolume opens the volume folder (for example pvcs/pvc-...) below shareRoot,
// one name at a time and never through a link. With create set it makes the
// folders that do not exist; without it a missing folder is ErrClaimNotReady.
func openVolume(shareRoot string, volume [][]byte, create bool) (int, error) {
	cur, err := openRootDir(shareRoot)
	if err != nil {
		return -1, err
	}
	for _, n := range volume {
		name := string(n)
		if create {
			if err := retry(func() error { return unix.Mkdirat(cur, name, folderMod) }); err != nil && !errors.Is(err, unix.EEXIST) {
				_ = unix.Close(cur)
				return -1, fmt.Errorf("make the volume folder: %w", err)
			}
		}
		next, err := openDirAt(cur, name)
		_ = unix.Close(cur)
		if errors.Is(err, unix.ENOENT) {
			return -1, ErrClaimNotReady
		}
		if err != nil {
			return -1, fmt.Errorf("open the volume folder (a link is never followed): %w", err)
		}
		cur = next
	}
	return cur, nil
}

// NewVolumeDest makes the destination for a new claim: the volume folder
// (volume, for example pvcs/pvc-...) below shareRoot. It makes the folder if it
// does not exist, and empties it: a new claim is held back from pods, so what
// is there is the output of an earlier try of this restore. capacity is the
// size of the claim in bytes (0 if not known).
func NewVolumeDest(shareRoot string, volume [][]byte, capacity int64) (*DirDest, error) {
	if len(volume) == 0 {
		return nil, errors.New("the volume folder is required: refusing to empty the whole share")
	}
	cur, err := openVolume(shareRoot, volume, true)
	if err != nil {
		return nil, err
	}
	d := &DirDest{root: cur, parent: -1, wipeOnAbort: true, capacity: capacity, cacheFd: -1}
	if err := d.wipe(); err != nil {
		_ = d.Close()
		return nil, fmt.Errorf("empty the new volume: %w", err)
	}
	return d, nil
}

// NewFolderDest makes the destination for an existing claim: a hidden staging
// folder in the claim's folder (volume below shareRoot). Commit moves it to its
// final name. It refuses a claim that holds a VM disk, and a claim that has no
// folder yet, because the driver makes that folder with the claim's owner when a
// pod first uses the claim. capacity is the size of the claim in bytes (0 if
// not known).
func NewFolderDest(shareRoot string, volume [][]byte, stagingName string, capacity int64) (*DirDest, error) {
	if len(volume) == 0 {
		return nil, errors.New("the volume folder is required")
	}
	pfd, err := openVolume(shareRoot, volume, false)
	if err != nil {
		return nil, err
	}
	return newFolderDest(pfd, stagingName, capacity)
}

// NewFolderDestAt is NewFolderDest for a mount that is the claim's folder
// itself: the writer of a restore that reads a VM disk mounts only that folder,
// and not the share around it.
func NewFolderDestAt(volumeRoot, stagingName string, capacity int64) (*DirDest, error) {
	pfd, err := openRootDir(volumeRoot)
	if err != nil {
		return nil, err
	}
	return newFolderDest(pfd, stagingName, capacity)
}

// newFolderDest takes over pfd, the open folder of the claim.
func newFolderDest(pfd int, stagingName string, capacity int64) (*DirDest, error) {
	if stagingName == "" || stagingName == "." || stagingName == ".." || containsSlash(stagingName) {
		_ = unix.Close(pfd)
		return nil, errors.New("bad staging folder name")
	}
	var st unix.Stat_t
	if unix.Fstatat(pfd, "disk.img", &st, unix.AT_SYMLINK_NOFOLLOW) == nil {
		_ = unix.Close(pfd)
		return nil, ErrVMDiskClaim
	}
	// An earlier try of this restore may have left its staging folder.
	if err := removeTree(pfd, stagingName); err != nil {
		_ = unix.Close(pfd)
		return nil, fmt.Errorf("remove the staging folder of an earlier try: %w", err)
	}
	if err := retry(func() error { return unix.Mkdirat(pfd, stagingName, folderMod) }); err != nil {
		_ = unix.Close(pfd)
		return nil, fmt.Errorf("make the staging folder: %w", err)
	}
	root, err := openDirAt(pfd, stagingName)
	if err != nil {
		_ = unix.Close(pfd)
		return nil, fmt.Errorf("open the staging folder: %w", err)
	}
	return &DirDest{root: root, parent: pfd, rootName: stagingName, capacity: capacity, cacheFd: -1}, nil
}

func containsSlash(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' || s[i] == 0 {
			return true
		}
	}
	return false
}

// openRootDir opens a folder the operator named. It may be reached through a
// link, because the operator chose it; names below it are never trusted.
func openRootDir(path string) (int, error) {
	var fd int
	err := retry(func() (e error) { fd, e = unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0); return })
	if err != nil {
		return -1, fmt.Errorf("open the destination: %w", err)
	}
	return fd, nil
}

// Commit moves the staging folder to prefix, or to prefix-2, prefix-3 and so
// on if the name exists, and returns the name. A name that exists is never
// replaced.
func (d *DirDest) Commit(prefix string) (string, error) {
	if d.parent < 0 {
		return "", errors.New("this destination has no staging folder")
	}
	if prefix == "" || containsSlash(prefix) || prefix == "." || prefix == ".." {
		return "", errors.New("bad folder name")
	}
	for i := 1; i < 10000; i++ {
		name := prefix
		if i > 1 {
			name = prefix + "-" + strconv.Itoa(i)
		}
		var st unix.Stat_t
		err := unix.Fstatat(d.parent, name, &st, unix.AT_SYMLINK_NOFOLLOW)
		if err == nil {
			continue
		}
		if !errors.Is(err, unix.ENOENT) {
			return "", fmt.Errorf("look for %q: %w", name, err)
		}
		// NFS has no rename that refuses to replace, so the name was checked
		// just above. Only a person who makes the same name in this instant can
		// win, and a rename never replaces a folder that holds files.
		if err := retry(func() error { return unix.Renameat(d.parent, d.rootName, d.parent, name) }); err != nil {
			return "", fmt.Errorf("move the staging folder to its name: %w", err)
		}
		d.rootName = ""
		return name, nil
	}
	return "", errors.New("no free folder name")
}

// Close releases the open folders. It does not remove anything.
func (d *DirDest) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dropCache()
	var err error
	if d.root >= 0 {
		err = unix.Close(d.root)
		d.root = -1
	}
	if d.parent >= 0 {
		_ = unix.Close(d.parent)
		d.parent = -1
	}
	return err
}

func (d *DirDest) dropCache() {
	if d.cacheFd >= 0 {
		_ = unix.Close(d.cacheFd)
	}
	d.cacheFd, d.cacheKey = -1, ""
}

// parentOf opens the folder that holds the last name of path. The folder is
// kept open for the next call, because a restore writes the files of one folder
// one after the other.
func (d *DirDest) parentOf(path [][]byte) (int, string, error) {
	if len(path) == 0 {
		return -1, "", errors.New("the root has no parent")
	}
	name := string(path[len(path)-1])
	dir := path[:len(path)-1]
	k := joinKey(dir)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cacheFd >= 0 && d.cacheKey == k {
		return d.cacheFd, name, nil
	}
	cur, err := unix.Dup(d.root)
	if err != nil {
		return -1, "", err
	}
	for _, n := range dir {
		next, err := openDirAt(cur, string(n))
		_ = unix.Close(cur)
		if err != nil {
			return -1, "", fmt.Errorf("open a folder (a link is never followed): %w", err)
		}
		cur = next
	}
	d.dropCache()
	d.cacheFd, d.cacheKey = cur, k
	return cur, name, nil
}

// Mkdir makes a folder, mode 0700. Its mode and owner are set by FinishDir.
func (d *DirDest) Mkdir(path [][]byte) error {
	pfd, name, err := d.parentOf(path)
	if err != nil {
		return err
	}
	return retry(func() error { return unix.Mkdirat(pfd, name, folderMod) })
}

func timevals(t time.Time) []unix.Timeval {
	tv := unix.NsecToTimeval(t.UnixNano())
	return []unix.Timeval{tv, tv}
}

// setMeta sets times, mode and owner, in that order, through an open file or
// folder. The owner is last: after it changes, this process may not own the
// file any more.
func setMeta(fd int, m Meta) error {
	if !m.MTime.IsZero() {
		if err := unix.Futimes(fd, timevals(m.MTime)); err != nil {
			return fmt.Errorf("set the time: %w", err)
		}
	}
	if err := unix.Fchmod(fd, m.Mode&0o7777); err != nil {
		return fmt.Errorf("set the mode: %w", err)
	}
	if m.UID != nil || m.GID != nil {
		uid, gid := -1, -1
		if m.UID != nil {
			uid = int(*m.UID)
		}
		if m.GID != nil {
			gid = int(*m.GID)
		}
		if err := unix.Fchown(fd, uid, gid); err != nil {
			return fmt.Errorf("set the owner: %w", err)
		}
	}
	return nil
}

// FinishDir sets times, mode and owner of a folder made with Mkdir, or of the
// root for the empty path.
func (d *DirDest) FinishDir(path [][]byte, m Meta) error {
	if len(path) == 0 {
		return setMeta(d.root, m)
	}
	pfd, name, err := d.parentOf(path)
	if err != nil {
		return err
	}
	fd, err := openDirAt(pfd, name)
	if err != nil {
		return fmt.Errorf("open a folder to finish it (a link is never followed): %w", err)
	}
	defer func() { _ = unix.Close(fd) }()
	return setMeta(fd, m)
}

type dirFile struct {
	f *os.File
}

func (f *dirFile) Write(p []byte) (int, error) { return f.f.Write(p) }
func (f *dirFile) Close() error                { return f.f.Close() }
func (f *dirFile) Finish(m Meta) error {
	if err := setMeta(int(f.f.Fd()), m); err != nil { //nolint:gosec // a file descriptor fits in int
		_ = f.f.Close()
		return err
	}
	return f.f.Close()
}

// Create makes a new file. A name that exists, even a link, is an error.
func (d *DirDest) Create(path [][]byte) (DestFile, error) {
	pfd, name, err := d.parentOf(path)
	if err != nil {
		return nil, err
	}
	var fd int
	err = retry(func() (e error) {
		fd, e = unix.Openat(pfd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, fileMode)
		return
	})
	if err != nil {
		return nil, err
	}
	return &dirFile{f: os.NewFile(uintptr(fd), "restored file")}, nil //nolint:gosec // fd is a small positive descriptor
}

// Symlink makes a link with the given text. The link is never followed, not
// here and not when its owner and time are set.
func (d *DirDest) Symlink(path [][]byte, target []byte, m Meta) error {
	pfd, name, err := d.parentOf(path)
	if err != nil {
		return err
	}
	if err := retry(func() error { return unix.Symlinkat(string(target), pfd, name) }); err != nil {
		return err
	}
	if !m.MTime.IsZero() {
		ts := []unix.Timespec{unix.NsecToTimespec(m.MTime.UnixNano()), unix.NsecToTimespec(m.MTime.UnixNano())}
		if err := unix.UtimesNanoAt(pfd, name, ts, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return fmt.Errorf("set the time of a link: %w", err)
		}
	}
	if m.UID != nil || m.GID != nil {
		uid, gid := -1, -1
		if m.UID != nil {
			uid = int(*m.UID)
		}
		if m.GID != nil {
			gid = int(*m.GID)
		}
		if err := unix.Fchownat(pfd, name, uid, gid, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return fmt.Errorf("set the owner of a link: %w", err)
		}
	}
	return nil
}

// Free is the number of bytes that can still be written: the free space of the
// share, or the size of the claim less what it holds, whichever is less.
func (d *DirDest) Free() (int64, error) {
	var sf unix.Statfs_t
	if err := unix.Fstatfs(d.root, &sf); err != nil {
		return 0, err
	}
	free := int64(sf.Bavail) * int64(sf.Bsize) //nolint:gosec // block counts fit in int64
	if d.capacity > 0 {
		var err error
		d.usedOnce.Do(func() {
			// For a folder in a claim, the claim's content is what counts.
			at := d.root
			if d.parent >= 0 {
				at = d.parent
			}
			d.used, err = treeSize(at)
		})
		if err != nil {
			return 0, fmt.Errorf("measure the claim: %w", err)
		}
		if left := d.capacity - d.used; left < free {
			free = left
		}
	}
	if free < 0 {
		free = 0
	}
	return free, nil
}

// treeSize adds up the sizes of the files below dirfd, without following links.
func treeSize(dirfd int) (int64, error) {
	dup, err := unix.Dup(dirfd)
	if err != nil {
		return 0, err
	}
	dir := os.NewFile(uintptr(dup), "folder") //nolint:gosec // fd is a small positive descriptor
	defer func() { _ = dir.Close() }()
	var total int64
	for {
		names, err := dir.Readdirnames(1000)
		for _, n := range names {
			var st unix.Stat_t
			if serr := unix.Fstatat(dirfd, n, &st, unix.AT_SYMLINK_NOFOLLOW); serr != nil {
				continue
			}
			switch st.Mode & unix.S_IFMT {
			case unix.S_IFREG:
				total += st.Size
			case unix.S_IFDIR:
				sub, oerr := openDirAt(dirfd, n)
				if oerr != nil {
					continue
				}
				sz, serr := treeSize(sub)
				_ = unix.Close(sub)
				if serr != nil {
					return 0, serr
				}
				total += sz
			}
		}
		if errors.Is(err, io.EOF) {
			return total, nil
		}
		if err != nil {
			return 0, err
		}
	}
}

// wipe removes everything in the root, and not the root.
func (d *DirDest) wipe() error { return removeChildren(d.root) }

// Abort removes what the restore wrote: the staging folder, or for a new claim
// the content of its folder.
func (d *DirDest) Abort() error {
	d.mu.Lock()
	d.dropCache()
	d.mu.Unlock()
	if d.wipeOnAbort {
		return d.wipe()
	}
	if d.parent >= 0 && d.rootName != "" {
		return removeTree(d.parent, d.rootName)
	}
	return nil
}

// removeChildren removes everything in the folder dirfd, without following a
// link.
func removeChildren(dirfd int) error {
	dup, err := unix.Dup(dirfd)
	if err != nil {
		return err
	}
	dir := os.NewFile(uintptr(dup), "folder") //nolint:gosec // fd is a small positive descriptor
	defer func() { _ = dir.Close() }()
	// A finished folder may be read-only; try to open it up. This may fail on a
	// folder that now belongs to another user, which is then not our write to undo.
	_ = unix.Fchmod(dirfd, folderMod)
	for {
		names, err := dir.Readdirnames(1000)
		for _, n := range names {
			if rerr := removeTree(dirfd, n); rerr != nil {
				return rerr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// removeTree removes name in dirfd and everything below it. A link is removed,
// never followed. A name that does not exist is not an error.
func removeTree(dirfd int, name string) error {
	var st unix.Stat_t
	if err := unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		if err := unix.Unlinkat(dirfd, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			return err
		}
		return nil
	}
	sub, err := openDirAt(dirfd, name)
	if err != nil {
		return err
	}
	rerr := removeChildren(sub)
	_ = unix.Close(sub)
	if rerr != nil {
		return rerr
	}
	if err := unix.Unlinkat(dirfd, name, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return nil
}
