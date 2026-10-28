package guestfs

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
)

// ParseListing reads the output of virt-ls --long --csv for one folder: one
// line for each name, with the type letter, the permissions in octal, the size,
// the name, and for a link its text. A line that cannot be a name of a file
// (empty, ".", "..", or one that holds "/") is left out, since a Linux or NTFS
// file system never has such a name and a path built from it could leave the
// folder.
func ParseListing(out []byte) ([]browse.Entry, error) {
	return parseListing(strings.NewReader(string(out)), browse.MaxDirEntries)
}

func parseListing(in io.Reader, limit int) ([]browse.Entry, error) {
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
		if len(rec) < 4 || rec[0] == "Type" {
			continue
		}
		name := []byte(rec[3])
		if browse.ValidatePath([][]byte{name}) != nil {
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
			e.Mode = uint32(m) & 0o7777
		}
		if e.Type == browse.TypeSymlink && len(rec) > 4 {
			e.Link = []byte(rec[4])
		}
		entries = append(entries, e)
	}
}

// typeOf maps the type letter of virt-ls: d folder, f file, l link; anything
// else (a device, a pipe, a socket, unknown) is "other".
func typeOf(letter string) string {
	switch strings.TrimSpace(letter) {
	case "d":
		return browse.TypeDir
	case "f", "r":
		return browse.TypeFile
	case "l":
		return browse.TypeSymlink
	}
	return browse.TypeOther
}
