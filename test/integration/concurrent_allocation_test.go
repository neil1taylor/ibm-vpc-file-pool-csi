//go:build integration

package integration

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------------
// Concurrent Allocation Tests: verify that multiple goroutines issuing
// CreateVolume simultaneously don't produce double-allocations or data
// corruption in the pool manager.
// ---------------------------------------------------------------------------

func TestConcurrentAllocation_NoDoubleAllocation(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	// Pool with a single share of 1000 GB
	pool := newTestPool("concurrent-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 0, 0),
	)
	h.k8sClient.addPool(pool)

	concurrency := 20
	var wg sync.WaitGroup
	wg.Add(concurrency)

	results := make([]string, concurrency)
	errors := make([]error, concurrency)

	for i := 0; i < concurrency; i++ {
		go func(idx int) {
			defer wg.Done()
			pvName := fmt.Sprintf("pvc-a1b2c3d4-5678-90ab-cdef-%012d", idx)
			resp, err := h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "concurrent-pool", 10))
			if err != nil {
				errors[idx] = err
				return
			}
			results[idx] = resp.GetVolume().VolumeId
		}(i)
	}

	wg.Wait()

	// Count successes
	successCount := 0
	for i := 0; i < concurrency; i++ {
		if errors[i] != nil {
			t.Logf("goroutine %d error: %v", i, errors[i])
		} else {
			successCount++
		}
	}

	// All 20 should succeed (1000 GB / 10 GB = 100 max, 20 fits easily)
	if successCount != concurrency {
		t.Errorf("expected %d successful allocations, got %d", concurrency, successCount)
	}

	// Verify unique volume IDs (no double allocation)
	seen := make(map[string]bool)
	for i := 0; i < concurrency; i++ {
		if results[i] == "" {
			continue
		}
		if seen[results[i]] {
			t.Errorf("duplicate volume ID: %s", results[i])
		}
		seen[results[i]] = true
	}

	// Verify exactly N SubVolumes
	if h.k8sClient.subVolumeCount() != concurrency {
		t.Errorf("expected %d SubVolumes, got %d", concurrency, h.k8sClient.subVolumeCount())
	}

	// Verify pool status: total allocated = 20 * 10 = 200
	updatedPool := h.k8sClient.getPool("concurrent-pool")
	if updatedPool.Status.TotalAllocatedGB != int64(concurrency)*10 {
		t.Errorf("expected TotalAllocatedGB=%d, got %d", concurrency*10, updatedPool.Status.TotalAllocatedGB)
	}
	if updatedPool.Status.TotalPVCCount != int32(concurrency) {
		t.Errorf("expected TotalPVCCount=%d, got %d", concurrency, updatedPool.Status.TotalPVCCount)
	}
}

func TestConcurrentAllocation_SpreadDistribution(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	// Pool with 4 shares, spread strategy
	pool := newTestPool("spread-concurrent", "spread", 500,
		newStableShare("share-1", "s1", 500, 0, 0),
		newStableShare("share-2", "s2", 500, 0, 0),
		newStableShare("share-3", "s3", 500, 0, 0),
		newStableShare("share-4", "s4", 500, 0, 0),
	)
	h.k8sClient.addPool(pool)

	// Allocate 20 volumes of 10 GB each concurrently
	concurrency := 20
	var wg sync.WaitGroup
	wg.Add(concurrency)

	for i := 0; i < concurrency; i++ {
		go func(idx int) {
			defer wg.Done()
			pvName := fmt.Sprintf("pvc-a1b2c3d4-5678-90ab-cdef-%012d", idx)
			_, err := h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "spread-concurrent", 10))
			if err != nil {
				t.Errorf("CreateVolume %d failed: %v", idx, err)
			}
		}(i)
	}

	wg.Wait()

	// All 20 should be allocated
	if h.k8sClient.subVolumeCount() != concurrency {
		t.Errorf("expected %d SubVolumes, got %d", concurrency, h.k8sClient.subVolumeCount())
	}

	// With spread strategy, shares should have fairly even distribution.
	// Each share should have some allocations (at least 1).
	updatedPool := h.k8sClient.getPool("spread-concurrent")
	for _, share := range updatedPool.Status.Shares {
		if share.PVCCount == 0 {
			t.Errorf("spread strategy: share %s has 0 PVCs, expected distribution", share.ShareID)
		}
	}

	// Total allocated should be 20 * 10 = 200
	if updatedPool.Status.TotalAllocatedGB != 200 {
		t.Errorf("expected TotalAllocatedGB=200, got %d", updatedPool.Status.TotalAllocatedGB)
	}
}

