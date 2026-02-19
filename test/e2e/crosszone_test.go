//go:build e2e

package e2e

import (
	"context"
	"testing"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
)

// TestCrossZonePool verifies a pool with accessor zones populates mount target
// entries for both home and accessor zones, the PV gets server.<zone> keys,
// and a pod can mount the share.
//
// VPC access mode uses a single mount target per VPC with a FQDN that the VPC
// DNS resolves to zone-optimal IPs automatically. Both zones share the same
// FQDN/IP in pool status.
func TestCrossZonePool(t *testing.T) {
	if homeZone == accessorZone {
		t.Skip("Skipping cross-zone test: home and accessor zones are the same (single-zone cluster)")
	}
	ctx := context.Background()

	poolName := resourceName("xzone")
	scName := resourceName("xzone-sc")
	pvcName := resourceName("xzone-pvc")
	podName := resourceName("xzone-pod")

	// Build pool with accessor zone
	pool := buildPool(poolName, []v1alpha1.AccessorZone{
		{
			Zone:     accessorZone,
			SubnetID: accessorSubnetID,
		},
	})
	sc := buildStorageClass(scName, poolName)
	pvc := buildPVC(pvcName, scName, 1)
	pod := buildTestPod(podName, pvcName)

	t.Cleanup(func() {
		cleanupPod(t, ctx, podName)
		cleanupResource(t, ctx, pvc)
		waitForSubVolumesGone(t, ctx, poolName)
		cleanupResource(t, ctx, pool)
		cleanupResource(t, ctx, sc)
	})

	// Step 1: Create StorageClass and Pool
	t.Log("Creating StorageClass and cross-zone FileSharePool")
	if err := k8sClient.Create(ctx, sc); err != nil {
		t.Fatalf("Failed to create StorageClass: %v", err)
	}
	if err := k8sClient.Create(ctx, pool); err != nil {
		t.Fatalf("Failed to create FileSharePool: %v", err)
	}

	// Step 2: Wait for pool Ready
	readyPool := waitForPoolReady(t, ctx, poolName)

	// Step 3: Verify mount targets in pool status
	if len(readyPool.Status.Shares) == 0 {
		t.Fatal("Pool has no shares after becoming Ready")
	}

	share := readyPool.Status.Shares[0]
	t.Logf("Share %s: primaryIP=%s, zone=%s, mountTargets=%d",
		share.ShareID, share.MountTargetIP, share.Zone, len(share.MountTargets))

	// Must have mount target entries for both home and accessor zones.
	// In VPC mode, both zones use the same FQDN (DNS handles zone-local routing).
	if len(share.MountTargets) < 2 {
		t.Fatalf("Expected at least 2 mount target entries (home + accessor), got %d", len(share.MountTargets))
	}

	var homeServer, accessorServer string
	for _, mt := range share.MountTargets {
		t.Logf("  MountTarget: zone=%s, server=%s, ID=%s", mt.Zone, mt.MountTargetIP, mt.MountTargetID)
		switch mt.Zone {
		case homeZone:
			homeServer = mt.MountTargetIP
		case accessorZone:
			accessorServer = mt.MountTargetIP
		}
	}

	if homeServer == "" {
		t.Errorf("No mount target entry for home zone %s", homeZone)
	}
	if accessorServer == "" {
		t.Errorf("No mount target entry for accessor zone %s", accessorZone)
	}
	// VPC mode: both zones use the same VPC FQDN
	if homeServer != "" && accessorServer != "" && homeServer != accessorServer {
		t.Logf("Note: home=%s, accessor=%s (different is OK for security_group mode)", homeServer, accessorServer)
	}

	// Verify MountTargetIPForZone helper returns the correct address
	if got := share.MountTargetIPForZone(homeZone); got != homeServer {
		t.Errorf("MountTargetIPForZone(%s)=%s, want %s", homeZone, got, homeServer)
	}
	if got := share.MountTargetIPForZone(accessorZone); got != accessorServer {
		t.Errorf("MountTargetIPForZone(%s)=%s, want %s", accessorZone, got, accessorServer)
	}

	// Step 4: Create PVC and wait for binding
	t.Log("Creating PVC")
	if err := k8sClient.Create(ctx, pvc); err != nil {
		t.Fatalf("Failed to create PVC: %v", err)
	}

	boundPVC := waitForPVCBound(t, ctx, pvcName)

	// Step 5: Inspect PV for server.<zone> keys
	pvName := boundPVC.Spec.VolumeName
	pv := getPV(t, ctx, pvName)
	attrs := pvVolumeAttributes(pv)
	if attrs == nil {
		t.Fatal("PV has no CSI volumeAttributes")
	}

	t.Logf("PV volumeAttributes: %v", attrs)

	// Must have server (primary)
	if attrs["server"] == "" {
		t.Error("PV missing 'server' key")
	}

	// Must have server.<homeZone> and server.<accessorZone>
	zoneKeys := hasServerZoneKeys(attrs)
	homeKey := "server." + homeZone
	accessorKey := "server." + accessorZone

	if zoneKeys[homeKey] == "" {
		t.Errorf("PV missing %s key; zone keys found: %v", homeKey, zoneKeys)
	}
	if zoneKeys[accessorKey] == "" {
		t.Errorf("PV missing %s key; zone keys found: %v", accessorKey, zoneKeys)
	}

	// Step 6: Create pod and verify mount works
	t.Log("Creating test pod")
	if err := k8sClient.Create(ctx, pod); err != nil {
		t.Fatalf("Failed to create pod: %v", err)
	}

	waitForPodRunning(t, ctx, podName)
	t.Log("Cross-zone pool test passed: mount targets for both zones, PV has server.<zone> keys, pod Running")
}

