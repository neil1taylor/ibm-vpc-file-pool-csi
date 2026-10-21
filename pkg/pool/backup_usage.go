package pool

import (
	"context"
	"errors"
	"fmt"
	"time"

	"k8s.io/klog/v2"

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

// snapshotPoolFull reads the usage of the snapshot pool's share now and then,
// with a Job, and says whether the pool is too full for new backups. A Job
// that is running, or has failed, leaves the last answer as it was. Backups are
// never deleted to make room.
func (bc *BackupController) snapshotPoolFull(ctx context.Context, pool string, share ShareMount) (int, bool) {
	if bc.shareData == nil {
		return 0, false
	}
	now := bc.nowFunc()
	u := bc.usage[pool]
	if u == nil {
		u = &poolUsage{}
		bc.usage[pool] = u
	}
	if !u.running && now.Sub(u.checkedAt) < snapshotUsageInterval {
		return u.percent, u.percent > snapshotPoolFullPercent
	}
	done, percent, err := bc.shareData.ShareUsage(ctx, "usage-"+pool, share)
	switch {
	case err != nil:
		if errors.Is(err, ErrShareJobFailed) {
			klog.ErrorS(err, "Could not read the usage of a snapshot pool; backups go on as before", "pool", pool)
		} else {
			klog.ErrorS(err, "Failed to start or check the usage Job of a snapshot pool", "pool", pool)
		}
		u.running = false
		u.checkedAt = now
	case !done:
		u.running = true
	default:
		u.running, u.percent, u.checkedAt = false, percent, now
		metrics.BackupSnapshotPoolUsedPercent.WithLabelValues(pool).Set(float64(percent))
		klog.V(2).InfoS("Read the usage of a snapshot pool", "pool", pool, "usedPercent", percent)
	}
	return u.percent, u.percent > snapshotPoolFullPercent
}

func snapshotPoolFullMessage(pool string, percent int) string {
	return fmt.Sprintf("the snapshot pool %q is %d%% full (limit %d%%); new backups are stopped until usage falls; backups are not deleted to make room",
		pool, percent, snapshotPoolFullPercent)
}
