package ibmcloud

import "fmt"

// dp2Range is one row of IBM's table of allowed IOPS for a dp2 share size.
type dp2Range struct {
	minSizeGB, maxSizeGB int64
	minIOPS, maxIOPS     int64
}

// dp2Ranges is Table 2 of "File Storage for VPC profiles" in the IBM Cloud
// documentation. The VPC API returns only the overall limits, not this table,
// so it is kept here by hand.
var dp2Ranges = []dp2Range{
	{10, 39, 100, 1000},
	{40, 79, 100, 2000},
	{80, 99, 100, 4000},
	{100, 499, 100, 6000},
	{500, 999, 100, 10000},
	{1000, 1999, 100, 20000},
	{2000, 3999, 200, 40000},
	{4000, 7999, 300, 40000},
	{8000, 15999, 500, 64000},
	{16000, 32000, 2000, 96000},
}

// ShareIOPSRange returns the IOPS range IBM allows for a share of the given
// size. ok is false when the profile is not dp2 or the size is outside 10 to
// 32000 GB, which means the table has no answer.
func ShareIOPSRange(profile string, sizeGB int64) (minIOPS, maxIOPS int64, ok bool) {
	if profile != "dp2" {
		return 0, 0, false
	}
	for _, r := range dp2Ranges {
		if sizeGB >= r.minSizeGB && sizeGB <= r.maxSizeGB {
			return r.minIOPS, r.maxIOPS, true
		}
	}
	return 0, 0, false
}

// ValidateShareIOPS returns an error when IBM would refuse the IOPS for a
// share of this size. It returns nil when the table has no answer, so another
// check can report a bad size.
func ValidateShareIOPS(profile string, sizeGB, iops int64) error {
	minIOPS, maxIOPS, ok := ShareIOPSRange(profile, sizeGB)
	if !ok {
		return nil
	}
	if iops < minIOPS || iops > maxIOPS {
		return fmt.Errorf("%d IOPS is not allowed for a %d GB %s share: IBM allows %d to %d IOPS at this size", iops, sizeGB, profile, minIOPS, maxIOPS)
	}
	return nil
}
