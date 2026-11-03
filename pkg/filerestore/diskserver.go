package filerestore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/cosdisk"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/guestfs"
)

const (
	remoteCacheBytes = 32 << 20
	remoteReadAhead  = 4
)

// openRemoteGuest opens the file systems inside a VM disk that a disk server
// holds (a VM disk in COS). The Job has no key: it pins the disk server's
// certificate and sends its token. The moment is the one the disk server chose
// when it started, so the moment asked for is not used here.
func openRemoteGuest(ctx context.Context, g GuestSource) (browse.Tree, browse.Info, func() error, error) {
	cert, err := os.ReadFile(g.Disk.CertFile) //nolint:gosec // named by the Job's own spec
	if err != nil {
		return nil, browse.Info{}, nil, errors.New("cannot read the certificate of the disk server")
	}
	rawToken, err := os.ReadFile(g.Disk.TokenFile) //nolint:gosec // named by the Job's own spec
	if err != nil {
		return nil, browse.Info{}, nil, errors.New("cannot read the token of the disk server")
	}
	client, err := browse.PinnedClient(cert)
	if err != nil {
		return nil, browse.Info{}, nil, fmt.Errorf("the certificate of the disk server is not valid: %w", err)
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	src, err := cosdisk.NewRemoteSource(cctx, client, g.Disk.URL, strings.TrimSpace(string(rawToken)))
	cancel()
	if err != nil {
		return nil, browse.Info{}, nil, fmt.Errorf("the disk server could not be reached: %w", err)
	}
	disk := cosdisk.New(src, cosdisk.Options{ReadAhead: remoteReadAhead, MaxBytes: remoteCacheBytes})
	o, err := guestfs.OpenRemote(ctx, guestfs.RemoteOptions{
		Disk: disk, Moment: src.Moment(), Socket: g.Socket,
		Runner: &guestfs.ExecRunner{Env: guestfs.Environment(g.Appliance, guestfs.HasKVM())},
		Crash:  g.Consistency == "CrashConsistent",
	})
	if err != nil {
		_ = disk.Close()
		return nil, browse.Info{}, nil, err
	}
	return o.Tree, o.Info, func() error { o.Close(); return disk.Close() }, nil
}
