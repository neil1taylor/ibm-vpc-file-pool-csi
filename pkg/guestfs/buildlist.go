package guestfs

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/filelist"
	"k8s.io/klog/v2"
)

// BuildList reads every file system of the disk once and returns the file list
// for the moment. Each file system costs one virt-ls run over the whole tree
// from its top; listing it folder by folder would start an appliance for each
// folder. A file system that cannot be read is listed as unreadable with a
// sentence for the person and does not fail the others; notes say which. An
// error means the list could not be built at all: the disk cannot be read, or
// it holds more than lim allows.
func (g *Guest) BuildList(ctx context.Context, moment time.Time, lim filelist.Limits) (*filelist.List, []string, error) {
	fss, err := g.Filesystems(ctx)
	if err != nil {
		return nil, nil, err
	}
	if len(fss) > lim.FilesystemLimit() {
		return nil, nil, fmt.Errorf("the disk has %d file systems; the list holds at most %d", len(fss), lim.FilesystemLimit())
	}
	l := filelist.NewListWithLimits(moment, lim)
	var notes []string
	left, budget := lim.EntryLimit(), lim.ByteLimit()
	for _, f := range fss {
		if cerr := g.check(); cerr != nil {
			return nil, nil, cerr
		}
		fsys := filelist.Filesystem{ID: f.ID, VFS: f.VFS, Label: f.Label, Size: f.Size, Readable: f.Readable, Reason: f.Reason}
		var folders map[string][]browse.Entry
		if f.Readable {
			var n listed
			folders, n, err = g.listAll(ctx, f.ID, left, budget)
			switch {
			case errors.Is(err, errTooMany):
				return nil, nil, fmt.Errorf("the disk holds more than %d files and folders, more than the list allows", lim.EntryLimit())
			case err != nil:
				if ctx.Err() != nil {
					return nil, nil, ctx.Err()
				}
				klog.V(2).InfoS("A file system could not be listed for the file list", "fs", f.ID, "err", err)
				fsys.Readable, fsys.Reason = false, "This volume could not be listed when the backup was made."
				notes = append(notes, fmt.Sprintf("%s could not be listed, so its files cannot be browsed without the disk.", f.ID))
				folders = nil
			default:
				left -= n.entries
				budget -= n.bytes
			}
		}
		l.AddFilesystem(fsys)
		if folders == nil {
			continue
		}
		if err := addReachable(l, f.ID, folders); err != nil {
			return nil, nil, err
		}
	}
	notes = append(notes, g.Notices()...)
	return l, notes, nil
}

var errTooMany = errors.New("too many entries")

// listed is what listAll holds in memory: the number of names, and an estimate
// of their bytes (the names, the link targets and a fixed cost for each).
type listed struct {
	entries int
	bytes   int64
}

// entryOverhead is the estimated cost of one entry besides its name and link.
const entryOverhead = 160

// maxLineLen bounds one line of the listing, so that a hostile name cannot make
// the reader hold a line of any length.
const maxLineLen = 16 << 10

// lineLimit passes bytes through and fails when a line is longer than max. A
// quoted name that holds newlines counts line by line, and the whole stream is
// bounded by total.
type lineLimit struct {
	r         io.Reader
	max       int
	run       int
	remaining int64
}

var errLineTooLong = errors.New("a line of the listing is too long")

func (l *lineLimit) Read(p []byte) (int, error) {
	if l.remaining <= 0 {
		return 0, errTooMany
	}
	if int64(len(p)) > l.remaining {
		p = p[:l.remaining]
	}
	n, err := l.r.Read(p)
	l.remaining -= int64(n)
	for _, b := range p[:n] {
		if b == '\n' {
			l.run = 0
		} else if l.run++; l.run > l.max {
			return n, errLineTooLong
		}
	}
	return n, err
}

// listAll reads the whole tree of one file system and returns it grouped by
// folder. The key is the folder as a path from the top ("" is the top, "/a/b"
// is below it). At most max names and about budget bytes are held.
func (g *Guest) listAll(ctx context.Context, id string, max int, budget int64) (map[string][]browse.Entry, listed, error) {
	f, err := g.readable([]byte(id))
	if err != nil {
		return nil, listed{}, err
	}
	_, rc, release, err := g.run(ctx, f, func(mount string) []string {
		return g.driveArgs("virt-ls", "-m", mount, "-lR", "--csv", "--", "/")
	}, true)
	if err != nil {
		return nil, listed{}, err
	}
	defer release()
	defer func() { _ = rc.Close() }()
	r := csv.NewReader(&lineLimit{r: rc, max: maxLineLen, remaining: 2 * budget})
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	out := map[string][]browse.Entry{"": nil}
	var held listed
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			return out, held, nil
		}
		if err != nil {
			return nil, listed{}, err
		}
		if len(rec) < 4 || rec[0] == "Type" || !strings.HasPrefix(rec[3], "/") || rec[3] == "/" {
			continue
		}
		i := strings.LastIndexByte(rec[3], '/')
		parent, name := rec[3][:i], []byte(rec[3][i+1:])
		if browse.ValidatePath([][]byte{name}) != nil {
			continue
		}
		e := entryOf(rec, name)
		if len(e.Link) > filelist.MaxLinkLen {
			e.Link = e.Link[:filelist.MaxLinkLen] // a longer target is cut, not refused
		}
		held.entries++
		held.bytes += int64(entryOverhead + len(name) + len(e.Link))
		if held.entries > max || held.bytes > budget {
			return nil, listed{}, errTooMany
		}
		out[parent] = append(out[parent], e)
		if e.Type == browse.TypeDir {
			if _, ok := out[parent+"/"+string(name)]; !ok {
				out[parent+"/"+string(name)] = nil // an empty folder still has a line
			}
		}
	}
}

// addReachable adds the folders that can be reached from the top through
// folders their parent lists. A folder whose parent was left out (for example
// because its name was not safe) is dropped with everything below it.
func addReachable(l *filelist.List, id string, folders map[string][]browse.Entry) error {
	queue := []string{""}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		entries := folders[cur]
		var path [][]byte
		for _, p := range strings.Split(strings.TrimPrefix(cur, "/"), "/") {
			if p != "" {
				path = append(path, []byte(p))
			}
		}
		if err := l.AddFolder(id, path, entries); err != nil {
			return err
		}
		sort.Slice(entries, func(i, j int) bool { return bytes.Compare(entries[i].Name, entries[j].Name) < 0 })
		for _, e := range entries {
			if e.Type == browse.TypeDir {
				queue = append(queue, cur+"/"+string(e.Name))
			}
		}
	}
	return nil
}
