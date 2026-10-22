package pool

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// A listing that is larger than a termination message is cut, and says so.
func TestListAwk_TruncatesAndStaysValidJSON(t *testing.T) {
	var in strings.Builder
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&in, "id%d /share0/backup/p/pvcs/%s 2026-10-21T%02d:%02d:00Z %s\n", i, testExportVolume, i/60%24, i%60, testExportVolume)
	}
	cmd := exec.Command("awk", "-v", "vol=", "-v", fmt.Sprintf("limit=%d", cosListLimit), listAwk)
	cmd.Stdin = strings.NewReader(in.String())
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > 4096 {
		t.Errorf("the listing has %d bytes; a termination message holds 4096", len(out))
	}
	got := parseCOSListing(string(out))
	if !got.Truncated || len(got.Volumes) != 1 || len(got.Volumes[0].Moments) < 50 {
		t.Errorf("listing = truncated %v, %d volumes, from %q", got.Truncated, len(got.Volumes), out)
	}
}

func TestListAwk_IgnoresMomentsFoldersAndFiltersByVolume(t *testing.T) {
	in := "a /share0/backup/p/pvcs/v1 2026-10-21T09:00:00Z v1\n" +
		"b /share0/backup/p/.moments/pvcs/v1 2026-10-21T09:00:00Z v1\n" +
		"c /share0/backup/p/pvcs/v2 2026-10-21T09:00:00Z v2\n"
	run := func(vol string) COSListing {
		cmd := exec.Command("awk", "-v", "vol="+vol, "-v", "limit=3500", listAwk)
		cmd.Stdin = strings.NewReader(in)
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return parseCOSListing(string(out))
	}
	if got := run(""); len(got.Volumes) != 2 || len(got.Volumes[0].Moments) != 1 {
		t.Errorf("all volumes: %+v", got)
	}
	if got := run("v2"); len(got.Volumes) != 1 || got.Volumes[0].Name != "v2" {
		t.Errorf("filter v2: %+v", got)
	}
}
