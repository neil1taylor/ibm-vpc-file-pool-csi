package main

import (
	"os"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/filerestore"
)

// runRestoreFiles copies chosen files out of a backup into a claim. A
// BackupFileRestore starts it as a Job, with the backup and the target mounted,
// the spec as JSON in an argument and the list of paths in a file (paths are
// never arguments). The outcome goes to the termination log, for the controller.
// The work is in filerestore.Main, which the browse program of the VM disk image
// runs too.
func runRestoreFiles(specJSON, pathsFile string) {
	os.Exit(restoreFilesMain(specJSON, pathsFile, "/dev/termination-log"))
}

func restoreFilesMain(specJSON, pathsFile, terminationLog string) int {
	return filerestore.Main(specJSON, pathsFile, terminationLog)
}
