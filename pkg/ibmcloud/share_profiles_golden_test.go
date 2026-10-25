package ibmcloud

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"testing"
)

var updateDP2Golden = flag.Bool("update-dp2", false, "rewrite the dp2 table golden file")

// dp2GoldenPath is read by the console plugin tests too (console-plugin/src/utils/dp2.test.ts),
// so the PoolBench form offers the same IOPS range that the admission check allows.
const dp2GoldenPath = "testdata/dp2-ranges.golden.json"

func TestDP2Golden(t *testing.T) {
	rows := make([]map[string]int64, 0, len(dp2Ranges))
	for _, r := range dp2Ranges {
		rows = append(rows, map[string]int64{"minSizeGB": r.minSizeGB, "maxSizeGB": r.maxSizeGB, "minIOPS": r.minIOPS, "maxIOPS": r.maxIOPS})
	}
	got, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if *updateDP2Golden {
		if merr := os.MkdirAll("testdata", 0o750); merr != nil {
			t.Fatal(merr)
		}
		if werr := os.WriteFile(dp2GoldenPath, got, 0o600); werr != nil {
			t.Fatal(werr)
		}
	}
	want, err := os.ReadFile(dp2GoldenPath)
	if err != nil {
		t.Fatalf("%v (run go test ./pkg/ibmcloud -run TestDP2Golden -update-dp2)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is stale: run go test ./pkg/ibmcloud -run TestDP2Golden -update-dp2", dp2GoldenPath)
	}
}
