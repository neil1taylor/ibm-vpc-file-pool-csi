package util

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

var testAliases = []FlagAlias{{Old: "moment", New: "restore-point"}, {Old: "moments", New: "restore-points"}}

func TestRewriteAliasedArgs(t *testing.T) {
	tests := []struct {
		name    string
		in      []string
		want    []string
		stderr  string
		wantErr bool
	}{
		{name: "new", in: []string{"--restore-point", "x"}, want: []string{"--restore-point", "x"}},
		{name: "old equals", in: []string{"--moment=x", "--a", "b"}, want: []string{"--restore-point=x", "--a", "b"},
			stderr: "--moment is deprecated; use --restore-point\n"},
		{name: "old space, single dash", in: []string{"-moments", "d"}, want: []string{"-restore-points", "d"},
			stderr: "--moments is deprecated; use --restore-points\n"},
		{name: "both same", in: []string{"--moment=x", "--restore-point", "x"}, want: []string{"--restore-point=x", "--restore-point", "x"},
			stderr: "--moment is deprecated; use --restore-point\n"},
		{name: "both differ", in: []string{"--moment=x", "--restore-point=y"}, wantErr: true},
		{name: "after double dash untouched", in: []string{"--", "--moment=x"}, want: []string{"--", "--moment=x"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var errb bytes.Buffer
			got, err := RewriteAliasedArgs(tt.in, testAliases, &errb)
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "--moment") || !strings.Contains(err.Error(), "--restore-point") {
					t.Fatalf("err = %v, want one naming both flags", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("args = %v, want %v", got, tt.want)
			}
			if errb.String() != tt.stderr {
				t.Errorf("stderr = %q, want %q", errb.String(), tt.stderr)
			}
		})
	}
}

func TestRewriteAliasedArgs_FlagWithoutAValueIsNotAConflict(t *testing.T) {
	aliases := []FlagAlias{{Old: "moments", New: "restore-points"}}
	var errb bytes.Buffer
	got, err := RewriteAliasedArgs([]string{"--moments", "--restore-points", "/x"}, aliases, &errb)
	if err != nil {
		t.Fatalf("a flag with no value is not a different value: %v", err)
	}
	if want := []string{"--restore-points", "--restore-points", "/x"}; !reflect.DeepEqual(got, want) {
		t.Errorf("args = %v, want %v", got, want)
	}
}

func TestRewriteAliasedArgs_NothingAfterDoubleDashIsRead(t *testing.T) {
	var errb bytes.Buffer
	in := []string{"--moment", "--", "--moment=x", "--restore-point=y"}
	got, err := RewriteAliasedArgs(in, testAliases, &errb)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"--restore-point", "--", "--moment=x", "--restore-point=y"}; !reflect.DeepEqual(got, want) {
		t.Errorf("args = %v, want %v", got, want)
	}
	if errb.String() != "--moment is deprecated; use --restore-point\n" {
		t.Errorf("stderr = %q", errb.String())
	}
}
