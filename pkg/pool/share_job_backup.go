package pool

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/util"
)

// RemoveBackup and MeasureBackup are on ShareJobRunner but not in ShareData on
// purpose: only the kubectl backup plugin, after a person has confirmed, may
// call them. The controller cannot.

// checkBackupFolders accepts the backup folder of one volume and the folder of
// its earlier moments, and nothing else: both must pass the backup path checks
// and belong to the same policy and the same volume.
func checkBackupFolders(backupPath, momentsPath string) error {
	if err := util.ValidateBackupPath(backupPath); err != nil {
		return err
	}
	if err := util.ValidateBackupMomentsPath(momentsPath); err != nil {
		return err
	}
	policyDir, volume, _ := strings.Cut(strings.TrimPrefix(backupPath, backupBasePath+"/"), "/pvcs/")
	if want := backupBasePath + "/" + policyDir + "/.moments/pvcs/" + volume; momentsPath != want {
		return fmt.Errorf("moments folder %q is not the one of backup folder %q", momentsPath, backupPath)
	}
	return nil
}

// RemoveBackup removes the backup folder and the moments folder of one volume
// from the snapshot pool's share.
func (r *ShareJobRunner) RemoveBackup(ctx context.Context, key string, share ShareMount, backupPath, momentsPath string) (bool, error) {
	if err := checkBackupFolders(backupPath, momentsPath); err != nil {
		return false, err
	}
	script := fmt.Sprintf("set -eu\nrm -r -f -- \"/share0%s\"\nrm -r -f -- \"/share0%s\"\n", backupPath, momentsPath)
	return r.run(ctx, key, []ShareMount{share}, r.scriptShape(script))
}

// MeasureBackup reports how many bytes the backup folder and the moments
// folder of one volume use, without changing anything. A folder that does not
// exist counts as 0.
func (r *ShareJobRunner) MeasureBackup(ctx context.Context, key string, share ShareMount, backupPath, momentsPath string) (bool, int64, error) {
	if err := checkBackupFolders(backupPath, momentsPath); err != nil {
		return false, 0, err
	}
	// One du run over both folders: each moment of a plain volume is a folder
	// of hard links to the backup folder's files, and a single run counts each
	// file once.
	script := fmt.Sprintf(`set -eu
set --
for d in "/share0%s" "/share0%s"; do
  if [ -d "$d" ]; then
    set -- "$@" "$d"
  fi
done
total=0
if [ "$#" -gt 0 ]; then
  for kb in $(du -sk "$@" | cut -f1); do
    total=$((total + kb))
  done
fi
echo "$total" > /dev/termination-log
`, backupPath, momentsPath)
	done, msg, err := r.runMessage(ctx, key, []ShareMount{share}, r.scriptShape(script))
	if err != nil || !done {
		return false, 0, err
	}
	kb, perr := strconv.ParseInt(strings.TrimSpace(msg), 10, 64)
	if perr != nil || kb < 0 {
		return false, 0, fmt.Errorf("the measuring Job %s did not report a size (got %q)", key, msg)
	}
	return true, kb * 1024, nil
}
