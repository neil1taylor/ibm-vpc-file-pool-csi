package failover

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/pool"
)

func writeTestMetadata(t *testing.T, dir string, meta *pool.SubVolumeMetadata) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".subvolume-metadata.json"), data, 0644); err != nil {
		t.Fatalf("write metadata: %v", err)
	}
}

func TestPlanner_DiscoversMetadataFiles(t *testing.T) {
	tmpDir := t.TempDir()
	pvcsDir := filepath.Join(tmpDir, "pvcs")

	now := time.Now().Truncate(time.Second)
	earlier := now.Add(-10 * time.Minute)

	// Write two SubVolume metadata files.
	writeTestMetadata(t, filepath.Join(pvcsDir, "pvc-aaa"), &pool.SubVolumeMetadata{
		SubVolumeName: "pvc-aaa",
		Spec: v1alpha1.SubVolumeSpec{
			PoolName:     "prod-pool",
			ShareID:      "share-1",
			SubPath:      "/pvcs/pvc-aaa",
			RequestedGB:  10,
			PVName:       "pv-aaa",
			PVCName:      "my-app-data",
			PVCNamespace: "production",
		},
		Labels:            map[string]string{"app": "web"},
		LastSyncTime:      now,
		ReplicationPolicy: "prod-to-dr",
	})

	writeTestMetadata(t, filepath.Join(pvcsDir, "pvc-bbb"), &pool.SubVolumeMetadata{
		SubVolumeName: "pvc-bbb",
		Spec: v1alpha1.SubVolumeSpec{
			PoolName:     "prod-pool",
			ShareID:      "share-1",
			SubPath:      "/pvcs/pvc-bbb",
			RequestedGB:  50,
			PVName:       "pv-bbb",
			PVCName:      "my-db-data",
			PVCNamespace: "production",
		},
		LastSyncTime:      earlier,
		ReplicationPolicy: "prod-to-dr",
	})

	nfsOps := pool.NewRealNFSOperations()
	planner := NewPlanner(nfsOps)

	plan, err := planner.Plan(tmpDir)
	if err != nil {
		t.Fatalf("Plan failed: %v", err)
	}

	if len(plan.SubVolumes) != 2 {
		t.Fatalf("expected 2 SubVolumes, got %d", len(plan.SubVolumes))
	}

	// RPO should be based on the oldest sync time.
	if plan.RPO < 9*time.Minute {
		t.Errorf("expected RPO >= 9m (oldest sync was 10m ago), got %s", plan.RPO)
	}

	// Verify fields parsed correctly.
	found := make(map[string]bool)
	for _, sv := range plan.SubVolumes {
		found[sv.SubVolumeName] = true
		if sv.PVCNamespace != "production" {
			t.Errorf("expected namespace 'production', got %q", sv.PVCNamespace)
		}
	}
	if !found["pvc-aaa"] || !found["pvc-bbb"] {
		t.Errorf("expected both pvc-aaa and pvc-bbb, found: %v", found)
	}
}

func TestPlanner_EmptyDirectoryIsAnError(t *testing.T) {
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, "pvcs"), 0755); err != nil {
		t.Fatal(err)
	}
	_, err := NewPlanner(pool.NewRealNFSOperations()).Plan(tmpDir)
	if err == nil {
		t.Fatal("a pvcs folder with no replicated volume must fail the Plan")
	}
	for _, want := range []string{filepath.Join(tmpDir, "pvcs"), "replicated volume", "destinationBasePath"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

func TestPlanner_MissingPvcsFolderIsAnError(t *testing.T) {
	tmpDir := t.TempDir()
	_, err := NewPlanner(pool.NewRealNFSOperations()).Plan(tmpDir)
	if err == nil {
		t.Fatal("a claim with no pvcs folder must fail the Plan")
	}
	for _, want := range []string{filepath.Join(tmpDir, "pvcs"), "destinationBasePath", "basePath"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

func TestPlanner_NamesTheBasePathWhereCopiesAre(t *testing.T) {
	tmpDir := t.TempDir()
	writeTestMetadata(t, filepath.Join(tmpDir, "repl", "pvcs", "pvc-aaa"), &pool.SubVolumeMetadata{
		SubVolumeName: "pvc-aaa", Spec: v1alpha1.SubVolumeSpec{PVCName: "a", PVCNamespace: "ns"}, LastSyncTime: time.Now(),
	})
	_, err := NewPlanner(pool.NewRealNFSOperations()).Plan(tmpDir)
	if err == nil || !strings.Contains(err.Error(), "basePath: /repl") {
		t.Fatalf("err = %v, want a hint to set basePath: /repl", err)
	}
}

func TestPlanner_ThePvcsFolderHoldingOnlyBrokenMetadataIsAnError(t *testing.T) {
	tmpDir := t.TempDir()
	bad := filepath.Join(tmpDir, "pvcs", "pvc-bad")
	if err := os.MkdirAll(bad, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, ".subvolume-metadata.json"), []byte("{nope"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPlanner(pool.NewRealNFSOperations()).Plan(tmpDir); err == nil {
		t.Fatal("no usable volume must fail the Plan")
	}
}

func TestPlanner_CorruptJSON(t *testing.T) {
	tmpDir := t.TempDir()
	pvcsDir := filepath.Join(tmpDir, "pvcs")

	// Write valid metadata.
	writeTestMetadata(t, filepath.Join(pvcsDir, "pvc-good"), &pool.SubVolumeMetadata{
		SubVolumeName:     "pvc-good",
		Spec:              v1alpha1.SubVolumeSpec{PVCName: "good", PVCNamespace: "default"},
		LastSyncTime:      time.Now(),
		ReplicationPolicy: "test",
	})

	// Write corrupt metadata.
	corruptDir := filepath.Join(pvcsDir, "pvc-bad")
	if err := os.MkdirAll(corruptDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(corruptDir, ".subvolume-metadata.json"), []byte("{invalid json"), 0644); err != nil {
		t.Fatal(err)
	}

	nfsOps := pool.NewRealNFSOperations()
	planner := NewPlanner(nfsOps)

	plan, err := planner.Plan(tmpDir)
	if err != nil {
		t.Fatalf("Plan failed: %v", err)
	}

	// Should only find the valid one.
	if len(plan.SubVolumes) != 1 {
		t.Fatalf("expected 1 SubVolume (corrupt one skipped), got %d", len(plan.SubVolumes))
	}
	if plan.SubVolumes[0].SubVolumeName != "pvc-good" {
		t.Errorf("expected pvc-good, got %s", plan.SubVolumes[0].SubVolumeName)
	}
}

func TestPlanner_NamesAPvcsBasePath(t *testing.T) {
	tmpDir := t.TempDir()
	writeTestMetadata(t, filepath.Join(tmpDir, "pvcs", "pvcs", "pvc-aaa"), &pool.SubVolumeMetadata{
		SubVolumeName: "pvc-aaa", Spec: v1alpha1.SubVolumeSpec{PVCName: "a", PVCNamespace: "ns"}, LastSyncTime: time.Now(),
	})
	_, err := NewPlanner(pool.NewRealNFSOperations()).Plan(tmpDir)
	if err == nil || !strings.Contains(err.Error(), "basePath: /pvcs") {
		t.Fatalf("err = %v, want a hint to set basePath: /pvcs", err)
	}
}
