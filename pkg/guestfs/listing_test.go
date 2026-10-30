package guestfs

import (
	"strings"
	"testing"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
)

func TestParseListing(t *testing.T) {
	in := "d,0755,4096,/\nd,0755,4096,/etc\n-,0600,5,\"/a,b\"\nl,0777,3,/ln,target\np,0644,0,/pipe\nb,0660,0,/disk\n?,0000,0,/odd\n-,0644,notanumber,/badsize\n"
	es, err := ParseListing([]byte(in), "/")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"etc": browse.TypeDir, "a,b": browse.TypeFile, "ln": browse.TypeSymlink, "pipe": browse.TypeOther, "disk": browse.TypeOther, "odd": browse.TypeOther, "badsize": browse.TypeFile}
	if len(es) != len(want) {
		t.Fatalf("got %d entries: %+v", len(es), es)
	}
	for _, e := range es {
		if want[string(e.Name)] != e.Type {
			t.Errorf("%s is %s", e.Name, e.Type)
		}
		if e.Type == browse.TypeSymlink && string(e.Link) != "target" {
			t.Errorf("link = %q", e.Link)
		}
		if string(e.Name) == "a,b" && (e.Mode != 0o600 || e.Size != 5) {
			t.Errorf("a,b = %+v", e)
		}
		if string(e.Name) == "badsize" && e.Size != 0 {
			t.Errorf("badsize = %d", e.Size)
		}
		if !e.Readable {
			t.Errorf("%s not readable", e.Name)
		}
	}
}

func TestParseListing_HeaderAndOddLines(t *testing.T) {
	in := "Type,Permissions,Size,Path\nd,0755,0,/ok\n\nonly,two\n,0755,0,/notype\nd,0755,0,/\nd,0755,0,/..\nd,0755,0,/.\nd,0755,0,x\nd,0755,0,\n"
	es, err := ParseListing([]byte(in), "/")
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 2 || string(es[0].Name) != "ok" || string(es[1].Name) != "notype" || es[1].Type != browse.TypeOther {
		t.Fatalf("entries = %+v", es)
	}
}

func TestParseListing_SpecialNames(t *testing.T) {
	in := "-,0644,1,\"/line\nbreak\"\n-,0644,1,/\xff\xfe\n"
	es, err := ParseListing([]byte(in), "/")
	if err != nil || len(es) != 2 {
		t.Fatalf("%v %+v", err, es)
	}
	if string(es[0].Name) != "line\nbreak" || string(es[1].Name) != "\xff\xfe" {
		t.Errorf("names = %q %q", es[0].Name, es[1].Name)
	}
}

func TestParseListing_TooManyNames(t *testing.T) {
	if _, err := parseListing(strings.NewReader(strings.Repeat("-,0644,1,/x\n", 20)), "/", 10); err == nil {
		t.Fatal("a folder over the limit must be refused")
	}
}

func FuzzParseListing(f *testing.F) {
	f.Add([]byte(rootListing))
	f.Add([]byte("-,0644,1,\"/\x00\"\n"))
	f.Fuzz(func(t *testing.T, in []byte) {
		es, _ := ParseListing(in, "/")
		for _, e := range es {
			if err := browse.ValidatePath([][]byte{e.Name}); err != nil {
				t.Fatalf("a bad name got through: %q", e.Name)
			}
		}
	})
}

// Real output of virt-ls 1.52.2 -lR --csv on an NTFS disk of the lab: the
// folder is the first line, every name has its full path, a name may hold a
// line break, and there is no header.
const labRootListing = "d,0777,4096,/\n-,0777,4194304,/big.bin\nd,0777,0,/docs\n-,0777,3,/docs/a.txt\n-,0777,5,/hello.txt\n-,0777,1,\"/line\nbreak\"\n"

func TestParseListing_RealOutputOfTheRoot(t *testing.T) {
	es, err := ParseListing([]byte(labRootListing), "/")
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		typ  string
		size int64
		mode uint32
	}
	want := map[string]row{
		"big.bin":     {browse.TypeFile, 4194304, 0o777},
		"docs":        {browse.TypeDir, 0, 0o777},
		"hello.txt":   {browse.TypeFile, 5, 0o777},
		"line\nbreak": {browse.TypeFile, 1, 0o777},
	}
	if len(es) != len(want) {
		t.Fatalf("got %d entries, want only the direct children: %+v", len(es), es)
	}
	for _, e := range es {
		w, ok := want[string(e.Name)]
		if !ok || e.Type != w.typ || e.Size != w.size || e.Mode != w.mode {
			t.Errorf("%q = %+v, want %+v", e.Name, e, w)
		}
	}
}

func TestParseListing_RealOutputOfASubfolder(t *testing.T) {
	es, err := ParseListing([]byte("d,0777,0,/docs\n-,0777,3,/docs/a.txt\n"), "/docs")
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 1 || string(es[0].Name) != "a.txt" || es[0].Type != browse.TypeFile || es[0].Size != 3 {
		t.Fatalf("entries = %+v", es)
	}
}

func TestParseListing_OnlyDirectChildrenCount(t *testing.T) {
	in := "d,0755,0,/d\n-,0644,1,/d/one\nd,0755,0,/d/sub\n-,0644,1,/d/sub/deep\n-,0644,1,/d/sub/deeper\n-,0644,1,/dx/other\n"
	es, err := ParseListing([]byte(in), "/d")
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 2 || string(es[0].Name) != "one" || string(es[1].Name) != "sub" {
		t.Fatalf("entries = %+v", es)
	}
	// Names below the first level do not count against the limit.
	deep := "d,0755,0,/d\n-,0644,1,/d/one\n" + strings.Repeat("-,0644,1,/d/one/x\n", 50)
	if es, err = parseListing(strings.NewReader(deep), "/d", 3); err != nil || len(es) != 1 {
		t.Fatalf("%v %+v", err, es)
	}
}

func TestTypeOf_RegularFileLetterOfVirtLs(t *testing.T) {
	for letter, want := range map[string]string{"-": browse.TypeFile, "r": browse.TypeFile, "f": browse.TypeFile, "d": browse.TypeDir, "l": browse.TypeSymlink, "b": browse.TypeOther} {
		if got := typeOf(letter); got != want {
			t.Errorf("typeOf(%q) = %s, want %s", letter, got, want)
		}
	}
}
