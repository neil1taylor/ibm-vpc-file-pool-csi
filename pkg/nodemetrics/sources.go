package nodemetrics

import (
	"context"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
)

// statfsTimeout bounds one statfs call; a dead NFS server can block it for a long time.
const statfsTimeout = 10 * time.Second

// ReadProcMountStats reads this process's NFS mount statistics from /proc.
func ReadProcMountStats() ([]NFSMount, error) {
	f, err := os.Open("/proc/self/mountstats")
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only
	return ParseMountStats(f)
}

// ResolverFromPools builds a Config.Resolve from a lister of FileSharePools. It
// maps each share's NFS export path to the pool and share name that the
// controller's metrics use.
func ResolverFromPools(list func(context.Context) ([]v1alpha1.FileSharePool, error)) func(context.Context) (map[string]ShareRef, error) {
	return func(ctx context.Context) (map[string]ShareRef, error) {
		pools, err := list(ctx)
		if err != nil {
			return nil, fmt.Errorf("list pools: %w", err)
		}
		out := map[string]ShareRef{}
		for _, p := range pools {
			for _, s := range p.Status.Shares {
				if s.ExportPath != "" {
					out[s.ExportPath] = ShareRef{Pool: p.Name, Share: s.ShareName}
				}
			}
		}
		return out, nil
	}
}

// Statfs returns the size and free bytes of the file system at path. It gives
// up after a timeout, so a hung mount does not stop the collector.
func Statfs(ctx context.Context, path string) (uint64, uint64, error) {
	return statfsWith(ctx, path, statfsTimeout, rawStatfs)
}

func rawStatfs(path string) (uint64, uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bs := uint64(st.Bsize) //nolint:gosec // block size is never negative
	return st.Blocks * bs, st.Bfree * bs, nil
}

func statfsWith(ctx context.Context, path string, timeout time.Duration, fn func(string) (uint64, uint64, error)) (uint64, uint64, error) {
	type result struct {
		total, free uint64
		err         error
	}
	ch := make(chan result, 1) // buffered: a late answer from a hung call is dropped, not leaked
	go func() {
		t, f, err := fn(path)
		ch <- result{t, f, err}
	}()
	select {
	case r := <-ch:
		return r.total, r.free, r.err
	case <-time.After(timeout):
		return 0, 0, fmt.Errorf("statfs %s: no answer in %s", path, timeout)
	case <-ctx.Done():
		return 0, 0, ctx.Err()
	}
}