// TestCrossZonePool_CRDValidation verifies the CRD schema accepts accessorZones fields.
func TestCrossZonePool_CRDValidation(t *testing.T) {
	ctx := context.Background()

	poolName := resourceName("crd-val")

	// Create a pool with accessor zones — should be accepted by the CRD schema
	pool := buildPool(poolName, []v1alpha1.AccessorZone{
		{
			Zone:     accessorZone,
			SubnetID: accessorSubnetID,
		},
	})

	t.Cleanup(func() {
		cleanupResource(t, ctx, pool)
	})

	if err := k8sClient.Create(ctx, pool); err != nil {
		t.Fatalf("CRD rejected accessorZones field: %v", err)
	}
	t.Log("CRD validation passed: accessorZones field accepted")

	// Re-fetch and verify the accessorZones were persisted
	fetched := getPool(t, ctx, poolName)
	if len(fetched.Spec.AccessorZones) != 1 {
		t.Fatalf("Expected 1 accessor zone, got %d", len(fetched.Spec.AccessorZones))
	}
	az := fetched.Spec.AccessorZones[0]
	if az.Zone != accessorZone {
		t.Errorf("AccessorZone.Zone=%s, want %s", az.Zone, accessorZone)
	}
	if az.SubnetID != accessorSubnetID {
		t.Errorf("AccessorZone.SubnetID=%s, want %s", az.SubnetID, accessorSubnetID)
	}

	// Verify AllAccessibleZones includes both home and accessor
	allZones := fetched.Spec.AllAccessibleZones()
	if len(allZones) != 2 {
		t.Fatalf("AllAccessibleZones()=%v, want 2 zones", allZones)
	}
	if allZones[0] != homeZone {
		t.Errorf("AllAccessibleZones()[0]=%s, want %s", allZones[0], homeZone)
	}
	if allZones[1] != accessorZone {
		t.Errorf("AllAccessibleZones()[1]=%s, want %s", allZones[1], accessorZone)
	}

	t.Log("CRD validation passed: accessorZones persisted and accessible")
}
