package guestfs

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
)

// lsFlags are the options of every listing: a long listing in CSV (virt-ls 1.52
// allows --csv with -l only together with -R), with times as seconds since the
// epoch and the owner. A line holds the type letter, the permissions in octal,
// the size, the user and group numbers, the access, change-of-content and
// status times, the full path, and for a link its text.
var lsFlags = []string{"-lR", "--csv", "--time-t", "--uids"}

// Positions of the fields of a line written with lsFlags.
const (
	fieldType = 0
	fieldMode = 1
	fieldSize = 2
	fieldUID  = 3
	fieldGID  = 4
	fieldTime = 6 // the time the content last changed
	fieldPath = 8
	fieldLink = 9
	minFields = fieldPath + 1
)

// lsArgs is the command line that lists a folder and everything below it.
func (g *Guest) lsArgs(mount, folder string) []string {
	args := append([]string{"-m", mount}, lsFlags...)
	return g.driveArgs("virt-ls", append(args, "--", folder)...)
}

// ParseListing reads the output of virt-ls with lsFlags -- FOLDER and returns the
// names directly in the folder. The output holds the folder itself (the first
// line) and every name below it, one line each. Only a line whose path has the folder
// as its parent is kept, and its name is the last part of the path. The folder's
// own line and the lines of deeper names are left out; the deeper names do not
// count against the limit of names.
//
// Because -R walks the whole tree below the folder, listing a folder with a deep
// tree is slow, and the output is as long as the tree. A line that cannot be a
// name of a file (empty, ".", "..", or one that holds "/") is left out, since a
// Linux or NTFS file system never has such a name and a path built from it could
// leave the folder.
func ParseListing(out []byte, folder string) ([]browse.Entry, error) {
	return parseListing(strings.NewReader(string(out)), folder, browse.MaxDirEntries)
}

func parseListing(in io.Reader, folder string, limit int) ([]browse.Entry, error) {
	prefix := strings.TrimRight(folder, "/") + "/"
	r := csv.NewReader(in)
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	var entries []browse.Entry
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			return entries, nil
		}
		if err != nil {
			return entries, nil // a broken quote ends the list; what was read stays
		}
		if len(rec) < minFields || rec[fieldType] == "Type" || !strings.HasPrefix(rec[fieldPath], prefix) {
			continue
		}
		name := []byte(rec[fieldPath][len(prefix):])
		if bytes.IndexByte(name, '/') >= 0 || browse.ValidatePath([][]byte{name}) != nil {
			continue
		}
		if len(entries) >= limit {
			return nil, &browse.Error{Code: browse.CodeInternal, Msg: fmt.Sprintf("the folder has more than %d names", limit)}
		}
		e := entryOf(rec, name)
		entries = append(entries, e)
	}
}

// typeOf maps the type letter of virt-ls: d folder, - a regular file (r and f
// are accepted too), l link; anything else (a device, a pipe, a socket, unknown)
// is "other".
func typeOf(letter string) string {
	switch strings.TrimSpace(letter) {
	case "d":
		return browse.TypeDir
	case "-", "f", "r":
		return browse.TypeFile
	case "l":
		return browse.TypeSymlink
	}
	return browse.TypeOther
}

// entryOf makes the entry of one virt-ls line (at least minFields fields) for name.
func entryOf(rec []string, name []byte) browse.Entry {
	e := browse.Entry{Name: name, Type: typeOf(rec[fieldType]), Readable: true}
	if n, err := strconv.ParseInt(strings.TrimSpace(rec[fieldSize]), 10, 64); err == nil && n >= 0 {
		e.Size = n
	}
	if m, err := strconv.ParseUint(strings.TrimSpace(rec[fieldMode]), 8, 32); err == nil {
		e.Mode = uint32(m) & 0o7777 //nolint:gosec // ParseUint was limited to 32 bits
	}
	if n, err := strconv.ParseUint(strings.TrimSpace(rec[fieldUID]), 10, 32); err == nil {
		e.UID = uint32(n) //nolint:gosec // ParseUint was limited to 32 bits
	}
	if n, err := strconv.ParseUint(strings.TrimSpace(rec[fieldGID]), 10, 32); err == nil {
		e.GID = uint32(n) //nolint:gosec // ParseUint was limited to 32 bits
	}
	if n, err := strconv.ParseInt(strings.TrimSpace(rec[fieldTime]), 10, 64); err == nil && n > 0 {
		e.MTime = time.Unix(n, 0).UTC()
	}
	if e.Type == browse.TypeSymlink && len(rec) > fieldLink {
		e.Link = []byte(rec[fieldLink])
	}
	return e
}
