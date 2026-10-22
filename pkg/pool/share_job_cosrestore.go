package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/util"
)

// Restore from COS (docs/specs/2026-10-21-backup-cos-export.md, part 5).
//
// A restore from COS runs in three steps, each a Job. The first opens the Kopia
// repository read-only and restores one export (the latest copy of the volume
// and its earlier moments) into a scratch folder on the share of the new
// volume. The second is the restore Job of a local restore (RestoreBackup),
// which reads the scratch folder as if it were a backup and writes the new
// volume. The third removes the scratch folder. So one restore path is tested
// for both kinds of backup, and COS is read once.

const (
	// cosScratchPrefix starts the name of every scratch folder. The cleanup Job
	// removes only folders with this start.
	cosScratchPrefix = "cosrestore-"

	// cosListLimit is the most the list Job puts in its termination message,
	// which Kubernetes cuts at 4096 bytes.
	cosListLimit = 3500
)

var (
	cosScratchPattern = regexp.MustCompile(`^` + cosScratchPrefix + `[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
	cosVolumePattern  = regexp.MustCompile(`^pvc-[a-f0-9-]{36}$`)
)

// COSScratchName returns the name of the scratch folder of a restore. The
// folder is /backup/<name> on the share of the new volume, and the name follows
// the rules of a backup policy's folder.
func COSScratchName(restore string) string {
	name := cosScratchPrefix + restore
	if len(name) > 63 {
		name = name[:63]
	}
	return strings.TrimRight(name, "-.")
}

// COSScratchPaths returns the backup folder and the moments folder of a
// volume inside a scratch folder. They have the shape of a local backup, so
// RestoreBackup reads them.
func COSScratchPaths(scratch, volume string) (backup, moments string) {
	return backupScratchBase + scratch + "/pvcs/" + volume, backupScratchBase + scratch + "/.moments/pvcs/" + volume
}

const backupScratchBase = "/backup/"

func validateScratchPaths(backup, moments string) error {
	if err := util.ValidateBackupPath(backup); err != nil {
		return err
	}
	return util.ValidateBackupMomentsPath(moments)
}

// COSRestoreRequest says which export to fetch and where to put it.
type COSRestoreRequest struct {
	// SourceVolume is the volume that was exported (pvc- and a UUID).
	SourceVolume string
	// TargetVolume is the new volume. It names the scratch copy so that the
	// restore Job's path rules hold.
	TargetVolume string
	// Scratch is the scratch folder name (COSScratchName).
	Scratch string
	// Moment is the time to restore. The oldest export made at or after it is
	// used, or the newest export if there is none. The zero time means the
	// newest export.
	Moment time.Time
}

func (q COSRestoreRequest) validate() error {
	if !cosVolumePattern.MatchString(q.SourceVolume) {
		return fmt.Errorf("source volume %q is not a volume name", q.SourceVolume)
	}
	if !cosVolumePattern.MatchString(q.TargetVolume) {
		return fmt.Errorf("target volume %q is not a volume name", q.TargetVolume)
	}
	if !cosScratchPattern.MatchString(q.Scratch) || len(q.Scratch) > 63 || strings.Contains(q.Scratch, "..") {
		return fmt.Errorf("scratch folder %q is not a restore scratch folder", q.Scratch)
	}
	return nil
}

// COSRestoreOutcome says which export the first step fetched.
type COSRestoreOutcome struct {
	// Moment is the moment of that export. It is the zero time if not known.
	Moment time.Time
}

// cosRestoreParams is everything the restore script depends on.
type cosRestoreParams struct {
	exportParams
	req COSRestoreRequest
}

// snapshotLines turns the Kopia snapshot list into lines of
// "<id> <path> <moment> <volume>". Snapshots that the export did not make (no
// moment and volume tags) are left out.
const snapshotLines = `kopia snapshot list --all --json | sed -n 's/^ *{"id":"\([^"]*\)".*"path":"\([^"]*\)".*"tag:moment":"\([^"]*\)","tag:volume":"\([^"]*\)".*/\1 \2 \3 \4/p'`

// pickAwk chooses the export to restore. Its input is snapshotLines. Of the
// snapshots of the volume's backup folder it takes the oldest whose moment is at
// or after want, or the newest if none is (and if want is empty). It prints
// "<moment> <backup id> <moments id>"; the last field is empty if that export
// had no moments folder. Times are RFC 3339 in UTC, which sort as text.
const pickAwk = `$4 != vol { next }
$2 ~ /\/\.moments\// { mom[$3] = $1; next }
{ back[$3] = $1; if ($3 > newest) newest = $3 }
END {
  best = ""
  for (m in back) { if (want != "" && m >= want && (best == "" || m < best)) best = m }
  if (best == "") best = newest
  if (best != "") print best, back[best], (best in mom ? mom[best] : "-")
}`

// cosRestoreScript fetches one export into the scratch folder. It connects
// read-only, so it cannot change the repository. If it fails it removes its
// scratch folder.
func cosRestoreScript(p cosRestoreParams) string {
	var b strings.Builder
	p.scriptHead(&b)
	want := ""
	if !p.req.Moment.IsZero() {
		want = p.req.Moment.UTC().Format(time.RFC3339)
	}
	scratchBackup, scratchMoments := COSScratchPaths(p.req.Scratch, p.req.TargetVolume)
	fmt.Fprintf(&b, "kopia repository connect %s $ident --readonly\n", p.backendWords())
	fmt.Fprintf(&b, "vol=%s\nwant=%s\nscratch=%s\n", util.ShellQuote(p.req.SourceVolume), util.ShellQuote(want),
		util.ShellQuote(p.rootDir()+backupScratchBase+p.req.Scratch))
	fmt.Fprintf(&b, "pick=$(%s | awk -v vol=\"$vol\" -v want=\"$want\" %s)\n", snapshotLines, util.ShellQuote(pickAwk))
	b.WriteString(`if [ -z "$pick" ]; then
  echo "no backup of $vol in the repository" >&2
  exit 1
fi
set -- $pick
moment=$1
back=$2
moms=$3
trap 'rm -r -f -- "$scratch"' EXIT
rm -r -f -- "$scratch"
`)
	fmt.Fprintf(&b, "mkdir -p %s %s\n", util.ShellQuote(p.rootDir()+scratchBackup), util.ShellQuote(p.rootDir()+scratchMoments))
	fmt.Fprintf(&b, "kopia snapshot restore \"$back\" %s --skip-owners --write-sparse-files\n", util.ShellQuote(p.rootDir()+scratchBackup))
	fmt.Fprintf(&b, "if [ \"$moms\" != - ]; then kopia snapshot restore \"$moms\" %s --skip-owners; fi\n", util.ShellQuote(p.rootDir()+scratchMoments))
	// A tenant can have put symlinks in the backup. None may stay in the copy.
	b.WriteString("find \"$scratch\" -type l -delete\n")
	fmt.Fprintf(&b, "trap - EXIT\nprintf '{\"moment\":\"%%s\"}' \"$moment\" > %s\n", util.ShellQuote(p.termFile()))
	return b.String()
}

// FetchFromCOS runs the first step of a restore from COS: a Job that restores
// one export from the repository into the scratch folder on dst.
func (r *ShareJobRunner) FetchFromCOS(ctx context.Context, key string, dst ShareMount, repo ExportRepo, req COSRestoreRequest) (bool, COSRestoreOutcome, error) {
	if err := req.validate(); err != nil {
		return false, COSRestoreOutcome{}, err
	}
	if err := repo.validateRead(); err != nil {
		return false, COSRestoreOutcome{}, err
	}
	if r.driverImage == "" {
		return false, COSRestoreOutcome{}, errors.New("no driver image set for COS restore Jobs")
	}
	script := cosRestoreScript(cosRestoreParams{exportParams: exportParams{repo: repo, useKeyProtect: true}, req: req})
	shape := r.kopiaShape(repo, script, false)
	done, msg, err := r.runMessage(ctx, key, []ShareMount{dst}, shape)
	if err != nil || !done {
		return false, COSRestoreOutcome{}, err
	}
	return true, parseCOSRestoreOutcome(msg), nil
}

// RemoveCOSScratch removes the scratch folder of a restore from share. It
// refuses any other folder.
func (r *ShareJobRunner) RemoveCOSScratch(ctx context.Context, key string, share ShareMount, scratch string) (bool, error) {
	if !cosScratchPattern.MatchString(scratch) || len(scratch) > 63 || strings.Contains(scratch, "..") {
		return false, fmt.Errorf("%q is not a restore scratch folder", scratch)
	}
	script := fmt.Sprintf("set -eu\nrm -r -f -- \"/share0%s%s\"\n", backupScratchBase, scratch)
	return r.run(ctx, key, []ShareMount{share}, r.scriptShape(script))
}

func parseCOSRestoreOutcome(msg string) COSRestoreOutcome {
	var m struct {
		Moment string `json:"moment"`
	}
	if err := json.Unmarshal([]byte(msg), &m); err != nil {
		return COSRestoreOutcome{}
	}
	t, err := time.Parse(time.RFC3339, m.Moment)
	if err != nil {
		return COSRestoreOutcome{}
	}
	return COSRestoreOutcome{Moment: t}
}

// COSVolume is one volume in the repository and the moments of its exports.
type COSVolume struct {
	Name    string
	Moments []time.Time
}

// COSListing is what a list Job reports.
type COSListing struct {
	Volumes []COSVolume
	// Truncated is true if there were more moments than the Job could report.
	// Ask for one volume to see all of its moments.
	Truncated bool
}

// listAwk prints the listing as JSON from snapshotLines. It stops adding
// moments when the answer would not fit the termination message.
const listAwk = `$2 ~ /\/\.moments\// { next }
vol != "" && $4 != vol { next }
{
  if (!($4 in seen)) { seen[$4] = 1; order[++n] = $4 }
  if (!(($4, $3) in has)) { has[$4, $3] = 1; ms[$4] = ms[$4] " " $3 }
}
END {
  out = "{\"volumes\":["; trunc = "false"; first = 1
  for (i = 1; i <= n && trunc == "false"; i++) {
    v = order[i]; k = split(ms[v], arr, " ")
    item = "{\"volume\":\"" v "\",\"moments\":["
    for (j = 1; j <= k; j++) {
      piece = (j > 1 ? "," : "") "\"" arr[j] "\""
      if (length(out) + length(item) + length(piece) + 40 > limit) { trunc = "true"; break }
      item = item piece
    }
    out = out (first ? "" : ",") item "]}"; first = 0
  }
  print out "],\"truncated\":" trunc "}"
}`

// cosListScript lists the volumes in the repository, with the moment of each
// export, oldest first.
func cosListScript(p exportParams, volume string) string {
	var b strings.Builder
	p.scriptHead(&b)
	fmt.Fprintf(&b, "kopia repository connect %s $ident --readonly\n", p.backendWords())
	fmt.Fprintf(&b, "%s | awk -v vol=%s -v limit=%d %s > %s\n", snapshotLines, util.ShellQuote(volume), cosListLimit,
		util.ShellQuote(listAwk), util.ShellQuote(p.termFile()))
	return b.String()
}

// ListCOS runs a Job that lists the volumes and the export moments in the
// repository. volume, if set, limits it to one volume.
func (r *ShareJobRunner) ListCOS(ctx context.Context, key string, repo ExportRepo, volume string) (bool, COSListing, error) {
	if volume != "" && !cosVolumePattern.MatchString(volume) {
		return false, COSListing{}, fmt.Errorf("volume %q is not a volume name", volume)
	}
	if err := repo.validateRead(); err != nil {
		return false, COSListing{}, err
	}
	if r.driverImage == "" {
		return false, COSListing{}, errors.New("no driver image set for COS list Jobs")
	}
	script := cosListScript(exportParams{repo: repo, useKeyProtect: true}, volume)
	done, msg, err := r.runMessage(ctx, key, nil, r.kopiaShape(repo, script, false))
	if err != nil || !done {
		return false, COSListing{}, err
	}
	return true, parseCOSListing(msg), nil
}

func parseCOSListing(msg string) COSListing {
	var m struct {
		Volumes []struct {
			Volume  string   `json:"volume"`
			Moments []string `json:"moments"`
		} `json:"volumes"`
		Truncated bool `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(msg), &m); err != nil {
		return COSListing{}
	}
	out := COSListing{Truncated: m.Truncated}
	for _, v := range m.Volumes {
		cv := COSVolume{Name: v.Volume}
		for _, s := range v.Moments {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				cv.Moments = append(cv.Moments, t)
			}
		}
		out.Volumes = append(out.Volumes, cv)
	}
	return out
}