func TestConcurrentAllocation_ExhaustsCapacity(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	// Pool with small capacity: 100 GB total, no auto-expand
	pool := newTestPool("exhaust-pool", "spread", 100,
		newStableShare("share-1", "s1", 100, 0, 0),
	)
	pool.Spec.AutoExpand = false
	pool.Spec.MaxShares = 1
	h.k8sClient.addPool(pool)

	// Try to allocate 15 volumes of 10 GB each (150 GB > 100 GB capacity)
	concurrency := 15
	var wg sync.WaitGroup
	wg.Add(concurrency)

	successCount := int32(0)
	var mu sync.Mutex

	for i := 0; i < concurrency; i++ {
		go func(idx int) {
			defer wg.Done()
			pvName := fmt.Sprintf("pvc-a1b2c3d4-5678-90ab-cdef-%012d", idx)
			_, err := h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "exhaust-pool", 10))
			if err == nil {
				mu.Lock()
				successCount++
				mu.Unlock()
			}
		}(i)
	}

	wg.Wait()

	// Exactly 10 should succeed (100 GB / 10 GB)
	if successCount != 10 {
		t.Errorf("expected 10 successful allocations (100 GB / 10 GB), got %d", successCount)
	}

	// Verify pool is at capacity
	updatedPool := h.k8sClient.getPool("exhaust-pool")
	if updatedPool.Status.TotalAllocatedGB != 100 {
		t.Errorf("expected TotalAllocatedGB=100 (full), got %d", updatedPool.Status.TotalAllocatedGB)
	}
}

func TestConcurrentAllocation_WithAutoExpand(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	// Pool with 100 GB, auto-expand enabled, max 5 shares of 100 GB each
	pool := newTestPool("expand-pool", "spread", 100,
		newStableShare("share-1", "s1", 100, 0, 0),
	)
	pool.Spec.AutoExpand = true
	pool.Spec.MaxShares = 5
	h.k8sClient.addPool(pool)

	// Try to allocate 15 volumes of 10 GB (150 GB needed > 100 GB initial)
	// Auto-expand should create new shares
	concurrency := 15
	var wg sync.WaitGroup
	wg.Add(concurrency)

	successCount := int32(0)
	var mu sync.Mutex

	for i := 0; i < concurrency; i++ {
		go func(idx int) {
			defer wg.Done()
			pvName := fmt.Sprintf("pvc-a1b2c3d4-5678-90ab-cdef-%012d", idx)
			_, err := h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "expand-pool", 10))
			if err == nil {
				mu.Lock()
				successCount++
				mu.Unlock()
			}
		}(i)
	}

	wg.Wait()

	// All 15 should succeed because auto-expand creates new shares
	if successCount != int32(concurrency) {
		t.Errorf("expected %d successful allocations with auto-expand, got %d", concurrency, successCount)
	}

	// VPC should have created at least 1 new share
	if h.vpcClient.CreateCalls == 0 {
		t.Error("expected at least 1 VPC create call for auto-expansion")
	}
}

