package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/cosbrowse"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/cosdisk"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/guestfs"
	"k8s.io/klog/v2"
)

// modeCOSDisk serves the VM disk of an export in COS, read-only, to the guest
// helper of a session. It holds the keys and reads COS through Kopia, and it
// never parses the guest's file system: it hands out bytes at offsets over
// HTTPS, behind the session certificate and token. See
// docs/specs/2026-10-29-vm-disks-in-cos.md, section 2.
const modeCOSDisk = "cos-disk-server"

// diskRepo is the part of Kopia that the disk server uses.
type diskRepo interface {
	Snapshots(ctx context.Context) ([]cosbrowse.Snapshot, error)
	StartWebDAV(ctx context.Context, snapshotID string, find func() (int, bool), wait time.Duration) (*cosbrowse.WebDAV, error)
}

// openDiskRepo connects to the repository in COS. Tests replace it.
var openDiskRepo = func(ctx context.Context, src cosbrowse.Source, kopia, stateDir string, log io.Writer) (diskRepo, error) {
	return cosbrowse.OpenKopia(ctx, src, cosbrowse.OpenOptions{Kopia: kopia, StateDir: stateDir, Log: log})
}

// cosDiskArgs are the flags of the mode.
type cosDiskArgs struct {
	source, kopia, stateDir, listen string
	tokenFile, certFile, keyFile    string
	termLog                         string
	at                              time.Time
	idle, maxLife                   time.Duration
	cacheMB, segmentMB, readAhead   int
}

func (a cosDiskArgs) validate() error {
	switch {
	case a.source == "":
		return errors.New("--cos-source is required")
	case a.tokenFile == "":
		return errors.New("--token-file is required")
	case a.certFile == "" || a.keyFile == "":
		return errors.New("--tls-cert-file and --tls-key-file are required: the disk is never served in plain HTTP")
	case a.cacheMB < 1 || a.cacheMB > 1024:
		return errors.New("--cache-mb must be between 1 and 1024")
	case a.segmentMB < 1 || a.segmentMB > 16:
		return errors.New("--segment-mb must be between 1 and 16")
	case a.readAhead < 0 || a.readAhead > 32:
		return errors.New("--read-ahead must be between 0 and 32")
	}
	return nil
}

