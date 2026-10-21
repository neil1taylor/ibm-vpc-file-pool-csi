package pool

import (
	"context"
	"errors"
	"fmt"
	"time"

	"k8s.io/klog/v2"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/metrics"
)

const (
	// snapshotPoolFullPercent is the usage above which a snapshot pool gets no
	// new backups.
	snapshotPoolFullPercent = 85

	// snapshotUsageInterval is how often a Job reads the usage of a snapshot
	// pool's share.
	snapshotUsageInterval = 10 * time.Minute
)

// poolUsage is what the last usage Job of a snapshot pool found.
type poolUsage struct {
	percent   int
	checkedAt time.Time
	// running is true while a usage Job has been started and has not finished.
	running bool
}

// shareFull reads the usage of a share of a snapshot pool now and then, with a
// Job, and says whether the share is too full for new backups. A Job that is
// running, or has failed, leaves the last answer as it was. Backups are never
// deleted to make room.
func (bc *BackupController) shareFull(ctx context.Context, pool string, sh v1alpha1.PoolShareStatus) (int, bool) {
	if bc.shareData == nil {
		return 0, false
	}
	share := shareMountOf(sh)
	now := bc.nowFunc()
	u := bc.usage[sh.ShareID]
	if u == nil {
		u = &poolUsage{}
		bc.usage[sh.ShareID] = u
	}
	if !u.running && now.Sub(u.checkedAt) < snapshotUsageInterval {
		return u.percent, u.percent > snapshotPoolFullPercent
	}
	done, percent, err := bc.shareData.ShareUsage(ctx, "usage-"+sh.ShareID, share)
	switch {
	case err != nil:
		if errors.Is(err, ErrShareJobFailed) {
			klog.ErrorS(err, "Could not read the usage of a snapshot pool share; backups go on as before", "pool", pool, "share", sh.ShareID)
		} else {
			klog.ErrorS(err, "Failed to start or check the usage Job of a snapshot pool share", "pool", pool, "share", sh.ShareID)
		}
		u.running = false
		u.checkedAt = now
	case !done:
		u.running = true
	default:
		u.running, u.percent, u.checkedAt = false, percent, now
		metrics.BackupSnapshotPoolUsedPercent.WithLabelValues(pool, sh.ShareID).Set(float64(percent))
		klog.V(2).InfoS("Read the usage of a snapshot pool share", "pool", pool, "share", sh.ShareID, "usedPercent", percent)
	}
	return u.percent, u.percent > snapshotPoolFullPercent
}

func snapshotPoolFullMessage(pool, shareID string, percent int) string {
	return fmt.Sprintf("share %s of the snapshot pool %q is %d%% full (limit %d%%); new backups to it are stopped until usage falls; backups are not deleted to make room",
		shareID, pool, percent, snapshotPoolFullPercent)
}
