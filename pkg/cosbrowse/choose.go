package cosbrowse

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
)

// SourceRoot is where the export Jobs mount the snapshot pool's share. Every
// snapshot the export makes has a source path below it.
const SourceRoot = "/share0"

// Params says which backup to open.
type Params struct {
	// Policy and Volume say whose backup. A snapshot is used only if its tags and
	// its source path both say so.
	Policy string
	Volume string
	// At is the time to open: the newest backup at or before it. The zero time
	// opens the latest copy.
	At time.Time
	// SourceRoot is where the export saw the share. Empty means SourceRoot.
	SourceRoot string
	Tree       TreeOptions
}

// export is one run of the export Job: a snapshot of the volume's backup
// folder (the latest copy at that time) and, if the volume has earlier moments,
// a snapshot of its moments folder.
type export struct {
	moment  time.Time
	backup  Snapshot
	moments *Snapshot
}

// exportsOf picks the snapshots that belong to this volume of this policy, and
// groups them by export, oldest first. Both the tags and the exact source path
// must say it: the credentials of the helper can read the whole area of the
// policy, so a snapshot of another volume is told apart here and nowhere else.
// A snapshot with a path that is not exactly one of the two folders of the
// volume is ignored, whatever its tags say.
func exportsOf(snaps []Snapshot, p Params) []export {
	root := p.SourceRoot
	if root == "" {
		root = SourceRoot
	}
	backupPath := root + "/backup/" + p.Policy + "/pvcs/" + p.Volume
	momentsPath := root + "/backup/" + p.Policy + "/.moments/pvcs/" + p.Volume
	byMoment := map[time.Time]*export{}
	for _, s := range snaps {
		if s.Volume != p.Volume || s.Moment.IsZero() || !ValidOID(s.Root) {
			continue
		}
		if s.SourcePath != backupPath && s.SourcePath != momentsPath {
			continue
		}
		e := byMoment[s.Moment]
		if e == nil {
			e = &export{moment: s.Moment}
			byMoment[s.Moment] = e
		}
		s := s
		if s.SourcePath == backupPath {
			if e.backup.ID == "" || s.Start.After(e.backup.Start) {
				e.backup = s
			}
		} else if e.moments == nil || s.Start.After(e.moments.Start) {
			e.moments = &s
		}
	}
	var out []export
	for _, e := range byMoment {
		// An export without the backup folder has nothing to read: the moments
		// folder alone is not a volume.
		if e.backup.ID != "" {
			out = append(out, *e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].moment.Before(out[j].moment) })
	return out
}

// exportFor chooses the export to read, by the rule of a restore from COS: the
// oldest export made at or after the time (it holds the earlier moments too),
// or the newest export if none is. The zero time means the newest.
func exportFor(exports []export, at time.Time) int {
	if !at.IsZero() {
		for i, e := range exports {
			if !e.moment.Before(at) {
				return i
			}
		}
	}
	return len(exports) - 1
}

// Opened is a backup opened for browsing.
type Opened struct {
	Tree *Tree
	// Info says which moment is read.
	Info browse.Info
	// Moments lists the moments in COS for the volume.
	Moments func() ([]string, error)
}

// Close stops the reads that are running.
func (o *Opened) Close() error { return o.Tree.Close() }

// OpenRepo opens the backup of one volume in repo.
func OpenRepo(ctx context.Context, repo Repo, p Params) (*Opened, error) {
	snaps, err := repo.Snapshots(ctx)
	if err != nil {
		return nil, err
	}
	exports := exportsOf(snaps, p)
	if len(exports) == 0 {
		return nil, &ReadError{Msg: fmt.Sprintf("no backup of volume %s was found in the COS bucket area of policy %s", p.Volume, p.Policy)}
	}
	idx := exportFor(exports, p.At)
	e := exports[idx]
	newest := idx == len(exports)-1

	latest := func(info browse.Info) *Opened {
		root := Root{OID: e.backup.Root, Meta: browse.Entry{Mode: e.backup.RootMode, UID: e.backup.RootUID, GID: e.backup.RootGID, MTime: e.backup.RootMTime}}
		return &Opened{Tree: NewTree(repo, root, p.Tree), Info: info, Moments: momentsLister(repo, exports, p.Tree)}
	}
	exportInfo := browse.Info{Moment: e.moment.Format(browse.MomentLayout), Latest: newest}
	if e.moments == nil {
		return latest(exportInfo), nil
	}
	mt := NewTree(repo, Root{OID: e.moments.Root}, p.Tree)
	defer func() { _ = mt.Close() }()
	names, err := folderNames(mt)
	if err != nil {
		return nil, err
	}
	pick, pickNewest, err := browse.PickMoment(names, p.At)
	if err != nil {
		return nil, &ReadError{Msg: err.Error()}
	}
	if pick == "" {
		return latest(exportInfo), nil
	}
	if p.At.IsZero() {
		// The latest copy is the newest moment.
		return latest(browse.Info{Moment: pick, Latest: newest && pickNewest}), nil
	}
	ent, err := mt.Stat([][]byte{[]byte(pick)})
	if err != nil {
		return nil, err
	}
	oid, err := mt.folderOID(pick)
	if err != nil {
		return nil, err
	}
	root := Root{OID: oid, Meta: ent}
	return &Opened{Tree: NewTree(repo, root, p.Tree), Info: browse.Info{Moment: pick, Latest: newest && pickNewest},
		Moments: momentsLister(repo, exports, p.Tree)}, nil
}

// folderOID returns the object ID of the folder called name in the root.
func (t *Tree) folderOID(name string) (string, error) {
	m, err := t.manifestOf(t.root.OID)
	if err != nil {
		return "", err
	}
	e, ok := m.find([]byte(name))
	if !ok || e.typ != "d" || e.badOID {
		return "", &browse.Error{Code: browse.CodeNotFound, Msg: "no such moment in this backup"}
	}
	return e.obj, nil
}

// folderNames lists the names of the folders in the root of a Tree.
func folderNames(t *Tree) ([]string, error) {
	var names []string
	var cursor []byte
	for {
		page, err := t.List(nil, cursor, maxListLimit)
		if err != nil {
			return nil, err
		}
		for _, e := range page.Entries {
			if e.Type == browse.TypeDir {
				names = append(names, string(e.Name))
			}
		}
		if len(page.Next) == 0 {
			return names, nil
		}
		cursor = page.Next
	}
}

// momentsLister lists every moment folder in every export, once, when it is
// first asked for. Moments that the local keep time removed are still in the
// older exports.
func momentsLister(repo Repo, exports []export, opts TreeOptions) func() ([]string, error) {
	var (
		once sync.Once
		out  []string
		err  error
	)
	return func() ([]string, error) {
		once.Do(func() {
			seen := map[string]bool{}
			for i := len(exports) - 1; i >= 0; i-- {
				if exports[i].moments == nil {
					continue
				}
				mt := NewTree(repo, Root{OID: exports[i].moments.Root}, opts)
				names, ferr := folderNames(mt)
				_ = mt.Close()
				if ferr != nil {
					err = ferr
					return
				}
				for _, n := range names {
					if _, perr := time.Parse(browse.MomentLayout, n); perr == nil && !seen[n] {
						seen[n] = true
						out = append(out, n)
					}
				}
			}
			sort.Strings(out)
		})
		return out, err
	}
}
