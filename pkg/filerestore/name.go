package filerestore

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

const (
	maxFolderName = 200
	momentStamp   = "20060102T150405Z"
	// stagingPrefix starts the name of the hidden folder a restore into an
	// existing claim is written to, before it gets its final name.
	stagingPrefix = ".restoring-"
)

// clean keeps letters, digits and dashes, lower case; every other character,
// dots included, becomes a dash. A policy name is a Kubernetes name, but the folder name is
// built from text that is checked elsewhere, so it is made safe here too.
func clean(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// FolderName is the name of the folder that a restore into an existing claim
// makes: restored-<policy>-<moment>, or "latest" without a moment. A policy
// name can be 253 characters long, so a long name is cut and a hash of the
// whole name keeps two policies apart.
func FolderName(policy string, moment time.Time) string {
	stamp := "latest"
	if !moment.IsZero() {
		stamp = moment.UTC().Format(momentStamp)
	}
	p := clean(policy)
	name := "restored-" + p + "-" + stamp
	if len(name) <= maxFolderName {
		return name
	}
	sum := sha256.Sum256([]byte(policy))
	tail := "-" + hex.EncodeToString(sum[:])[:8] + "-" + stamp
	keep := maxFolderName - len("restored-") - len(tail)
	return "restored-" + p[:keep] + tail
}

// StagingName is the hidden folder a restore writes to first. It is named by
// the ID of the restore, so a retry finds and replaces its own earlier try and
// nothing else.
func StagingName(id string) string {
	return stagingPrefix + clean(id)
}
