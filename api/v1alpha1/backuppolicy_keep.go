package v1alpha1

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// DefaultBackupKeepFor is how long backups are kept when a policy sets nothing.
const DefaultBackupKeepFor = 7 * 24 * time.Hour

// ParseKeepFor reads a keepFor value: a Go duration such as "36h", or a whole
// number of days such as "30d". An empty value is the default of 7 days.
func ParseKeepFor(s string) (time.Duration, error) {
	if s == "" {
		return DefaultBackupKeepFor, nil
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return 0, fmt.Errorf("%q is not a whole number of days", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration such as 36h or 30d", s)
	}
	return d, nil
}
