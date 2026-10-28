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

// restoreFilesMain returns the exit code.
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
		return 1
	}
	klog.InfoS("File restore done", "mode", spec.Mode, "files", out.Files, "dirs", out.Dirs, "links", out.Links,
		"skipped", out.Skipped, "unreadable", out.Unreadable, "bytes", out.Bytes)
	return 0
}
