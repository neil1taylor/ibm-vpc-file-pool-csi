package cosdisk

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/replication"
)

// A backup of a VM disk keeps one full copy for each restore point, in a folder
// named by the time of the restore point. The export snapshots those folders, so
// the Kopia WebDAV server of that snapshot serves them: the root lists the
// folders, and /<folder>/disk.img is the disk at that restore point. See
// docs/specs/2026-10-29-vm-disks-in-cos.md, section "Phase 6d".

// RestorePointFolderLayout is how a folder of the moments snapshot is named.
const RestorePointFolderLayout = "20060102T150405Z"

// maxFolders is the most names in a folder listing.
const maxFolders = 100000

// ErrNoRestorePoint means no restore point of the disk is at or before the time
// that was asked for. It is the error of the undo reader too.
var ErrNoRestorePoint = replication.ErrNoRestorePoint

var folderNameRE = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z$`)

// RestorePointFolder is one restore point of the moments snapshot.
type RestorePointFolder struct {
	// Name is the folder name. It has passed ParseRestorePointFolder, so it is
	// safe to put in a path.
	Name   string
	Moment time.Time
}

// ParseRestorePointFolder reads a folder name such as 20261106T180326Z. Anything
// else, including a name with a slash or dots, is refused: the names come from
// a backup that is not trusted.
func ParseRestorePointFolder(name string) (time.Time, bool) {
	if !folderNameRE.MatchString(name) {
		return time.Time{}, false
	}
	t, err := time.Parse(RestorePointFolderLayout, name)
	if err != nil || t.Format(RestorePointFolderLayout) != name {
		return time.Time{}, false
	}
	return t, true
}

// ListRestorePointFolders lists the restore point folders at the root of the
// Kopia WebDAV server that serves serverURL (any URL on it), oldest first, with
// one PROPFIND. Like HTTPSource it talks only to 127.0.0.1. A snapshot that has
// no root to list (Kopia answers 405 or 404) has no folders; that is an empty
// list, not an error.
func ListRestorePointFolders(ctx context.Context, client *http.Client, serverURL string) ([]RestorePointFolder, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return nil, fmt.Errorf("cosdisk: bad URL: %w", err)
	}
	if u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil {
		return nil, fmt.Errorf("cosdisk: the disk must be on http://127.0.0.1, got %q", u.Redacted())
	}
	body := `<?xml version="1.0" encoding="utf-8"?><D:propfind xmlns:D="DAV:"><D:prop><D:resourcetype/></D:prop></D:propfind>`
	req, err := http.NewRequestWithContext(ctx, "PROPFIND", u.Scheme+"://"+u.Host+"/", strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Depth", "1")
	req.Header.Set("Content-Type", "application/xml")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cosdisk: listing the restore points: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return nil, nil
	}
	if resp.StatusCode != http.StatusMultiStatus {
		return nil, fmt.Errorf("cosdisk: listing the restore points: %s", resp.Status)
	}
	var ms multistatus
	if err = xml.NewDecoder(io.LimitReader(resp.Body, maxListBytes)).Decode(&ms); err != nil {
		return nil, fmt.Errorf("cosdisk: the list of restore points is not valid: %w", err)
	}
	if len(ms.Responses) > maxFolders {
		return nil, errors.New("cosdisk: the backup holds too many restore point folders")
	}
	var out []RestorePointFolder
	for _, r := range ms.Responses {
		hu, perr := url.Parse(r.Href)
		if perr != nil {
			continue
		}
		name := strings.Trim(hu.Path, "/")
		if m, ok := ParseRestorePointFolder(name); ok {
			out = append(out, RestorePointFolder{Name: name, Moment: m})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Moment.Before(out[j].Moment) })
	return out, nil
}

// PickRestorePointFolder chooses the newest folder at or before at. folders are
// sorted oldest first, as ListRestorePointFolders gives them. It fails with
// ErrNoRestorePoint if every folder is later.
func PickRestorePointFolder(folders []RestorePointFolder, at time.Time) (RestorePointFolder, error) {
	for i := len(folders) - 1; i >= 0; i-- {
		if !folders[i].Moment.After(at) {
			return folders[i], nil
		}
	}
	if len(folders) == 0 {
		return RestorePointFolder{}, ErrNoRestorePoint
	}
	return RestorePointFolder{}, fmt.Errorf("%w: the oldest restore point is %s", ErrNoRestorePoint, folders[0].Moment.UTC().Format(time.RFC3339))
}
