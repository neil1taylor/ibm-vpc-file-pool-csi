package util

import "testing"

func TestShellQuote(t *testing.T) {
	tests := map[string]string{
		"plain":       `'plain'`,
		"a b":         `'a b'`,
		"it's":        `'it'\''s'`,
		`$(id)`:       `'$(id)'`,
		"x; touch /y": `'x; touch /y'`,
		"":            `''`,
	}
	for in, want := range tests {
		if got := ShellQuote(in); got != want {
			t.Errorf("ShellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestValidRsyncOption(t *testing.T) {
	for _, ok := range []string{"--compress", "-v", "--bwlimit=100", "--exclude=*.tmp", "--exclude=/a/b"} {
		if !ValidRsyncOption(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "compress", "--a b", "--a;id", "--a=$(id)", "--a=`id`", "--a='x'", "--a\nb", "---a"} {
		if ValidRsyncOption(bad) {
			t.Errorf("%q should be refused", bad)
		}
	}
}

func TestValidBasePath(t *testing.T) {
	for _, ok := range []string{"/", "/pvcs", "/dr/site-1/pvcs_a.b"} {
		if !ValidBasePath(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "pvcs", "/a/../b", "/a b", `/a"b`, "/a$b", "/a\nb", "/.."} {
		if ValidBasePath(bad) {
			t.Errorf("%q should be refused", bad)
		}
	}
}
