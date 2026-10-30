package guestfs

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
)

// ParseListing reads the output of virt-ls -lR --csv -- FOLDER and returns the
// names directly in the folder. virt-ls 1.52 allows --csv with -l only together
// with -R, so the output holds the folder itself (the first line) and every name
// below it, one line each: the type letter, the permissions in octal, the size,
// the full path, and for a link its text. Only a line whose path has the folder
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
		if len(rec) < 4 || rec[0] == "Type" || !strings.HasPrefix(rec[3], prefix) {
			continue
		}
		name := []byte(rec[3][len(prefix):])
		if bytes.IndexByte(name, '/') >= 0 || browse.ValidatePath([][]byte{name}) != nil {
			continue
		}
		if len(entries) >= limit {
			return nil, &browse.Error{Code: browse.CodeInternal, Msg: fmt.Sprintf("the folder has more than %d names", limit)}
		}
		e := browse.Entry{Name: name, Type: typeOf(rec[0]), Readable: true}
		if n, perr := strconv.ParseInt(strings.TrimSpace(rec[2]), 10, 64); perr == nil && n >= 0 {
			e.Size = n
		}
		if m, perr := strconv.ParseUint(strings.TrimSpace(rec[1]), 8, 32); perr == nil {
			e.Mode = uint32(m) & 0o7777 //nolint:gosec // ParseUint was limited to 32 bits
		}
		if e.Type == browse.TypeSymlink && len(rec) > 4 {
			e.Link = []byte(rec[4])
		}
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