func runCOSDisk(ctx context.Context, a cosDiskArgs, stdout, stderr io.Writer, onListen func(net.Addr)) int {
	raw, err := os.ReadFile(a.tokenFile)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "cannot read the session token file")
		return 1
	}
	token := strings.TrimSpace(string(raw))
	if err = browse.CheckToken(token); err != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	tlsConfig, err := loadTLS(a.certFile, a.keyFile)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	var src cosbrowse.Source
	dec := json.NewDecoder(strings.NewReader(a.source))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&src); err != nil {
		failStart(stderr, a.termLog, fmt.Errorf("--cos-source is not valid: %w", err))
		return 1
	}
	if err = src.Validate(); err != nil {
		failStart(stderr, a.termLog, err)
		return 1
	}
	var fileListURL string
	repo, err := openDiskRepo(ctx, src, a.kopia, a.stateDir, stderr)
	if err != nil {
		failStart(stderr, a.termLog, err)
		return 1
	}
	snaps, err := repo.Snapshots(ctx)
	if err != nil {
		failStart(stderr, a.termLog, err)
		return 1
	}
	pick, err := cosbrowse.ChooseBackup(snaps, cosbrowse.Params{Policy: src.Policy, Volume: src.Volume, At: a.at})
	if err != nil {
		failStart(stderr, a.termLog, err)
		return 1
	}
	davClient := &http.Client{Timeout: 5 * time.Minute}
	// A restore point is named in whole seconds (as the listing prints it) but
	// keeps its nanoseconds, so the view is asked for the end of that second.
	viewAt := guestfs.AtEndOfSecond(a.at)
	older := !viewAt.IsZero() && viewAt.Before(pick.Moment)
	mv := &momentsView{ctx: ctx, repo: repo, snap: pick.Moments, client: davClient}
	defer mv.stop()
	diskURL, served := "", pick.Moment
	if older {
		// An older restore point is a full copy in a folder of the export's
		// moments snapshot. The newest folder at or before the time is served.
		folders, lerr := mv.list(ctx)
		if lerr != nil {
			failStart(stderr, a.termLog, olderRefusal(lerr, a.at))
			return 1
		}
		f, perr := cosdisk.PickRestorePointFolder(folders, viewAt)
		if perr != nil {
			failStart(stderr, a.termLog, olderRefusal(perr, a.at))
			return 1
		}
		diskURL, served = mv.base+"/"+f.Name+"/disk.img", f.Moment
	} else {
		dav, derr := repo.StartWebDAV(ctx, pick.Snapshot.ID, nil, time.Minute)
		if derr != nil {
			failStart(stderr, a.termLog, derr)
			return 1
		}
		defer dav.Stop()
		diskURL = dav.URL
		fileListURL = dav.URL
	}
	hctx, cancel := context.WithTimeout(ctx, time.Minute)
	httpSrc, herr := cosdisk.NewHTTPSource(hctx, davClient, diskURL)
	cancel()
	if herr != nil {
		failStart(stderr, a.termLog, fmt.Errorf("the backup has no readable disk: %w", herr))
		return 1
	}
	disk := cosdisk.New(httpSrc, cosdisk.Options{SegmentSize: a.segmentMB << 20, ReadAhead: a.readAhead, MaxBytes: int64(a.cacheMB) << 20})
	defer func() { _ = disk.Close() }()
	var lastUse atomic.Int64
	lastUse.Store(time.Now().UnixNano())
	moment := served.UTC().Format(time.RFC3339)
	opts := []cosdisk.HandlerOption{
		cosdisk.WithOnAuthed(func(w http.ResponseWriter, _ *http.Request) func() {
			lastUse.Store(time.Now().UnixNano())
			w.Header().Set(cosdisk.MomentHeader, moment)
			return func() { lastUse.Store(time.Now().UnixNano()) }
		}),
	}
	if !older {
		// The saved file list belongs to the export's own restore point. For an
		// older one the folders are read from the disk.
		opts = append(opts, cosdisk.WithFileList(func(ctx context.Context) (io.ReadCloser, int64, error) { return fetchFileList(ctx, fileListURL) }))
	}
	opts = append(opts, cosdisk.WithRestorePoints(restorePointsOf(mv, pick.Moment)))
	diskHandler, err := cosdisk.NewHandler(disk, token, opts...)
	if err != nil {
		failStart(stderr, a.termLog, err)
		return 1
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		_, _ = io.WriteString(w, "ok")
	})
	mux.Handle("/", diskHandler)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	rawLn, err := net.Listen("tcp", a.listen)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "listen: %v\n", err)
		return 1
	}
	ln := tls.NewListener(rawLn, tlsConfig)
	_, _ = fmt.Fprintf(stdout, "browse %s serving the disk of %s at %s on %s\n", version, src.Volume, moment, rawLn.Addr())
	if onListen != nil {
		onListen(rawLn.Addr())
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	reason := "stopped"
	tick := time.NewTicker(minDuration(a.idle/4, time.Second))
	defer tick.Stop()
	life := time.NewTimer(a.maxLife)
	defer life.Stop()
wait:
	for {
		select {
		case <-ctx.Done():
			break wait
		case <-life.C:
			reason = "max-life"
			break wait
		case <-tick.C:
			if time.Since(time.Unix(0, lastUse.Load())) > a.idle {
				reason = "idle"
				break wait
			}
		case err = <-serveErr:
			if !errors.Is(err, http.ErrServerClosed) {
				_, _ = fmt.Fprintf(stderr, "serve: %v\n", err)
				return 1
			}
			break wait
		}
	}
	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()
	_ = srv.Shutdown(sctx)
	_ = os.WriteFile(a.termLog, []byte(reason), 0o600) //nolint:gosec // the path is an operator flag, not user input
	klog.InfoS("Disk server ended", "reason", reason)
	klog.Flush()
	return 0
}

