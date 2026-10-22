//go:build linux

package browse

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// MaxDirEntries is the most names one folder may hold for a listing. The names
// of a folder are read and sorted in memory, so a folder of millions of names
// is refused instead of using all of the helper's memory.
const MaxDirEntries = 5_000_000

const defaultListLimit = 1000

// DirTree is a Tree over a folder on disk.
//
// Every name below the root is opened with openat2 and RESOLVE_BENEATH,
// RESOLVE_NO_SYMLINKS, RESOLVE_NO_MAGICLINKS and RESOLVE_NO_XDEV. So a link
// anywhere in a path makes the open fail, and nothing can leave the root, even
// if a backup holds a link to /etc or to another volume's folder. If the
// kernel or the container's seccomp profile does not allow openat2, names are
// opened one folder at a time with O_NOFOLLOW, which has the same effect.
type DirTree struct {
	root int
	walk bool
}

// NewTree opens root. The root itself may be reached through a link, because
// it is set by the operator; names below it are never trusted.
func NewTree(root string) (*DirTree, error) {
	t, err := newTree(root, false)
	if err != nil {
		return nil, err
	}
	// Probe openat2 once. A container's seccomp profile may refuse it.
	fd, err := t.openAt2(t.root, ".", unix.O_RDONLY|unix.O_DIRECTORY)
	switch {
	case err == nil:
		_ = unix.Close(fd)
	case errors.Is(err, unix.ENOSYS), errors.Is(err, unix.EPERM), errors.Is(err, unix.EINVAL):
		t.walk = true
	default:
		_ = t.Close()
		return nil, fmt.Errorf("open the root folder: %w", err)
	}
	return t, nil
}

// newTree opens root. With walk set it opens names folder by folder, as the
// fallback does when openat2 is not available.
func newTree(root string, walk bool) (*DirTree, error) {
	fd, err := retryEINTR(func() (int, error) {
		return unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	})
	if err != nil {
		return nil, fmt.Errorf("open the root folder %q: %w", root, err)
	}
	return &DirTree{root: fd, walk: walk}, nil
}

// Walking reports whether names are opened folder by folder (no openat2).
func (t *DirTree) Walking() bool { return t.walk }

// Close closes the root folder.
func (t *DirTree) Close() error { return unix.Close(t.root) }

func retryEINTR(fn func() (int, error)) (int, error) {
	for {
		v, err := fn()
		if !errors.Is(err, unix.EINTR) {
			return v, err
		}
	}
}

func (t *DirTree) openAt2(dirfd int, rel string, flags int) (int, error) {
	how := &unix.OpenHow{
		Flags: uint64(flags | unix.O_CLOEXEC),
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS |
			unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_XDEV,
	}
	return retryEINTR(func() (int, error) { return unix.Openat2(dirfd, rel, how) })
}

// openAt opens rel, which holds names joined by "/", below dirfd.
func (t *DirTree) openAt(dirfd int, rel string, flags int) (int, error) {
	if !t.walk {
		return t.openAt2(dirfd, rel, flags)
	}
	cur, owned := dirfd, false
	defer func() {
		if owned {
			_ = unix.Close(cur)
		}
	}()
	parts := strings.Split(rel, "/")
	for i, name := range parts {
		last := i == len(parts)-1
		f := unix.O_PATH | unix.O_NOFOLLOW
		if last {
			f = flags | unix.O_NOFOLLOW
		}
		fd, err := retryEINTR(func() (int, error) { return unix.Openat(cur, name, f|unix.O_CLOEXEC, 0) })
		if err != nil {
			if last && errors.Is(err, unix.ENOTDIR) && isLink(cur, name) {
				return -1, unix.ELOOP
			}
			return -1, err
		}
		if last {
			return fd, nil
		}
		// The O_PATH handle names the thing itself, never what a link points to.
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			_ = unix.Close(fd)
			return -1, err
		}
		if st.Mode&unix.S_IFMT != unix.S_IFDIR {
			_ = unix.Close(fd)
			if st.Mode&unix.S_IFMT == unix.S_IFLNK {
				return -1, unix.ELOOP
			}
			return -1, unix.ENOTDIR
		}
		if owned {
			_ = unix.Close(cur)
		}
		cur, owned = fd, true
	}
	return -1, unix.EINVAL
}

