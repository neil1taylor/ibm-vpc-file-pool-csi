package guestfs

import (
	"strings"
	"testing"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
)

func TestParseListing(t *testing.T) {
	in := "d,0755,4096,etc\nf,0600,5,\"a,b\"\nl,0777,3,ln,target\np,0644,0,pipe\nb,0660,0,disk\n?,0000,0,odd\nf,0644,notanumber,badsize\n"
	es, err := ParseListing([]byte(in))
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
	in := "Type,Permissions,Size,Path\nd,0755,0,ok\n\nonly,two\n,0755,0,notype\nd,0755,0,\nd,0755,0,a/b\nd,0755,0,..\nd,0755,0,.\n"
	es, err := ParseListing([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 2 || string(es[0].Name) != "ok" || string(es[1].Name) != "notype" || es[1].Type != browse.TypeOther {
		t.Fatalf("entries = %+v", es)
	}
}

func TestParseListing_SpecialNames(t *testing.T) {
	in := "f,0644,1,\"line\nbreak\"\nf,0644,1,\xff\xfe\n"
	es, err := ParseListing([]byte(in))
	if err != nil || len(es) != 2 {
		t.Fatalf("%v %+v", err, es)
	}
	if string(es[0].Name) != "line\nbreak" || string(es[1].Name) != "\xff\xfe" {
		t.Errorf("names = %q %q", es[0].Name, es[1].Name)
	}
}

func TestParseListing_TooManyNames(t *testing.T) {
	if _, err := parseListing(strings.NewReader(strings.Repeat("f,0644,1,x\n", 20)), 10); err == nil {
		t.Fatal("a folder over the limit must be refused")
	}
}

func FuzzParseListing(f *testing.F) {
	f.Add([]byte(rootListing))
	f.Add([]byte("f,0644,1,\"\x00\"\n"))
	f.Fuzz(func(t *testing.T, in []byte) {
		es, _ := ParseListing(in)
		for _, e := range es {
			if err := browse.ValidatePath([][]byte{e.Name}); err != nil {
				t.Fatalf("a bad name got through: %q", e.Name)
			}
		}
	})
}
