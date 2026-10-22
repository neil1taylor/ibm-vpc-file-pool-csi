//go:build !linux

package browse

import (
	"errors"
	"io"
)

// DirTree needs Linux (openat2). On other systems it only exists so that the
// code builds.
type DirTree struct{}

var errUnsupported = errors.New("browsing a backup folder needs Linux")

// NewTree is not supported here.
func NewTree(root string) (*DirTree, error) { return nil, errUnsupported }

func (*DirTree) Close() error  { return nil }
func (*DirTree) Walking() bool { return false }
func (*DirTree) List([][]byte, []byte, int) (ListPage, error) {
	return ListPage{}, errUnsupported
}
func (*DirTree) Stat([][]byte) (Entry, error) { return Entry{}, errUnsupported }
func (*DirTree) Open([][]byte) (File, error)  { return nil, errUnsupported }
func (*DirTree) Tar(io.Writer, [][]byte) (TarReport, error) {
	return TarReport{}, errUnsupported
}
