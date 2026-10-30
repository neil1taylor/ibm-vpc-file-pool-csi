package filerestore

import (
	"context"
	"encoding/json"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/klog/v2"
)

// mainTimeout is a hard stop for one run of the Job. The Job has its own
// deadline too; this one makes the Job clean up before it is killed.
const mainTimeout = 4 * time.Hour

// Main runs a file restore Job: it reads the spec (JSON) and the list of paths
// (a file; paths are never arguments), does the restore, and writes the outcome
// to the termination log for the controller. Both programs that run the Job use
// it: the driver (snapshot pool and COS) and the browse program of the guest
// image (VM disks). It returns the exit code: 0 when the Job reached a decision,
// good or bad (the outcome in the termination log says which), 2 for a bad
// command line, 1 when it was stopped.
func Main(specJSON, pathsFile, terminationLog string) int {
	var spec JobSpec
	if err := json.Unmarshal([]byte(specJSON), &spec); err != nil || pathsFile == "" {
		klog.ErrorS(nil, "restore-files needs --restore-files-spec as JSON and --restore-files-paths")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, mainTimeout)
	defer cancel()

	out, err := RunJob(ctx, spec, pathsFile)
	// The message holds counts and a reason that names no path.
	_ = os.WriteFile(terminationLog, []byte(EncodeOutcome(out)), 0o600) //nolint:gosec // the path is a fixed argument of the caller
	if err != nil {
		klog.ErrorS(err, "File restore failed", "mode", spec.Mode)
		// The Job runs with a restart policy that starts the container again after
		// a failure, which loses the message, and a refusal (no room, a VM disk
		// claim, a missing path) would only fail again. So a failure the Job has
		// decided on ends with exit code 0 and the reason in the message; the
		// controller reads it as a failed restore. Only a Job that was stopped
		// (SIGTERM, or its time) exits with an error, and is not tried again by us.
		if ctx.Err() != nil {
			return 1
		}
		return 0
	}
	klog.InfoS("File restore done", "mode", spec.Mode, "files", out.Files, "dirs", out.Dirs, "links", out.Links,
		"skipped", out.Skipped, "unreadable", out.Unreadable, "bytes", out.Bytes)
	return 0
}
