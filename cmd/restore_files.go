package main

import (
	"context"
	"encoding/json"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/filerestore"
	"k8s.io/klog/v2"
)

// restoreFilesTimeout is a hard stop for one run of the Job. The Job has its own
// deadline too; this one makes the Job clean up before it is killed.
const restoreFilesTimeout = 4 * time.Hour

// runRestoreFiles copies chosen files out of a backup into a claim. A
// BackupFileRestore starts it as a Job, with the backup and the target mounted,
// the spec as JSON in an argument and the list of paths in a file (paths are
// never arguments). The outcome goes to the termination log, for the controller.
func runRestoreFiles(specJSON, pathsFile string) {
	os.Exit(restoreFilesMain(specJSON, pathsFile, "/dev/termination-log"))
}

// restoreFilesMain returns the exit code: 0 when the Job reached a decision,
// good or bad (the outcome in the termination log says which), 2 for a bad
// command line, 1 when it was stopped.
func restoreFilesMain(specJSON, pathsFile, terminationLog string) int {
	var spec filerestore.JobSpec
	if err := json.Unmarshal([]byte(specJSON), &spec); err != nil || pathsFile == "" {
		klog.ErrorS(nil, "restore-files needs --restore-files-spec as JSON and --restore-files-paths")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, restoreFilesTimeout)
	defer cancel()

	out, err := filerestore.RunJob(ctx, spec, pathsFile)
	// The message holds counts and a reason that names no path.
	_ = os.WriteFile(terminationLog, []byte(filerestore.EncodeOutcome(out)), 0o600) //nolint:gosec // the path is a fixed argument of the caller
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