func isLink(dirfd int, name string) bool {
	var st unix.Stat_t
	return unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW) == nil && st.Mode&unix.S_IFMT == unix.S_IFLNK
}

// relPath joins names for openAt. The empty path is the root itself.
func relPath(path [][]byte) string {
	if len(path) == 0 {
		return "."
	}
	parts := make([]string, len(path))
	for i, n := range path {
		parts[i] = string(n)
	}
	return strings.Join(parts, "/")
}

// mapErr turns an errno into an API error. It never includes a path.
func mapErr(err error) error {
	var e *Error
	if errors.As(err, &e) {
		return err
	}
	switch {
	case errors.Is(err, unix.ELOOP):
		return &Error{Code: CodeLink, Msg: "a symbolic link is in the path; links are never followed"}
	case errors.Is(err, unix.ENOENT):
		return &Error{Code: CodeNotFound, Msg: "no such file or folder in this backup"}
	case errors.Is(err, unix.ENOTDIR):
		return &Error{Code: CodeNotDir, Msg: "a name in the path is not a folder"}
	case errors.Is(err, unix.EISDIR):
		return &Error{Code: CodeNotFile, Msg: "this is a folder, not a file"}
	case errors.Is(err, unix.EACCES), errors.Is(err, unix.EPERM):
		return &Error{Code: CodeNotReadable, Msg: "the backup reader may not read this"}
	case errors.Is(err, unix.EXDEV):
		return &Error{Code: CodeBadPath, Msg: "the path leaves the backup"}
	case errors.Is(err, unix.ENAMETOOLONG):
		return &Error{Code: CodeBadPath, Msg: "a name in the path is too long"}
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return &Error{Code: CodeInternal, Msg: errno.Error()}
	}
	return &Error{Code: CodeInternal, Msg: "read error"}
}

// openDir opens the folder at path for reading.
func (t *DirTree) openDir(path [][]byte) (int, error) {
	if err := ValidatePath(path); err != nil {
		return -1, err
	}
	fd, err := t.openAt(t.root, relPath(path), unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		return -1, mapErr(err)
	}
	return fd, nil
}

// split opens the folder that holds the last name of path.
func (t *DirTree) openParent(path [][]byte) (fd int, name string, err error) {
	if len(path) == 0 {
		return -1, "", &Error{Code: CodeBadPath, Msg: "the root has no parent"}
	}
	if err := ValidatePath(path); err != nil {
		return -1, "", err
	}
	fd, err = t.openDir(path[:len(path)-1])
	return fd, string(path[len(path)-1]), err
}

func entryOf(name string, st *unix.Stat_t) Entry {
	e := Entry{
		Name:  []byte(name),
		Size:  st.Size,
		Mode:  uint32(st.Mode) & 0o7777,
		UID:   st.Uid,
		GID:   st.Gid,
		MTime: time.Unix(st.Mtim.Sec, st.Mtim.Nsec).UTC(),
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFREG:
		e.Type = TypeFile
	case unix.S_IFDIR:
		e.Type = TypeDir
	case unix.S_IFLNK:
		e.Type = TypeSymlink
	default:
		e.Type = TypeOther
	}
	return e
}

