// Command browse is the helper that reads one backup for the browse feature.
// It runs in a pod that mounts the backup read-only, serves a small API with a
// session token, and ends by itself when it has been idle or has lived too
// long. See docs/specs/2026-10-22-backup-file-restore.md.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
	"k8s.io/klog/v2"
)

var version = "dev"

const modeServer = "browse-server"

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
	root := fs.String("root", "", "the folder that holds the backup, mounted read-only")
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
	case *root == "":
		_, _ = fmt.Fprintln(stderr, "--root is required")
		return 2
	case *tokenFile == "":
		_, _ = fmt.Fprintln(stderr, "--token-file is required")
		return 2
	case *tlsCert == "" || *tlsKey == "":
		_, _ = fmt.Fprintln(stderr, "--tls-cert-file and --tls-key-file are required: the API is never served in plain HTTP")
		return 2
	case *at != "" && *moments == "":
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
	tlsConfig, err := loadTLS(*tlsCert, *tlsKey)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	opts := browse.Options{Idle: *idle, MaxLife: *maxLife, MaxStreams: *streams}
	readRoot, info, err := chooseRoot(*root, *moments, atTime)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	opts.Info = info
	server, tree, err := newServer(strings.TrimSpace(string(raw)), readRoot, opts)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	defer func() { _ = tree.Close() }()

	rawLn, err := net.Listen("tcp", *listen)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "listen: %v\n", err)
		return 1
	}
	ln := tls.NewListener(rawLn, tlsConfig)
	opened := "openat2"
	if tree.Walking() {
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

func newServer(token, root string, opts browse.Options) (*browse.Server, *browse.DirTree, error) {
	if err := browse.CheckToken(token); err != nil {
		return nil, nil, err
	}
	tree, err := browse.NewTree(root)
	if err != nil {
		return nil, nil, fmt.Errorf("open the root folder: %w", err)
	}
	s, err := browse.NewServer(tree, token, opts)
	if err != nil {
		_ = tree.Close()
		return nil, nil, err
	}
	return s, tree, nil
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

// chooseRoot says which folder to read. Without --at it is the latest copy.
// With --at it is the newest moment folder at or before the time; with no
// moment folders yet, the only copy is the latest one. The info says which
// moment the session reads.
func chooseRoot(latest, momentsDir string, at time.Time) (string, browse.Info, error) {
	if momentsDir == "" {
		return latest, browse.Info{Latest: true}, nil
	}
	names, err := momentNames(momentsDir)
	if err != nil {
		return "", browse.Info{}, err
	}
	pick, newest, err := browse.PickMoment(names, at)
	if err != nil {
		return "", browse.Info{}, err
	}
	if pick == "" {
		return latest, browse.Info{Latest: true}, nil
	}
	if at.IsZero() {
		return latest, browse.Info{Moment: pick, Latest: true}, nil
	}
	return filepath.Join(momentsDir, pick), browse.Info{Moment: pick, Latest: newest}, nil
}

// momentNames lists the moments folder through the safe reader, so a link
// named like a moment is not taken for a folder.
func momentNames(dir string) ([]string, error) {
	tree, err := browse.NewTree(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil // no moments yet
		}
		return nil, fmt.Errorf("open the moments folder: %w", err)
	}
	defer func() { _ = tree.Close() }()
	var names []string
	var cursor []byte
	for {
		page, err := tree.List(nil, cursor, 1000)
		if err != nil {
			return nil, fmt.Errorf("list the moments folder: %w", err)
		}
		for _, e := range page.Entries {
			if e.Type == browse.TypeDir {
				names = append(names, string(e.Name))
			}
		}
		if len(page.Next) == 0 {
			return names, nil
		}
		cursor = page.Next
	}
}