func TestConcurrentAllocation_AllocateAndDeallocateSimultaneously(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	// Set up pool with a share
	pool := newTestPool("churn-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 0, 0),
	)
	h.k8sClient.addPool(pool)

	// Pre-allocate 10 volumes to deallocate
	preAllocated := make([]string, 10)
	for i := 0; i < 10; i++ {
		pvName := fmt.Sprintf("pvc-a1b2c3d4-5678-90ab-cdef-bbb%09d", i)
		resp, err := h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "churn-pool", 5))
		if err != nil {
			t.Fatalf("pre-allocation %d failed: %v", i, err)
		}
		preAllocated[i] = resp.GetVolume().VolumeId
	}

	// Now simultaneously allocate 10 new and deallocate 10 existing
	var wg sync.WaitGroup
	wg.Add(20)

	allocErrors := make([]error, 10)
	deallocErrors := make([]error, 10)

	// Allocate 10 new volumes
	for i := 0; i < 10; i++ {
		go func(idx int) {
			defer wg.Done()
			pvName := fmt.Sprintf("pvc-a1b2c3d4-5678-90ab-cdef-aaa%09d", idx)
			_, err := h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "churn-pool", 5))
			allocErrors[idx] = err
		}(i)
	}

	// Deallocate 10 pre-existing volumes
	for i := 0; i < 10; i++ {
		go func(idx int) {
			defer wg.Done()
			_, err := h.driver.DeleteVolume(ctx, deleteVolumeRequest(preAllocated[idx]))
			deallocErrors[idx] = err
		}(i)
	}

	wg.Wait()

	// All allocations should succeed
	for i, err := range allocErrors {
		if err != nil {
			t.Errorf("allocation %d failed: %v", i, err)
		}
	}

	// All deallocations should succeed
	for i, err := range deallocErrors {
		if err != nil {
			t.Errorf("deallocation %d failed: %v", i, err)
		}
	}

	// Should end up with exactly 10 SubVolumes (10 new - 10 deleted)
	if h.k8sClient.subVolumeCount() != 10 {
		t.Errorf("expected 10 SubVolumes after churn, got %d", h.k8sClient.subVolumeCount())
	}
}

func TestConcurrentAllocation_SamePVNameIdempotent(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pool := newTestPool("same-pv-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 0, 0),
	)
	h.k8sClient.addPool(pool)

	// Multiple goroutines try to create the same PV name
	pvName := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	concurrency := 10
	var wg sync.WaitGroup
	wg.Add(concurrency)

	volumeIDs := make([]string, concurrency)
	errs := make([]error, concurrency)

	for i := 0; i < concurrency; i++ {
		go func(idx int) {
			defer wg.Done()
			resp, err := h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "same-pv-pool", 10))
			errs[idx] = err
			if err == nil {
				volumeIDs[idx] = resp.GetVolume().VolumeId
			}
		}(i)
	}

	wg.Wait()

	// Due to the pool manager's mutex, the first goroutine creates the SubVolume,
	// and subsequent ones hit the idempotency path. Some may fail due to the
	// fake K8s client's "already exists" error on CreateSubVolume, but the
	// CSI driver should handle this gracefully via idempotency check.
	//
	// At least 1 should succeed (the first to acquire the lock).
	successCount := 0
	var successVolumeID string
	for i := 0; i < concurrency; i++ {
		if errs[i] == nil {
			successCount++
			if successVolumeID == "" {
				successVolumeID = volumeIDs[i]
			}
		}
	}

	if successCount == 0 {
		t.Fatal("expected at least 1 successful allocation for same PV name")
	}

	// All successful results should return the same volume ID
	for i := 0; i < concurrency; i++ {
		if errs[i] == nil && volumeIDs[i] != successVolumeID {
			t.Errorf("goroutine %d returned different volume ID: %q vs %q", i, volumeIDs[i], successVolumeID)
		}
	}

	// Should have exactly 1 SubVolume regardless of concurrency
	if h.k8sClient.subVolumeCount() != 1 {
		t.Errorf("expected 1 SubVolume (idempotent), got %d", h.k8sClient.subVolumeCount())
	}
}
