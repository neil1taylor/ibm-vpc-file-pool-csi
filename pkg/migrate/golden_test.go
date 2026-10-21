package migrate

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden files")

// goldenPath is read by the console plugin tests too (console-plugin/src/utils/migrate-objects.test.ts).
// Both sides build the same objects from the same input, so the Go and TypeScript builders cannot drift apart.
const goldenPath = "testdata/migration-objects.golden.json"

// clean drops what a client does not set: the zero creation time and the empty status.
func clean(t *testing.T, obj any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "status")
	if meta, ok := m["metadata"].(map[string]any); ok {
		delete(meta, "creationTimestamp")
	}
	return m
}

func TestMigrationObjects_MatchGoldenFile(t *testing.T) {
	pvc := BuildTargetPVC(TargetPVCSpec{
		Name:         "data-pooled",
		Namespace:    "apps",
		SourcePVC:    "data",
		Pool:         "pool-a",
		StorageClass: "pool-a",
		Size:         resource.MustParse("200Gi"),
	})
	pod := BuildMigrationPod(MigrationPodSpec{
		Name:      "migrate-data",
		Namespace: "apps",
		SourcePVC: "data",
		TargetPVC: "data-pooled",
	})

	got, err := json.MarshalIndent(map[string]any{
		"input": map[string]any{
			"namespace":    "apps",
			"sourcePVC":    "data",
			"pool":         "pool-a",
			"storageClass": "pool-a",
			"size":         "200Gi",
		},
		"targetPVC":    clean(t, pvc),
		"migrationPod": clean(t, pod),
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')

	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden file (run: go test ./pkg/migrate -run Golden -update): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("the migration objects changed. If that is on purpose, run: go test ./pkg/migrate -run Golden -update, and change the console builders (console-plugin/src/utils/migrate-objects.ts) to match.\n got: %s\nwant: %s", got, want)
	}
}
