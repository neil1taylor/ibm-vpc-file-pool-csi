package main

import (
	"encoding/json"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/replication"
	"k8s.io/klog/v2"
)

// parseRestoreMoment reads the --restore-moment flag: an RFC 3339 time, or
// empty for the latest backup.
func parseRestoreMoment(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("--restore-moment: %w", err)
	}
	return t, nil
}

// restoreVMDiskMessage is the termination message of a restore-vm-disk Job.
func restoreVMDiskMessage(res replication.RestoreResult) string {
	msg := struct {
		Moment string `json:"moment,omitempty"`
		Latest bool   `json:"latest"`
	}{Latest: res.Latest}
	if !res.Moment.IsZero() {
		msg.Moment = res.Moment.UTC().Format(time.RFC3339)
	}
	b, _ := json.Marshal(msg)
	return string(b)
}

// runRestoreVMDisk writes a backed-up VM disk, as it was at a moment, into a
// new volume. A restore-backup Job runs it.
func runRestoreVMDisk(backupDisk, outDisk, moment string) {
	if backupDisk == "" || outDisk == "" {
		klog.ErrorS(nil, "backup-disk and out-disk are required for restore-vm-disk mode")
		os.Exit(1)
	}
	at, err := parseRestoreMoment(moment)
	if err != nil {
		klog.ErrorS(err, "Invalid restore-vm-disk flags")
		os.Exit(1)
	}
	// QEMU runs as 107, and the backup disk belongs to it. Root cannot read or
	// change it on NFS (root_squash), so run as that user: the new disk is then
	// made by 107 and needs no wider permissions.
	if err = dropToQEMUUser(); err != nil {
		klog.ErrorS(err, "Failed to run as the QEMU user")
		os.Exit(1)
	}
	res, err := replication.RestoreDisk(backupDisk, at, outDisk)
	if err != nil {
		klog.ErrorS(err, "Failed to restore the VM disk", "backup", backupDisk, "out", outDisk)
		os.Exit(1)
	}
	klog.InfoS("Restored the VM disk", "out", outDisk, "moment", res.Moment, "latest", res.Latest)
	// The restore controller reads the result from the termination log.
	_ = os.WriteFile("/dev/termination-log", []byte(restoreVMDiskMessage(res)), 0o600)
}

// qemuUser is the user QEMU runs as in a virt-launcher pod.
const qemuUser = 107

// dropToQEMUUser makes the process run as the QEMU user. The group goes first,
// because after the user changes the process may not change its group.
func dropToQEMUUser() error {
	if os.Getuid() == qemuUser && os.Getgid() == qemuUser {
		return nil
	}
	if err := syscall.Setgid(qemuUser); err != nil {
		return fmt.Errorf("setgid %d: %w", qemuUser, err)
	}
	if err := syscall.Setuid(qemuUser); err != nil {
		return fmt.Errorf("setuid %d: %w", qemuUser, err)
	}
	return nil
}