func minDuration(a, b time.Duration) time.Duration {
	if a < time.Millisecond {
		return time.Millisecond
	}
	return min(a, b)
}

// fileListName is the file next to disk.img that the export carries to COS.
const fileListName = "/.filelist.json.gz"

// fetchFileList opens the file list of the snapshot from the Kopia WebDAV
// server. It does not read the list.
func fetchFileList(ctx context.Context, diskURL string) (io.ReadCloser, int64, error) {
	listURL := strings.TrimSuffix(diskURL, "/disk.img") + fileListName
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, listURL, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode != http.StatusOK || resp.ContentLength < 0 {
		_ = resp.Body.Close()
		return nil, 0, cosdisk.ErrNoFileList
	}
	return resp.Body, resp.ContentLength, nil
}

// olderRefusal says in words why an older restore point cannot be served.
func olderRefusal(err error, at time.Time) error {
	if errors.Is(err, cosdisk.ErrNoRestorePoint) {
		return fmt.Errorf("this VM disk has no restore point in COS at or before %s (%w)", at.UTC().Format(time.RFC3339), err)
	}
	return fmt.Errorf("the restore point of the VM disk could not be opened: %w", err)
}

// momentsView is the export's moments snapshot, mounted on first use. Its root
// lists the restore point folders, each with a full copy of the disk. The mount
// belongs to the life of the disk server, so it is started with the server's
// context and not with the context of the request that asks for it.
type momentsView struct {
	ctx    context.Context
	repo   diskRepo
	snap   *cosbrowse.Snapshot
	client *http.Client

	mu      sync.Mutex
	dav     *cosbrowse.WebDAV
	base    string
	folders []cosdisk.RestorePointFolder
	listed  bool
}

// list mounts the snapshot (once) and lists its folders (once, when it works).
// An export with no moments snapshot, or one that Kopia cannot list, has none; a
// failure of the mount or the listing is returned, and tried again next time.
func (m *momentsView) list(ctx context.Context) ([]cosdisk.RestorePointFolder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listed || m.snap == nil {
		return m.folders, nil
	}
	if m.dav == nil {
		dav, err := m.repo.StartWebDAV(m.ctx, m.snap.ID, nil, time.Minute)
		if err != nil {
			return nil, err
		}
		m.dav, m.base = dav, strings.TrimSuffix(dav.URL, "/disk.img")
	}
	folders, err := cosdisk.ListRestorePointFolders(ctx, m.client, m.base)
	if err != nil {
		return nil, err
	}
	m.folders, m.listed = folders, true
	return folders, nil
}

func (m *momentsView) stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dav != nil {
		m.dav.Stop()
		m.dav = nil
	}
}

// restorePointsOf lists the restore points of the export: the folders of its
// moments snapshot and the export's own. The snapshot is mounted at the first
// request, and a failed listing is tried again at the next one.
func restorePointsOf(m *momentsView, own time.Time) func(context.Context) ([]time.Time, error) {
	return func(ctx context.Context) ([]time.Time, error) {
		folders, err := m.list(ctx)
		if err != nil {
			return nil, err
		}
		all := make([]time.Time, 0, len(folders)+1)
		for _, f := range folders {
			all = append(all, f.Moment)
		}
		all = append(all, own)
		sort.Slice(all, func(i, j int) bool { return all[i].Before(all[j]) })
		var out []time.Time
		for _, t := range all {
			// A folder is named in whole seconds; the export's own restore point
			// keeps its nanoseconds and wins over the folder of the same second.
			if n := len(out); n > 0 && out[n-1].Truncate(time.Second).Equal(t.Truncate(time.Second)) {
				out[n-1] = t
				continue
			}
			out = append(out, t)
		}
		return out, nil
	}
}
