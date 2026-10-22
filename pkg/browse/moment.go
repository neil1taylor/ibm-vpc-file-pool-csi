package browse

import (
	"fmt"
	"sort"
	"time"
)

// MomentLayout is how a moment folder is named: 20261022T091500Z.
const MomentLayout = "20060102T150405Z"

// PickMoment chooses the moment folder to read from the names in the moments
// folder. With a zero at it takes the newest. Otherwise it takes the newest
// that is not after at. Names that are not moments are ignored. newest says
// whether the choice is the newest moment there is. With no moment folders it
// returns "" and no error: the only copy is the latest one.
func PickMoment(names []string, at time.Time) (pick string, newest bool, err error) {
	var moments []string
	for _, n := range names {
		if _, perr := time.Parse(MomentLayout, n); perr == nil {
			moments = append(moments, n)
		}
	}
	if len(moments) == 0 {
		return "", false, nil
	}
	sort.Strings(moments)
	newestName := moments[len(moments)-1]
	if at.IsZero() {
		return newestName, true, nil
	}
	want := at.UTC().Format(MomentLayout)
	for i := len(moments) - 1; i >= 0; i-- {
		if moments[i] <= want {
			return moments[i], moments[i] == newestName, nil
		}
	}
	return "", false, fmt.Errorf("no backup at or before %s", at.UTC().Format(time.RFC3339))
}
