//go:build e2e

package e2e

import (
	"context"
	"testing"
)

// TestBasicPool verifies a pool with no accessor zones works end-to-end.
// Creates a pool, PVC, and pod. Verifies the PV has a "server" key but NO
// server.<zone> keys (backward compat). Verifies the pod can mount and write.
func TestBasicPool(t *testing.T) {
	ctx := context.Background()

	poolName := resourceName("basic")
	scName := resourceName("basic-sc")
	pvcName := resourceName("basic-pvc")
	podName := resourceName("basic-pod")

	// Build resources (no accessor zones)
	pool := buildPool(poolName, nil)
	sc := buildStorageClass(scName, poolName)
	pvc := buildPVC(pvcName, scName, 1)
	pod := buildTestPod(podName, pvcName)

	// Cleanup in reverse order. Pod must be fully gone before PVC deletion
	// proceeds (pvc-protection finalizer blocks until no pods reference it).
	t.Cleanup(func() {
		cleanupPod(t, ctx, podName)
		cleanupResource(t, ctx, pvc)
		waitForSubVolumesGone(t, ctx, poolName)
		cleanupResource(t, ctx, pool)
		cleanupResource(t, ctx, sc)
	})

	// Step 1: Create StorageClass and Pool
	t.Log("Creating StorageClass and FileSharePool")
	if err := k8sClient.Create(ctx, sc); err != nil {
		t.Fatalf("Failed to create StorageClass: %v", err)
	}
	if err := k8sClient.Create(ctx, pool); err != nil {
		t.Fatalf("Failed to create FileSharePool: %v", err)
	}

	// Step 2: Wait for pool to become Ready (VPC share creation 30-90s)
	readyPool := waitForPoolReady(t, ctx, poolName)

	// Verify pool has at least one share with a stable state
	if len(readyPool.Status.Shares) == 0 {
		t.Fatal("Pool has no shares after becoming Ready")
	}
	share := readyPool.Status.Shares[0]
	if share.State != "stable" {
		t.Errorf("Expected share state 'stable', got %q", share.State)
	}
	if share.MountTargetIP == "" {
		t.Error("Share has empty MountTargetIP")
	}
	t.Logf("Share %s: IP=%s, zone=%s, state=%s", share.ShareID, share.MountTargetIP, share.Zone, share.State)

	// Step 3: Create PVC
	t.Log("Creating PVC")
	if err := k8sClient.Create(ctx, pvc); err != nil {
		t.Fatalf("Failed to create PVC: %v", err)
	}

	boundPVC := waitForPVCBound(t, ctx, pvcName)

	// Step 4: Inspect PV volume attributes
	pvName := boundPVC.Spec.VolumeName
	pv := getPV(t, ctx, pvName)
	attrs := pvVolumeAttributes(pv)
	if attrs == nil {
		t.Fatal("PV has no CSI volumeAttributes")
	}

	// Must have "server" key with a non-empty IP
	server, ok := attrs["server"]
	if !ok || server == "" {
		t.Fatalf("PV volumeAttributes missing 'server' key, got: %v", attrs)
	}
	t.Logf("PV server=%s", server)

	// Must NOT have server.<zone> keys (no accessor zones configured)
	zoneKeys := hasServerZoneKeys(attrs)
	if len(zoneKeys) > 0 {
		t.Errorf("Basic pool PV should NOT have server.<zone> keys, but found: %v", zoneKeys)
	}

	// Verify other expected attributes
	if attrs["pool"] != poolName {
		t.Errorf("Expected pool=%s in volumeAttributes, got %s", poolName, attrs["pool"])
	}
	if attrs["shareID"] == "" {
		t.Error("PV volumeAttributes missing 'shareID'")
	}
	if attrs["subDir"] == "" {
		t.Error("PV volumeAttributes missing 'subDir'")
	}

	// Step 5: Verify SubVolume CR was created
	svs := getSubVolumesForPool(t, ctx, poolName)
	if len(svs) != 1 {
		t.Fatalf("Expected 1 SubVolume for pool %s, got %d", poolName, len(svs))
	}
	sv := svs[0]
	// PVCName may be empty because CSI provisioner v5.x doesn't inject
	// csi.storage.k8s.io/pvc/name into CreateVolume parameters.
	if sv.Spec.PVCName != "" && sv.Spec.PVCName != pvcName {
		t.Errorf("SubVolume PVCName=%s, want %s or empty", sv.Spec.PVCName, pvcName)
	}
	if sv.Status.Phase != "Bound" {
		t.Errorf("SubVolume phase=%s, want Bound", sv.Status.Phase)
	}
	t.Logf("SubVolume %s: shareID=%s, subPath=%s, pvcName=%s, phase=%s",
		sv.Name, sv.Spec.ShareID, sv.Spec.SubPath, sv.Spec.PVCName, sv.Status.Phase)

	// Step 6: Create pod and verify mount
	t.Log("Creating test pod")
	if err := k8sClient.Create(ctx, pod); err != nil {
		t.Fatalf("Failed to create pod: %v", err)
	}

	waitForPodRunning(t, ctx, podName)
	t.Log("Basic pool test passed: pool Ready, PVC Bound, pod Running, no server.<zone> keys")
}
