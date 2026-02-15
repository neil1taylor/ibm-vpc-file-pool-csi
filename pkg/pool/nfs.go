package pool

import "os"

// NFSOperations wraps filesystem calls for testability.
// The real implementation delegates to os.* functions.
// Tests use FakeNFSOperations (in test files).
type NFSOperations interface {
	MkdirAll(path string, perm os.FileMode) error
	RemoveAll(path string) error
	Stat(path string) (os.FileInfo, error)
	Chown(path string, uid, gid int) error
	Chmod(path string, mode os.FileMode) error
}

type realNFSOperations struct{}

// NewRealNFSOperations returns an NFSOperations that delegates to the real OS.
func NewRealNFSOperations() NFSOperations {
	return &realNFSOperations{}
}

func (r *realNFSOperations) MkdirAll(path string, perm os.FileMode) error {
	return os.MkdirAll(path, perm)
}

func (r *realNFSOperations) RemoveAll(path string) error {
	return os.RemoveAll(path)
}

func (r *realNFSOperations) Stat(path string) (os.FileInfo, error) {
	return os.Stat(path)
}

func (r *realNFSOperations) Chown(path string, uid, gid int) error {
	return os.Chown(path, uid, gid)
}

func (r *realNFSOperations) Chmod(path string, mode os.FileMode) error {
	return os.Chmod(path, mode)
}
