package browse

import (
	"bytes"
	"strings"
	"testing"
)

func TestValidatePath(t *testing.T) {
	long := bytes.Repeat([]byte("a"), MaxNameLen+1)
	deep := make([][]byte, MaxPathDepth+1)
	for i := range deep {
		deep[i] = []byte("d")
	}
	tests := []struct {
		name string
		path [][]byte
		ok   bool
	}{
		{"root", nil, true},
		{"plain", [][]byte{[]byte("a"), []byte("b.txt")}, true},
		{"spaces and accents", [][]byte{[]byte("my doc é.txt")}, true},
		{"newline in a name", [][]byte{[]byte("a\nb")}, true},
		{"invalid utf-8", [][]byte{{0xff, 0xfe, 'x'}}, true},
		{"dot dot", [][]byte{[]byte("a"), []byte("..")}, false},
		{"dot", [][]byte{[]byte(".")}, false},
		{"empty name", [][]byte{[]byte("a"), {}}, false},
		{"slash in a name", [][]byte{[]byte("a/b")}, false},
		{"nul in a name", [][]byte{[]byte("a\x00b")}, false},
		{"name too long", [][]byte{long}, false},
		{"name at the limit", [][]byte{long[:MaxNameLen]}, true},
		{"too deep", deep, false},
		{"dot dot dot is a name", [][]byte{[]byte("...")}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePath(tt.path)
			if (err == nil) != tt.ok {
				t.Fatalf("ValidatePath(%q) = %v, want ok=%v", tt.path, err, tt.ok)
			}
			if err != nil {
				if e, isErr := err.(*Error); !isErr || e.Code != CodeBadPath {
					t.Fatalf("error %v: want an *Error with code %s", err, CodeBadPath)
				}
			}
		})
	}
}

func TestEscapeRoundTrip(t *testing.T) {
	names := [][]byte{
		[]byte("plain"), []byte("my doc é.txt"), []byte("a\nb"), {0xff, 0xfe, 'x'},
		[]byte(`back\slash`), []byte("tab\there"), []byte("nul\x00byte"), []byte("日本語"),
	}
	for _, n := range names {
		esc := Escape(n)
		if strings.ContainsAny(esc, "\n\t\x00") {
			t.Errorf("Escape(%q) = %q holds a control character", n, esc)
		}
		back, err := Unescape(esc)
		if err != nil || !bytes.Equal(back, n) {
			t.Errorf("Unescape(Escape(%q)) = %q, %v", n, back, err)
		}
	}
	if got := Escape([]byte("my doc é.txt")); got != "my doc é.txt" {
		t.Errorf("printable text changed: %q", got)
	}
	if got := Escape([]byte("a\nb")); got != `a\x0ab` {
		t.Errorf("Escape newline = %q", got)
	}
}

func TestUnescapeRejectsBadInput(t *testing.T) {
	for _, s := range []string{`a\`, `a\x`, `a\x1`, `a\xzz`, `a\q`} {
		if _, err := Unescape(s); err == nil {
			t.Errorf("Unescape(%q) should fail", s)
		}
	}
}

func TestSplitAndJoinPath(t *testing.T) {
	p, err := SplitPath("/var//www/html/")
	if err != nil || len(p) != 3 || string(p[0]) != "var" || string(p[2]) != "html" {
		t.Fatalf("SplitPath = %q, %v", p, err)
	}
	if got := JoinPath(p); got != "/var/www/html" {
		t.Errorf("JoinPath = %q", got)
	}
	if p, _ := SplitPath("/"); len(p) != 0 {
		t.Errorf("root should be the empty path, got %q", p)
	}
	if got := JoinPath(nil); got != "/" {
		t.Errorf("JoinPath(root) = %q", got)
	}
	// An escaped slash is one name, so ValidatePath refuses it later.
	p, _ = SplitPath(`/a\x2fb`)
	if len(p) != 1 || string(p[0]) != "a/b" {
		t.Errorf("escaped slash: %q", p)
	}
	if _, err := SplitPath(`/a\xzz`); err == nil {
		t.Error("bad escape should fail")
	}
	// A path survives the trip, including odd names.
	odd := [][]byte{[]byte("a b"), {0xff, 'z'}, []byte("n\nl")}
	back, err := SplitPath(JoinPath(odd))
	if err != nil || len(back) != 3 || !bytes.Equal(back[1], odd[1]) || !bytes.Equal(back[2], odd[2]) {
		t.Errorf("round trip = %q, %v", back, err)
	}
}
