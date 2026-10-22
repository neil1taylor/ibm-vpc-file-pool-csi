package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/util"
)

// RestoreOutcome says which moment of a backup a restore wrote.
type RestoreOutcome struct {
	// Moment is the moment written. It is the zero time if it is not known.
	Moment time.Time
	// Latest is true if the newest backup was written.
	Latest bool
}

// RestoreBackup runs a Job that copies the backup of one volume into a volume
// folder. The Job mounts the backup share and the data share. A VM disk is
// written by the driver's restore-vm-disk mode, which rewinds it to the moment;
// any other volume is copied from the nearest earlier moment folder, or from
// the latest copy.
func (r *ShareJobRunner) RestoreBackup(ctx context.Context, key string, backup ShareMount, volPath, momentsPath string,
	dst ShareMount, dstPath string, moment time.Time, owner *Ownership) (bool, RestoreOutcome, error) {
	if err := util.ValidateBackupPath(volPath); err != nil {
		return false, RestoreOutcome{}, err
	}
	if err := util.ValidateBackupMomentsPath(momentsPath); err != nil {
		return false, RestoreOutcome{}, err
	}
	if err := util.ValidateSubDir(dstPath); err != nil {
		return false, RestoreOutcome{}, fmt.Errorf("the restore target: %w", err)
	}
	if owner != nil && owner.Permissions != "" && !validPermissionsPattern.MatchString(owner.Permissions) {
		return false, RestoreOutcome{}, fmt.Errorf("invalid permissions %q", owner.Permissions)
	}
	if r.driverImage == "" {
		return false, RestoreOutcome{}, errors.New("no driver image set for backup restore Jobs")
	}
	at, momentFlag := "", ""
	if !moment.IsZero() {
		at = moment.UTC().Format(time.RFC3339)
		momentFlag = " --restore-moment=" + at
	}
	// A restore from COS reads its scratch copy from the share it writes to.
	shares, dstRoot := []ShareMount{backup, dst}, "/share1"
	if backup == dst {
		shares, dstRoot = []ShareMount{backup}, "/share0"
	}
	var b strings.Builder
	fmt.Fprintf(&b, `set -eu
src="/share0%s"
moments="/share0%s"
dst="%s%s"
at="%s"
if [ -L "$src" ] || [ -L "$src/disk.img" ]; then
  echo "refusing a backup folder or disk that is a symlink" >&2
  exit 1
fi
if [ -f "$src/disk.img" ]; then
  # A retry finds the disk of the first try; the target is a new volume, so
  # this removes only our own earlier output.
  rm -f "$dst/disk.img"
  # The new volume has no folder until a pod mounts it. The disk restore runs
  # as the QEMU user, so it needs to be able to write there.
  mkdir -p "$dst"
  chmod 0777 "$dst"
  exec /usr/local/bin/vpc-file-pool-csi --mode=restore-vm-disk --backup-disk="$src/disk.img" --out-disk="$dst/disk.img"%s
fi
pick="$src"
latest=true
newest=""
for d in "$moments"/*; do
  [ -d "$d" ] && newest=$(basename "$d")
done
if [ -n "$at" ]; then
  want=$(date -u -d "$at" +%%Y%%m%%dT%%H%%M%%SZ)
  pick=""
  for d in "$moments"/*; do
    [ -d "$d" ] || continue
    if [[ ! "$(basename "$d")" > "$want" ]]; then pick="$d"; fi
  done
  if [ -z "$newest" ]; then
    # no moment folder yet: the only copy is the latest one
    echo "no moment folder yet; using the latest copy" >&2
    pick="$src"
    stamp=""
  else
    [ -n "$pick" ] || { echo "no backup at or before $at" >&2; exit 1; }
    if [ "$(basename "$pick")" != "$newest" ]; then latest=false; fi
    stamp=$(basename "$pick")
  fi
else
  stamp="$newest"
fi
if [ -L "$pick" ]; then
  echo "refusing a moment folder that is a symlink" >&2
  exit 1
fi
rm -r -f -- "$dst"
mkdir -p "$dst"
cp -a "$pick/." "$dst/"
rm -f "$dst/.subvolume-metadata.json"
`, volPath, momentsPath, dstRoot, dstPath, at, momentFlag)
	writeOwnerScript(&b, "$dst", owner)
	b.WriteString(`iso=""
if [ -n "$stamp" ]; then
  iso=$(date -u -d "${stamp:0:4}-${stamp:4:2}-${stamp:6:2} ${stamp:9:2}:${stamp:11:2}:${stamp:13:2}" +%Y-%m-%dT%H:%M:%SZ)
fi
printf '{"moment":"%s","latest":%s}' "$iso" "$latest" > /dev/termination-log
`)
	shape := jobShape{
		image:     r.driverImage,
		command:   []string{"/bin/bash", "-c", b.String()},
		runAsUser: 0,
	}
	done, msg, err := r.runMessage(ctx, key, shares, shape)
	if err != nil || !done {
		return false, RestoreOutcome{}, err
	}
	return true, parseRestoreOutcome(msg), nil
}

// parseRestoreOutcome reads the termination message of a restore Job. A
// message that is missing or cannot be read gives an outcome that is not known.
func parseRestoreOutcome(msg string) RestoreOutcome {
	var m struct {
		Moment string `json:"moment"`
		Latest bool   `json:"latest"`
	}
	if err := json.Unmarshal([]byte(msg), &m); err != nil {
		return RestoreOutcome{}
	}
	out := RestoreOutcome{Latest: m.Latest}
	if t, err := time.Parse(time.RFC3339, m.Moment); err == nil {
		out.Moment = t
	}
	return out
}
