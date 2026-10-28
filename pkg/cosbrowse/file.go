package cosbrowse

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
)

// objectFile is a file in the repository. The repository gives a file as one
// stream from the start, so reads that go forward are served from one open
// stream. A read that goes backward starts the stream again and skips ahead.
// The server and the restore read a file front to back, so this is rare.
type objectFile struct {
	ctx  context.Context
	repo Repo
	oid  string
	size int64

	mu     sync.Mutex
	stream io.ReadCloser
	pos    int64
	closed bool
}

func newObjectFile(ctx context.Context, repo Repo, oid string, size int64) *objectFile {
	return &objectFile{ctx: ctx, repo: repo, oid: oid, size: size}
}

func (f *objectFile) Size() int64 { return f.size }

func (f *objectFile) ReadAt(p []byte, off int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, errors.New("the file is closed")
	}
	if off < 0 {
		return 0, errors.New("negative offset")
	}
	if off >= f.size {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	if f.stream == nil || off != f.pos {
		if err := f.restart(off); err != nil {
			return 0, err
		}
	}
	want := int64(len(p))
	if off+want > f.size {
		want = f.size - off
	}
	n, err := io.ReadFull(f.stream, p[:want])
	f.pos += int64(n)
	switch {
	case err == nil:
		if int64(n) < int64(len(p)) {
			return n, io.EOF
		}
		return n, nil
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		// The stream ended before the size the folder gave: the file is shorter.
		f.drop()
		return n, io.ErrUnexpectedEOF
	}
	f.drop()
	return n, readFailed(err)
}

func (f *objectFile) restart(off int64) error {
	f.drop()
	rc, err := f.repo.Object(f.ctx, f.oid)
	if err != nil {
		return readFailed(err)
	}
	if off > 0 {
		if _, err := io.CopyN(io.Discard, rc, off); err != nil {
			_ = rc.Close()
			if errors.Is(err, io.EOF) {
				return io.ErrUnexpectedEOF
			}
			return readFailed(err)
		}
	}
	f.stream, f.pos = rc, off
	return nil
}

func (f *objectFile) drop() {
	if f.stream != nil {
		_ = f.stream.Close()
		f.stream = nil
	}
}

func (f *objectFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	f.drop()
	return nil
}

var _ browse.File = (*objectFile)(nil)
