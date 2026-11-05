package main

import (
	"bytes"
	"flag"
	"strings"
	"testing"
)

func withFreshFlags(t *testing.T) *string {
	t.Helper()
	old := flag.CommandLine
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(&bytes.Buffer{})
	flag.CommandLine = fs
	t.Cleanup(func() { flag.CommandLine = old })
	return fs.String("restore-to-restore-point", "", "")
}

func TestParseCommandLine_NewFlag(t *testing.T) {
	got := withFreshFlags(t)
	var errb bytes.Buffer
	if err := parseCommandLine([]string{"--restore-to-restore-point", "2026-10-21T09:00:00Z"}, &errb); err != nil {
		t.Fatal(err)
	}
	if *got != "2026-10-21T09:00:00Z" || errb.Len() != 0 {
		t.Errorf("value = %q, stderr = %q", *got, errb.String())
	}
}

func TestParseCommandLine_OldFlagStillWorksAndWarns(t *testing.T) {
	got := withFreshFlags(t)
	var errb bytes.Buffer
	if err := parseCommandLine([]string{"--restore-moment=2026-10-21T09:00:00Z"}, &errb); err != nil {
		t.Fatal(err)
	}
	if *got != "2026-10-21T09:00:00Z" {
		t.Errorf("value = %q", *got)
	}
	if want := "--restore-moment is deprecated; use --restore-to-restore-point\n"; errb.String() != want {
		t.Errorf("stderr = %q, want %q", errb.String(), want)
	}
}

func TestParseCommandLine_OldAndNewThatDifferAreRefused(t *testing.T) {
	withFreshFlags(t)
	err := parseCommandLine([]string{"--restore-moment=a", "--restore-to-restore-point=b"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "--restore-moment") || !strings.Contains(err.Error(), "--restore-to-restore-point") {
		t.Fatalf("err = %v, want one that names both flags", err)
	}
}
