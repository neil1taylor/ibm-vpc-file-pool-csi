package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/cosdisk"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/filelist"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/guestfs"
	"k8s.io/klog/v2"
)

const (
	listFromListNotice = "Folders are shown from a list saved when the backup was made. Downloads read the disk."
	listSlowNotice     = "This backup has no saved file list, so folders are read from the disk, which is slow."

	// listMomentSlack is how far the moment in a file list may be from the
	// moment of the disk (the same rule as the controller's).
	listMomentSlack = 30 * time.Minute

	remoteCacheBytes = 32 << 20
	remoteReadAhead  = 4
)

// remoteGuestArgs say where the disk server is.
type remoteGuestArgs struct {
	url, certFile, tokenFile       string
	socket, appliance, consistency string
}

// openRemoteGuestSession opens a VM disk that a disk server holds. The helper
// holds no keys: it has the disk server's certificate to pin and the token for
// it, both from files. Folders come from the file list the disk server has,
// when its moment matches the disk; otherwise from the disk, slowly.
func openRemoteGuestSession(ctx context.Context, a remoteGuestArgs) (*guestSession, error) {
	cert, err := os.ReadFile(a.certFile) //nolint:gosec // operator flag
	if err != nil {
		return nil, errors.New("cannot read the certificate of the disk server")
	}
	rawToken, err := os.ReadFile(a.tokenFile) //nolint:gosec // operator flag
	if err != nil {
		return nil, errors.New("cannot read the token of the disk server")
	}
	token := strings.TrimSpace(string(rawToken))
	client, err := browse.PinnedClient(cert)
	if err != nil {
		return nil, fmt.Errorf("the certificate of the disk server is not valid: %w", err)
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	src, err := cosdisk.NewRemoteSource(cctx, client, a.url, token)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("the disk server could not be reached: %w", err)
	}
	disk := cosdisk.New(src, cosdisk.Options{ReadAhead: remoteReadAhead, MaxBytes: remoteCacheBytes})
	opened, err := guestfs.OpenRemote(ctx, guestfs.RemoteOptions{
		Disk: disk, Moment: src.Moment(), Socket: a.socket, Moments: remoteMoments(client, a.url, token, src.Moment()),
		Runner: newToolRunner(a.appliance, guestfs.HasKVM()), Crash: a.consistency == consistencyCrash,
	})
	if err != nil {
		_ = disk.Close()
		return nil, err
	}
	g := &guestSession{Opened: opened, consistency: a.consistency, tree: opened.Tree, closeExtra: func() { _ = disk.Close() }}
	list := fetchSavedList(ctx, client, a.url, token, src.Moment())
	if list != nil {
		g.tree = filelist.NewTree(list, opened.Tree)
		g.notices = append(g.notices, listFromListNotice)
	} else {
		g.notices = append(g.notices, listSlowNotice)
	}
	return g, nil
}

// fetchSavedList returns the file list of the export if there is one that was
// made for this moment. Any problem gives nil: the disk is read instead.
func fetchSavedList(ctx context.Context, client *http.Client, diskURL, token string, moment time.Time) *filelist.List {
	if moment.IsZero() {
		return nil
	}
	listURL := strings.TrimSuffix(diskURL, cosdisk.DiskPath) + cosdisk.FileListPath
	lctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	rc, err := cosdisk.FetchFileList(lctx, client, listURL, token, cosdisk.MaxFileListBytes)
	if err != nil {
		klog.V(2).InfoS("The saved file list could not be fetched; the disk is read", "err", err)
		return nil
	}
	if rc == nil {
		return nil
	}
	defer func() { _ = rc.Close() }()
	list, err := filelist.Read(rc, filelist.Limits{})
	if err != nil {
		klog.V(2).InfoS("The saved file list could not be read; the disk is read", "err", err)
		return nil
	}
	if d := list.Moment().Sub(moment); d < -listMomentSlack || d > listMomentSlack {
		klog.V(2).InfoS("The saved file list is for another restore point; the disk is read", "list", list.Moment(), "disk", moment)
		return nil
	}
	return list
}

// remoteMoments lists the restore points the disk server holds, as names of
// whole seconds, oldest first. A disk server with no list (an older one) gives
// the restore point it serves, as before.
func remoteMoments(client *http.Client, diskURL, token string, served time.Time) func() ([]string, error) {
	listURL := strings.TrimSuffix(diskURL, cosdisk.DiskPath) + cosdisk.RestorePointsPath
	return func() ([]string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		points, err := cosdisk.FetchRestorePoints(ctx, client, listURL, token)
		if err != nil {
			return nil, err
		}
		if len(points) == 0 && !served.IsZero() {
			points = []time.Time{served}
		}
		var out []string
		for _, p := range points {
			name := p.UTC().Format(browse.MomentLayout)
			if len(out) == 0 || out[len(out)-1] != name {
				out = append(out, name)
			}
		}
		return out, nil
	}
}
