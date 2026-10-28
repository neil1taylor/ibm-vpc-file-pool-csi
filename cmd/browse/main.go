// Command browse is the helper that reads one backup for the browse feature.
// It runs in a pod that mounts the backup read-only, serves a small API with a
// session token, and ends by itself when it has been idle or has lived too
// long. See docs/specs/2026-10-22-backup-file-restore.md.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/cosbrowse"
	"k8s.io/klog/v2"
)

var version = "dev"

const modeServer = "browse-server"

// openCOS opens a backup in COS. Tests replace it.
var openCOS = func(ctx context.Context, src cosbrowse.Source, at time.Time, kopia, stateDir string, log io.Writer) (*cosbrowse.Opened, error) {
	return cosbrowse.Open(ctx, src, at, cosbrowse.OpenOptions{Kopia: kopia, StateDir: stateDir, Log: log})
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr, nil))
}

// run returns the exit code: 0 when the session ended as planned, 2 for a bad
// command line, 1 for anything else. onListen, if set, is told the address.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, onListen func(net.Addr)) int {
	fs := flag.NewFlagSet("browse", flag.ContinueOnError)
	fs.SetOutput(stderr)
	klog.InitFlags(fs)
	mode := fs.String("mode", "", "what to run; only "+modeServer+" exists")
	root := fs.String("root", "", "the folder that holds the backup, mounted read-only (not used with --cos-source)")
	cosSource := fs.String("cos-source", "", "JSON that names a backup exported to COS to read instead of a folder; the keys are files at fixed places (no secret is in it)")
	kopia := fs.String("kopia", "kopia", "the Kopia program (used with --cos-source)")
	kopiaState := fs.String("kopia-state-dir", cosbrowse.DefaultStateDir, "a writable folder for Kopia's settings and cache (used with --cos-source)")
	listen := fs.String("listen", ":8080", "address to listen on")
	tokenFile := fs.String("token-file", "", "file that holds the session token")
	idle := fs.Duration("idle", 15*time.Minute, "end the session after this long without use")
	maxLife := fs.Duration("max-life", 2*time.Hour, "end the session after this long, even if it is in use")
	streams := fs.Int("max-streams", 8, "how many downloads may run at once")
	moments := fs.String("moments", "", "folder of earlier moments (20261022T091500Z, ...); needed for --at")
	at := fs.String("at", "", "read the newest moment at or before this time (RFC 3339); needs --moments")
	tlsCert := fs.String("tls-cert-file", "", "the session certificate (PEM)")
	tlsKey := fs.String("tls-key-file", "", "the session key (PEM)")
	termLog := fs.String("termination-log", "/dev/termination-log", "file that gets the reason the session ended")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	switch {
	case *mode != modeServer:
		_, _ = fmt.Fprintf(stderr, "--mode must be %s\n", modeServer)
		return 2
	case *cosSource != "" && (*root != "" || *moments != ""):
		_, _ = fmt.Fprintln(stderr, "--cos-source cannot be used with --root or --moments")
		return 2
	case *cosSource == "" && *root == "":
		_, _ = fmt.Fprintln(stderr, "--root is required")
		return 2
	case *tokenFile == "":
		_, _ = fmt.Fprintln(stderr, "--token-file is required")
		return 2
	case *tlsCert == "" || *tlsKey == "":
		_, _ = fmt.Fprintln(stderr, "--tls-cert-file and --tls-key-file are required: the API is never served in plain HTTP")
		return 2
	case *at != "" && *moments == "" && *cosSource == "":
		_, _ = fmt.Fprintln(stderr, "--at needs --moments")
		return 2
	}
	var atTime time.Time
	if *at != "" {
		t, err := time.Parse(time.RFC3339, *at)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "--at must be an RFC 3339 time")
			return 2
		}
		atTime = t
	}
	raw, err := os.ReadFile(*tokenFile)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "cannot read the session token file")
		return 1
	}
	token := strings.TrimSpace(string(raw))
	if err = browse.CheckToken(token); err != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	tlsConfig, err := loadTLS(*tlsCert, *tlsKey)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	opts := browse.Options{Idle: *idle, MaxLife: *maxLife, MaxStreams: *streams}
	var (
		tree browse.Tree
		walk bool
	)
	if *cosSource != "" {
		cosTree, info, moments, cerr := startCOS(ctx, *cosSource, atTime, *kopia, *kopiaState, stderr)
		if cerr != nil {
			failStart(stderr, *termLog, cerr)
			return 1
		}
		defer func() { _ = cosTree.Close() }()
		opts.Info, opts.Moments, tree = info, moments, cosTree
	} else {
		readRoot, info, rerr := browse.ChooseRoot(*root, *moments, atTime)
		if rerr != nil {
			_, _ = fmt.Fprintf(stderr, "%v\n", rerr)
			return 1
		}
		opts.Info = info
		if *moments != "" {
			momentsDir := *moments
			opts.Moments = func() ([]string, error) { return browse.MomentNames(momentsDir) }
		}
		dirTree, derr := browse.NewTree(readRoot)
		if derr != nil {
			_, _ = fmt.Fprintf(stderr, "open the root folder: %v\n", derr)
			return 1
		}
		defer func() { _ = dirTree.Close() }()
		tree, walk = dirTree, dirTree.Walking()
	}
	server, err := browse.NewServer(tree, token, opts)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}

	rawLn, err := net.Listen("tcp", *listen)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "listen: %v\n", err)
		return 1
	}
	ln := tls.NewListener(rawLn, tlsConfig)
	opened := "openat2"
	switch {
	case *cosSource != "":
		opened = "object IDs from the COS repository"
	case walk:
		opened = "folder by folder (openat2 is not available)"
	}
	_, _ = fmt.Fprintf(stdout, "browse %s serving on %s, opening names with %s\n", version, rawLn.Addr(), opened)
	if onListen != nil {
		onListen(rawLn.Addr())
	}
	reason, err := server.Run(ctx, ln)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "serve: %v\n", err)
		return 1
	}
	_ = os.WriteFile(*termLog, []byte(reason), 0o600) //nolint:gosec // the path is an operator flag, not user input
	klog.InfoS("Browse session ended", "reason", reason)
	klog.Flush()
	return 0
}

