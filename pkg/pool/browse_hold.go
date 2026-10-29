package pool

import (
	"strings"
	"time"
)

// A browse session on a VM disk holds the disk's next backup copy (issue #92,
// phase 5). The helper rebuilds an older view of the disk from the disk and its
// undo files, so the disk and the undo files must stay as they are while the
// session is open. The hold is an annotation on the SubVolume, like the export
// hold, and the replication controller makes no new copy of a held volume.
//
// Two sessions may be open on one volume, so the value lists the sessions:
// "<session UID>/<time the hold began>", separated by commas. A UID is
// made of hex digits and dashes, so neither separator can be part of one.

const (
	// browseHoldAnnotation is set on a SubVolume while a browse session reads
	// its VM disk.
	browseHoldAnnotation = "storage.ibmcloud.io/browse-hold"

	// browseHoldLimit is how old a hold may be and still count. A session lives
	// 2 hours at most (the default hard limit), so an older hold is left over,
	// for example by a controller that stopped. The replication controller then
	// ignores it, so a hold can never stop backups for good.
	browseHoldLimit = 3 * time.Hour

	// guestHoldBudget is the most time a file restore may keep a VM disk held,
	// counted from the first hold. It ends 15 minutes before browseHoldLimit, so
	// no Job of the restore can still be reading when the sweep lets the hold go.
	guestHoldBudget = browseHoldLimit - 15*time.Minute

	// guestMinRun is the least time left that is worth starting a Job for.
	guestMinRun = time.Minute

	// heldByBrowseMessage is the status of a volume that waits for a browse
	// session to end. It is not an error.
	heldByBrowseMessage = "held: waiting for a browse session"

	// browseHoldSkew is how far into the future a hold's time may be.
	browseHoldSkew = 5 * time.Minute
)

type browseHold struct {
	session string
	since   time.Time
}

// parseBrowseHolds reads the value of the hold annotation. Entries that do not
// parse are left out.
func parseBrowseHolds(value string) []browseHold {
	var holds []browseHold
	for _, part := range strings.Split(value, ",") {
		session, stamp, found := strings.Cut(part, "/")
		if !found || session == "" {
			continue
		}
		since, err := time.Parse(time.RFC3339, stamp)
		if err != nil {
			continue
		}
		holds = append(holds, browseHold{session: session, since: since})
	}
	return holds
}

func formatBrowseHolds(holds []browseHold) string {
	parts := make([]string, 0, len(holds))
	for _, h := range holds {
		parts = append(parts, h.session+"/"+h.since.UTC().Format(time.RFC3339))
	}
	return strings.Join(parts, ",")
}

// browseHeld reports whether the annotation value holds the volume at now: at
// least one hold is not older than the limit and not from the future.
func browseHeld(value string, now time.Time) bool {
	for _, h := range parseBrowseHolds(value) {
		if !h.since.After(now.Add(browseHoldSkew)) && now.Sub(h.since) < browseHoldLimit {
			return true
		}
	}
	return false
}

// addBrowseHold adds a session to the value. A session that is there already
// keeps its first time.
func addBrowseHold(value, session string, at time.Time) string {
	holds := parseBrowseHolds(value)
	for _, h := range holds {
		if h.session == session {
			return formatBrowseHolds(holds)
		}
	}
	return formatBrowseHolds(append(holds, browseHold{session: session, since: at}))
}

// removeBrowseHold takes a session out of the value.
func removeBrowseHold(value, session string) string {
	var keep []browseHold
	for _, h := range parseBrowseHolds(value) {
		if h.session != session {
			keep = append(keep, h)
		}
	}
	return formatBrowseHolds(keep)
}
