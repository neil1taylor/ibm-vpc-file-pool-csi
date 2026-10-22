// Package browse reads a backup tree for the browse helper. It is read-only.
// A backup is data that someone else controlled, so the tree is never trusted:
// no link is followed, no name is used as a shell word, and no path can leave
// the root. See docs/specs/2026-10-22-backup-file-restore.md.
package browse

import (
	"io"
	"time"
)

// Entry types, as sent in the API.
const (
	TypeFile    = "file"
	TypeDir     = "dir"
	TypeSymlink = "symlink"
	TypeOther   = "other" // a device, a pipe or a socket
)

// Limits on a path.
const (
	MaxNameLen   = 255
	MaxPathDepth = 256
)

// MaxDirEntries is the most names one folder may hold for a listing. The names
// of a folder are read and sorted in memory, so a folder of millions of names
// is refused instead of using all of the helper's memory.
const MaxDirEntries = 5_000_000

// Entry describes one name in a folder. Names and link targets are raw bytes
// (base64 in JSON), because a Linux file name is not always text.
type Entry struct {
	Name     []byte    `json:"name"`
	Type     string    `json:"type"`
	Size     int64     `json:"size"`
	Mode     uint32    `json:"mode"` // permission bits, with setuid, setgid and sticky
	UID      uint32    `json:"uid"`
	GID      uint32    `json:"gid"`
	MTime    time.Time `json:"mtime"`
	Link     []byte    `json:"link,omitempty"` // the text of a symbolic link; never followed
	Readable bool      `json:"readable"`       // false if this session's user may not read it
}

// ListPage is one page of a folder, in order of name (byte by byte).
type ListPage struct {
	Entries []Entry `json:"entries"`
	// Next is the cursor for the next page. It is empty on the last page.
	Next []byte `json:"next,omitempty"`
}

// Error codes.
const (
	CodeBadPath     = "bad_path"
	CodeNotFound    = "not_found"
	CodeNotDir      = "not_a_folder"
	CodeNotFile     = "not_a_file"
	CodeLink        = "link_refused"
	CodeNotReadable = "not_readable"
	CodeSpecial     = "special_file"
	CodeInternal    = "internal"
)

// Error is an error with a code that the API passes on.
type Error struct {
	Code string
	Msg  string
}

func (e *Error) Error() string { return e.Code + ": " + e.Msg }

// File is an open regular file.
type File interface {
	ReadAt(p []byte, off int64) (int, error)
	Size() int64
	Close() error
}

// TarReport says what a tar stream left out.
type TarReport struct {
	Files      int // regular files written
	Links      int // symbolic links written as links
	Skipped    int // special files (devices, pipes, sockets) not written
	Unreadable int // files this session could not read
}

// Tree is a read-only view of one backup. A path is a list of names below the
// root; the empty path is the root. Every method is safe to call from many
// goroutines.
type Tree interface {
	List(path [][]byte, cursor []byte, limit int) (ListPage, error)
	Stat(path [][]byte) (Entry, error)
	Open(path [][]byte) (File, error)
	// Tar writes the file or folder at path to w as a tar stream. Links are
	// written as links.
	Tar(w io.Writer, path [][]byte) (TarReport, error)
}

// Walker is an optional part of a Tree: it calls fn once for every entry of a
// folder, in no particular order, reading the folder once. A tar uses it so
// that each folder is read once and not once per page.
type Walker interface {
	Walk(path [][]byte, fn func(Entry) error) error
}
