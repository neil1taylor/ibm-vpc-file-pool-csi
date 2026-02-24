package failover

import (
	"encoding/json"
	"os"
	"path/filepath"
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

func TestPlanner_EmptyDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	pvcsDir := filepath.Join(tmpDir, "pvcs")
	if err := os.MkdirAll(pvcsDir, 0755); err != nil {
		t.Fatal(err)
	}

	nfsOps := pool.NewRealNFSOperations()
	planner := NewPlanner(nfsOps)

	plan, err := planner.Plan(tmpDir)
	if err != nil {
		t.Fatalf("Plan failed: %v", err)
	}

	if len(plan.SubVolumes) != 0 {
		t.Errorf("expected 0 SubVolumes for empty directory, got %d", len(plan.SubVolumes))
	}
}

func TestPlanner_NonExistentDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	// Don't create the pvcs subdirectory.

	nfsOps := pool.NewRealNFSOperations()
	planner := NewPlanner(nfsOps)

	plan, err := planner.Plan(tmpDir)
	if err != nil {
		t.Fatalf("Plan failed: %v", err)
	}

	if len(plan.SubVolumes) != 0 {
		t.Errorf("expected 0 SubVolumes for non-existent directory, got %d", len(plan.SubVolumes))
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