// statAt describes name in the folder dirfd, without following a link.
func statAt(dirfd int, name string) (Entry, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return Entry{}, mapErr(err)
	}
	e := entryOf(name, &st)
	switch e.Type {
	case TypeSymlink:
		buf := make([]byte, unix.PathMax)
		n, err := unix.Readlinkat(dirfd, name, buf)
		if err != nil {
			return Entry{}, mapErr(err)
		}
		e.Link = buf[:n]
		e.Readable = true
	case TypeFile:
		e.Readable = unix.Faccessat(dirfd, name, unix.R_OK, 0) == nil
	case TypeDir:
		e.Readable = unix.Faccessat(dirfd, name, unix.R_OK|unix.X_OK, 0) == nil
	}
	return e, nil
}

// List returns up to limit entries of the folder at path whose names sort
// after cursor.
func (t *DirTree) List(path [][]byte, cursor []byte, limit int) (ListPage, error) {
	if limit <= 0 || limit > defaultListLimit*10 {
		limit = defaultListLimit
	}
	fd, err := t.openDir(path)
	if err != nil {
		return ListPage{}, err
	}
	dir := os.NewFile(uintptr(fd), "folder")
	defer func() { _ = dir.Close() }()

	var names []string
	for {
		batch, err := dir.Readdirnames(10000)
		for _, n := range batch {
			if len(cursor) == 0 || n > string(cursor) {
				names = append(names, n)
			}
		}
		if len(names) > MaxDirEntries {
			return ListPage{}, &Error{Code: CodeInternal, Msg: fmt.Sprintf("the folder has more than %d names", MaxDirEntries)}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return ListPage{}, mapErr(err)
		}
	}
	sort.Strings(names)
	page := ListPage{Entries: []Entry{}}
	for _, n := range names {
		if len(page.Entries) == limit {
			page.Next = []byte(page.Entries[limit-1].Name)
			break
		}
		e, err := statAt(fd, n)
		if err != nil {
			var ae *Error
			if errors.As(err, &ae) && ae.Code == CodeNotFound {
				continue // removed while we looked
			}
			return ListPage{}, err
		}
		page.Entries = append(page.Entries, e)
	}
	return page, nil
}

// Stat describes the name at path. A link is described, not followed.
func (t *DirTree) Stat(path [][]byte) (Entry, error) {
	if len(path) == 0 {
		var st unix.Stat_t
		if err := unix.Fstat(t.root, &st); err != nil {
			return Entry{}, mapErr(err)
		}
		e := entryOf("", &st)
		e.Readable = true
		return e, nil
	}
	fd, name, err := t.openParent(path)
	if err != nil {
		return Entry{}, err
	}
	defer func() { _ = unix.Close(fd) }()
	return statAt(fd, name)
}

type regularFile struct {
	f    *os.File
	size int64
}

func (r *regularFile) ReadAt(p []byte, off int64) (int, error) { return r.f.ReadAt(p, off) }
func (r *regularFile) Size() int64                             { return r.size }
func (r *regularFile) Close() error                            { return r.f.Close() }

// Open opens the regular file at path. A link, a folder, a pipe, a device and
// a socket are refused.
func (t *DirTree) Open(path [][]byte) (File, error) {
	pfd, name, err := t.openParent(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(pfd) }()
	// O_NONBLOCK so that a pipe planted in the backup cannot block the open;
	// the file type is checked before anything is read.
	fd, err := t.openAt(pfd, name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOCTTY)
	if err != nil {
		return nil, mapErr(err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return nil, mapErr(err)
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFREG:
	case unix.S_IFDIR:
		_ = unix.Close(fd)
		return nil, &Error{Code: CodeNotFile, Msg: "this is a folder, not a file"}
	default:
		_ = unix.Close(fd)
		return nil, &Error{Code: CodeSpecial, Msg: "this is not a regular file"}
	}
	_ = unix.SetNonblock(fd, false)
	return &regularFile{f: os.NewFile(uintptr(fd), "file"), size: st.Size}, nil
}

// Tar writes the file or folder at path as a tar stream.
func (t *DirTree) Tar(w io.Writer, path [][]byte) (TarReport, error) {
	return writeTar(t, w, path)
}
