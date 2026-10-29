//go:build !unix

package filerestore

import "errors"

// DirDest needs a Unix system. On other systems it only exists so that the
// code builds.
type DirDest struct{}

var errNeedsUnix = errors.New("restoring files needs a Unix system")

func NewVolumeDest(string, [][]byte, int64) (*DirDest, error)         { return nil, errNeedsUnix }
func NewFolderDest(string, [][]byte, string, int64) (*DirDest, error) { return nil, errNeedsUnix }
func NewFolderDestAt(string, string, int64) (*DirDest, error)         { return nil, errNeedsUnix }
func (*DirDest) Commit(string) (string, error)                        { return "", errNeedsUnix }
func (*DirDest) Close() error                                         { return nil }
func (*DirDest) Mkdir([][]byte) error                                 { return errNeedsUnix }
func (*DirDest) FinishDir([][]byte, Meta) error                       { return errNeedsUnix }
func (*DirDest) Create([][]byte) (DestFile, error)                    { return nil, errNeedsUnix }
func (*DirDest) Symlink([][]byte, []byte, Meta) error                 { return errNeedsUnix }
func (*DirDest) Free() (int64, error)                                 { return 0, errNeedsUnix }
func (*DirDest) Abort() error                                         { return errNeedsUnix }

func folderOwner(string) (int64, int64, error) { return 0, 0, errNeedsUnix }
