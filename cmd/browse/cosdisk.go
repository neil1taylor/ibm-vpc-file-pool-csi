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
	"strings"
	"sync/atomic"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/cosbrowse"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/cosdisk"
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
	dav, err := repo.StartWebDAV(ctx, pick.Snapshot.ID, nil, time.Minute)
	if err != nil {
		failStart(stderr, a.termLog, err)
		return 1
	}
	defer dav.Stop()
	hctx, cancel := context.WithTimeout(ctx, time.Minute)
	httpSrc, err := cosdisk.NewHTTPSource(hctx, &http.Client{Timeout: 5 * time.Minute}, dav.URL)
	cancel()
	if err != nil {
		failStart(stderr, a.termLog, fmt.Errorf("the backup has no readable disk: %w", err))
		return 1
	}
	disk := cosdisk.New(httpSrc, cosdisk.Options{SegmentSize: a.segmentMB << 20, ReadAhead: a.readAhead, MaxBytes: int64(a.cacheMB) << 20})
	defer func() { _ = disk.Close() }()
	diskHandler, err := cosdisk.NewHandler(disk, token)
	if err != nil {
		failStart(stderr, a.termLog, err)
		return 1
	}

	var lastUse atomic.Int64
	lastUse.Store(time.Now().UnixNano())
	moment := pick.Moment.UTC().Format(time.RFC3339)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		_, _ = io.WriteString(w, "ok")
	})
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastUse.Store(time.Now().UnixNano())
		w.Header().Set(cosdisk.MomentHeader, moment)
		diskHandler.ServeHTTP(w, r)
	}))
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
