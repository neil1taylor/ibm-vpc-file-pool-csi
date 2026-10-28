package cosbrowse

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
)

// Limits on what is read from the repository. A folder is read into memory, so
// the limits keep a hostile or damaged folder from using the helper's memory.
const (
	maxManifestBytes   = 256 << 20
	maxManifestEntries = 1_000_000
	// maxLinkTarget is the longest symbolic link target that is shown (PATH_MAX).
	maxLinkTarget = 4096
)

// mEntry is one name in a folder, as the folder manifest holds it.
type mEntry struct {
	name  []byte
	typ   string // Kopia's one letter: d, f, s, or another for a device, pipe or socket
	mode  uint32
	size  int64
	uid   uint32
	gid   uint32
	mtime time.Time
	obj   string
	// badOID is set when obj does not have the shape of an object ID. The entry
	// is listed but cannot be read, and its ID is never given to Kopia.
	badOID bool
}

// manifest is one folder: its entries in order of name, byte by byte.
type manifest struct {
	entries []mEntry
}

type rawEntry struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Mode  string `json:"mode"`
	Size  int64  `json:"size"`
	MTime string `json:"mtime"`
	UID   uint32 `json:"uid"`
	GID   uint32 `json:"gid"`
	Obj   string `json:"obj"`
}

type rawManifest struct {
	Stream  string     `json:"stream"`
	Entries []rawEntry `json:"entries"`
}

// parseManifest reads the JSON that `kopia show` prints for a folder. A name
// that cannot be a name in a path (empty, ".", "..", or holding "/" or NUL) is
// left out: no path can ask for it. The same name twice means the folder is
// damaged, and is an error, because two different files could not both be
// reached by their name.
func parseManifest(r io.Reader) (*manifest, error) {
	lr := &io.LimitedReader{R: r, N: maxManifestBytes + 1}
	var raw rawManifest
	if err := json.NewDecoder(lr).Decode(&raw); err != nil {
		if lr.N <= 0 {
			return nil, &browse.Error{Code: browse.CodeInternal, Msg: "this folder is too large to read from COS"}
		}
		return nil, damaged("the folder is not in the expected form")
	}
	if raw.Stream != "kopia:directory" {
		return nil, damaged("the object is not a folder")
	}
	if len(raw.Entries) > maxManifestEntries {
		return nil, &browse.Error{Code: browse.CodeInternal, Msg: fmt.Sprintf("this folder has more than %d names, too many to read from COS", maxManifestEntries)}
	}
	m := &manifest{entries: make([]mEntry, 0, len(raw.Entries))}
	for _, e := range raw.Entries {
		name := []byte(e.Name)
		if browse.ValidatePath([][]byte{name}) != nil {
			continue
		}
		me := mEntry{name: name, typ: e.Type, size: e.Size, uid: e.UID, gid: e.GID, obj: e.Obj, badOID: !ValidOID(e.Obj)}
		if v, err := strconv.ParseUint(e.Mode, 8, 32); err == nil {
			me.mode = uint32(v) & 0o7777
		}
		if t, err := time.Parse(time.RFC3339Nano, e.MTime); err == nil {
			me.mtime = t.UTC()
		}
		if me.size < 0 {
			me.size = 0
			me.badOID = true
		}
		m.entries = append(m.entries, me)
	}
	sort.Slice(m.entries, func(i, j int) bool { return bytes.Compare(m.entries[i].name, m.entries[j].name) < 0 })
	for i := 1; i < len(m.entries); i++ {
		if bytes.Equal(m.entries[i-1].name, m.entries[i].name) {
			return nil, damaged("a folder lists the same name twice")
		}
	}
	return m, nil
}

func damaged(why string) error {
	return &browse.Error{Code: browse.CodeInternal, Msg: why + "; the backup in COS may be damaged"}
}

// find returns the entry with this exact name.
func (m *manifest) find(name []byte) (mEntry, bool) {
	i := sort.Search(len(m.entries), func(i int) bool { return bytes.Compare(m.entries[i].name, name) >= 0 })
	if i < len(m.entries) && bytes.Equal(m.entries[i].name, name) {
		return m.entries[i], true
	}
	return mEntry{}, false
}
