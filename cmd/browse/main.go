// Command browse is the helper that reads one backup for the browse feature.
// It runs in a pod that mounts the backup read-only, serves a small API with a
// session token, and ends by itself when it has been idle or has lived too
// long. See docs/specs/2026-10-22-backup-file-restore.md.
package main

import (
	"context"
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
	}
	raw, err := os.ReadFile(*tokenFile)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "cannot read the session token file")
		return 1
	}
	server, tree, err := newServer(strings.TrimSpace(string(raw)), *root, browse.Options{Idle: *idle, MaxLife: *maxLife, MaxStreams: *streams})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	defer func() { _ = tree.Close() }()

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "listen: %v\n", err)
		return 1
	}
	opened := "openat2"
	if tree.Walking() {
		opened = "folder by folder (openat2 is not available)"
	}
	_, _ = fmt.Fprintf(stdout, "browse %s serving on %s, opening names with %s\n", version, ln.Addr(), opened)
	if onListen != nil {
		onListen(ln.Addr())
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
