package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/filelist"
)

// modeBuildList reads the file systems of a VM disk once and writes the file
// list that a session in COS shows folders from (see
// docs/specs/2026-10-29-vm-disks-in-cos.md, section 1a). It runs with the same
// rules as the guest browse server: the disk is hostile, there are no Secrets,
// and the share is read-only except for the one output file.
const modeBuildList = "build-list"

// listReport is the termination message of a build: what was listed and what
// the person should be told. The names in notes are file system ids that the
// helper made, never names from inside the guest.
type listReport struct {
	// The key keeps the name "moment": the controller of an earlier version reads it.
	Moment  string   `json:"moment,omitempty"`
	Entries int      `json:"entries"`
	Bytes   int64    `json:"bytes"`
	Notes   []string `json:"notes,omitempty"`
}

// runBuildList returns the exit code: 0 when the list is written, 1 when it
// could not be built. The reason goes to the termination log in words.
func runBuildList(ctx context.Context, root, moments string, at time.Time, socket, appliance, consistency, out, termLog string, stderr io.Writer) int {
	code := buildList(ctx, root, moments, at, socket, appliance, consistency, out, termLog, stderr)
	if code != 0 {
		// The Job's second container waits for the list or for this marker.
		_ = os.WriteFile(out+failedSuffix, nil, 0o600) //nolint:gosec // the path is an operator flag, not user input
	}
	return code
}

// failedSuffix is added to the output path for the marker of a failed build.
const failedSuffix = ".failed"

func buildList(ctx context.Context, root, moments string, at time.Time, socket, appliance, consistency, out, termLog string, stderr io.Writer) int {
	g, err := openGuestSession(ctx, root, moments, at, socket, appliance, consistency)
	if err != nil {
		failStart(stderr, termLog, err)
		return 1
	}
	defer g.Close()
	moment := time.Now().UTC()
	if t, perr := time.Parse(browse.MomentLayout, g.Info.Moment); perr == nil {
		moment = t
	}
	l, notes, err := g.Tree.BuildList(ctx, moment, filelist.Limits{})
	if err != nil {
		failStart(stderr, termLog, fmt.Errorf("the file list could not be built: %w", err))
		return 1
	}
	size, err := writeAtomically(out, l.Encode)
	if err != nil {
		failStart(stderr, termLog, fmt.Errorf("the file list could not be saved: %w", err))
		return 1
	}
	if g.consistency == consistencyCrash {
		notes = append(notes, crashNotice)
	}
	msg, _ := json.Marshal(listReport{Moment: moment.Format(time.RFC3339), Entries: l.Entries(), Bytes: size, Notes: notes})
	if len(msg) > 3500 { // the termination message holds 4 KiB
		msg, _ = json.Marshal(listReport{Moment: moment.Format(time.RFC3339), Entries: l.Entries(), Bytes: size})
	}
	_ = os.WriteFile(termLog, msg, 0o600) //nolint:gosec // the path is an operator flag, not user input
	return 0
}

// writeAtomically writes the file next to its final name and renames it, so a
// reader never sees half a list. A failure leaves nothing behind.
func writeAtomically(final string, write func(io.Writer) error) (int64, error) {
	f, err := os.CreateTemp(filepath.Dir(final), ".filelist-*")
	if err != nil {
		return 0, err
	}
	tmp := f.Name()
	done := false
	defer func() {
		if !done {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if err = write(f); err != nil {
		return 0, err
	}
	if err = f.Sync(); err != nil {
		return 0, err
	}
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if err = f.Close(); err != nil {
		return 0, err
	}
	if err = os.Rename(tmp, final); err != nil {
		return 0, err
	}
	done = true
	return st.Size(), nil
}

func validListOut(p string) bool {
	return strings.HasPrefix(p, "/") && !strings.Contains(p, "..") && !strings.HasSuffix(p, "/")
}
