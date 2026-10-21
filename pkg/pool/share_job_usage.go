package pool

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// ShareUsage reads how full the share is, as a whole percent, with df in a Job.
// The controller cannot see this from custom resources: a pool counts only the
// claims, not the backups. The Job only reads.
func (r *ShareJobRunner) ShareUsage(ctx context.Context, key string, share ShareMount) (bool, int, error) {
	script := "set -eu\ndf -P /share0 | tail -n 1 > /dev/termination-log\n"
	done, msg, err := r.runMessage(ctx, key, []ShareMount{share}, r.scriptShape(script))
	if err != nil || !done {
		return false, 0, err
	}
	pct := parseDfPercent(msg)
	if pct < 0 {
		return false, 0, fmt.Errorf("share usage Job %s left no readable df line: %q", key, msg)
	}
	return true, pct, nil
}

// parseDfPercent reads the Use% column of the last line of df -P output. It
// returns -1 when there is none.
func parseDfPercent(msg string) int {
	fields := strings.Fields(msg)
	if len(fields) < 5 {
		return -1
	}
	pct, err := strconv.Atoi(strings.TrimSuffix(fields[4], "%"))
	if err != nil || !strings.HasSuffix(fields[4], "%") {
		return -1
	}
	return pct
}
