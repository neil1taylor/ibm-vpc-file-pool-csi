package browse

import (
	"archive/tar"
	"bytes"
	"io"
	"testing"
)

type tarFile struct {
	hdr  tar.Header
	data string
}

// walkTree is a Tree that counts how a tar reads it.
type walkTree struct {
	lists, walks int
	dirs         map[string][]Entry
	files        map[string]string
}

func key(path [][]byte) string { return JoinPath(path) }

func (w *walkTree) List([][]byte, []byte, int) (ListPage, error) { w.lists++; return ListPage{}, nil }
func (w *walkTree) Stat(path [][]byte) (Entry, error) {
	if _, ok := w.dirs[key(path)]; ok {
		return Entry{Type: TypeDir, Mode: 0o755}, nil
	}
	return Entry{Type: TypeFile, Mode: 0o644, Size: int64(len(w.files[key(path)]))}, nil
}
func (w *walkTree) Open(path [][]byte) (File, error)           { return memFile{[]byte(w.files[key(path)])}, nil }
func (w *walkTree) Tar(io.Writer, [][]byte) (TarReport, error) { return TarReport{}, nil }
func (w *walkTree) Walk(path [][]byte, fn func(Entry) error) error {
	w.walks++
	for _, e := range w.dirs[key(path)] {
		if err := fn(e); err != nil {
			return err
		}
	}
	return nil
}

func TestTarReadsEachFolderOnce(t *testing.T) {
	w := &walkTree{
		dirs: map[string][]Entry{
			"/":    {{Name: []byte("a"), Type: TypeDir, Mode: 0o755}, {Name: []byte("f1"), Type: TypeFile, Mode: 0o644, Size: 1}},
			"/a":   {{Name: []byte("b"), Type: TypeDir, Mode: 0o755}, {Name: []byte("f2"), Type: TypeFile, Mode: 0o644, Size: 2}},
			"/a/b": {{Name: []byte("f3"), Type: TypeFile, Mode: 0o644, Size: 3}},
		},
		files: map[string]string{"/f1": "1", "/a/f2": "22", "/a/b/f3": "333"},
	}
	var buf bytes.Buffer
	rep, err := writeTar(w, &buf, nil)
	if err != nil {
		t.Fatal(err)
	}
	if w.lists != 0 || w.walks != 3 {
		t.Errorf("List called %d times, Walk %d times; want 0 and 3 (one read per folder)", w.lists, w.walks)
	}
	got := tarContents(t, buf.Bytes())
	if got["a/b/f3"].data != "333" || got["f1"].data != "1" || rep.Files != 3 {
		t.Errorf("tar content wrong: %d entries, report %+v", len(got), rep)
	}
}

func tarContents(t *testing.T, b []byte) map[string]tarFile {
	t.Helper()
	out := map[string]tarFile{}
	r := tar.NewReader(bytes.NewReader(b))
	for {
		hdr, err := r.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(r)
		out[hdr.Name] = tarFile{hdr: *hdr, data: string(data)}
	}
}