// startCOS opens the backup in COS that the JSON names. Every failure is a
// sentence for a person, because it goes to the termination log and from there
// to the status of the session.
func startCOS(ctx context.Context, sourceJSON string, at time.Time, kopia, stateDir string, log io.Writer) (*cosbrowse.Tree, browse.Info, func() ([]string, error), error) {
	var src cosbrowse.Source
	dec := json.NewDecoder(strings.NewReader(sourceJSON))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&src); err != nil {
		return nil, browse.Info{}, nil, fmt.Errorf("--cos-source is not valid: %w", err)
	}
	opened, err := openCOS(ctx, src, at, kopia, stateDir, log)
	if err != nil {
		return nil, browse.Info{}, nil, err
	}
	return opened.Tree, opened.Info, opened.Moments, nil
}

// failStart prints why the session could not start and leaves the same words in
// the termination log, where the controller reads them.
func failStart(stderr io.Writer, termLog string, err error) {
	msg := err.Error()
	_, _ = fmt.Fprintln(stderr, msg)
	if len(msg) > 900 {
		msg = msg[:900] + "..."
	}
	_ = os.WriteFile(termLog, []byte(msg), 0o600) //nolint:gosec // the path is an operator flag, not user input
}

func loadTLS(certFile, keyFile string) (*tls.Config, error) {
	cert, err := os.ReadFile(certFile) //nolint:gosec // operator flag
	if err != nil {
		return nil, fmt.Errorf("cannot read the session certificate")
	}
	key, err := os.ReadFile(keyFile) //nolint:gosec // operator flag
	if err != nil {
		return nil, fmt.Errorf("cannot read the session certificate key")
	}
	return browse.ServerTLSConfig(cert, key)
}
