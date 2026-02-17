package pool

import (
	"context"
	"fmt"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/ibmcloud"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/ibmcloud/fake"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/metrics"
	dto "github.com/prometheus/client_model/go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
)

func newReconciler(k8s *fakeK8sClient, vpc *fake.FakeVPCClient) *FileSharePoolReconciler {
	return NewFileSharePoolReconciler(k8s, vpc)
}

func reconcilePool(r *FileSharePoolReconciler, name string) (ctrl.Result, error) {
	return r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name},
	})
}

// ---------------------------------------------------------------------------
// 1. Pool Not Found
// ---------------------------------------------------------------------------

func TestReconcile_PoolNotFound(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	r := newReconciler(k, vpc)

	result, err := reconcilePool(r, "nonexistent")
	if err != nil {
		t.Fatalf("expected no error for deleted pool, got: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue, got RequeueAfter=%v", result.RequeueAfter)
	}
}

// ---------------------------------------------------------------------------
// 2. Adds Finalizer
// ---------------------------------------------------------------------------

func TestReconcile_AddsFinalizer(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()

	pool := newTestPool("test-pool", "spread", 1000)
	pool.Spec.InitialShares = 0 // skip initial provisioning to focus on finalizer
	k.addPool(pool)

	r := newReconciler(k, vpc)
	_, err := reconcilePool(r, "test-pool")
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	updated := k.getPool("test-pool")
	found := false
	for _, f := range updated.Finalizers {
		if f == FinalizerName {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected finalizer to be added")
	}
}

// ---------------------------------------------------------------------------
// 3. Initial Share Provisioning
// ---------------------------------------------------------------------------

func TestReconcile_InitialShareProvisioning(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()

	pool := newTestPool("test-pool", "spread", 1000)
	pool.Spec.InitialShares = 3
	pool.Status.Phase = "" // empty triggers initial provisioning
	k.addPool(pool)

	r := newReconciler(k, vpc)
	result, err := reconcilePool(r, "test-pool")
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	if result.RequeueAfter != ReconcileInterval {
		t.Errorf("expected RequeueAfter=%v, got %v", ReconcileInterval, result.RequeueAfter)
	}

	if vpc.CreateCalls != 3 {
		t.Errorf("expected 3 VPC create calls, got %d", vpc.CreateCalls)
	}

	updated := k.getPool("test-pool")
	if len(updated.Status.Shares) != 3 {
		t.Errorf("expected 3 shares in status, got %d", len(updated.Status.Shares))
	}
	if updated.Status.ShareCount != 3 {
		t.Errorf("expected ShareCount=3, got %d", updated.Status.ShareCount)
	}
}

// ---------------------------------------------------------------------------
// 4. Health Check — Degraded Share Marked Draining
// ---------------------------------------------------------------------------

func TestReconcile_ShareHealthCheck_DegradedMarkedDraining(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()

	// Create a share in VPC first
	shareInfo, _ := vpc.CreateFileShare(context.Background(), ibmcloudInput("test-pool", 1000))
	vpc.SetShareState(shareInfo.ID, "failed")

	pool := newTestPool("test-pool", "spread", 1000,
		v1alpha1.PoolShareStatus{
			ShareID:       shareInfo.ID,
			ShareName:     shareInfo.Name,
			MountTargetIP: "10.240.0.1",
			TotalGB:       1000,
			State:         "stable",
			Zone:          "us-south-1",
		},
	)
	pool.Spec.InitialShares = 1 // already have 1
	k.addPool(pool)

	r := newReconciler(k, vpc)
	_, err := reconcilePool(r, "test-pool")
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	updated := k.getPool("test-pool")
	if updated.Status.Shares[0].State != "draining" {
		t.Errorf("expected share state 'draining', got %q", updated.Status.Shares[0].State)
	}
	if updated.Status.Phase != "Degraded" {
		t.Errorf("expected phase 'Degraded', got %q", updated.Status.Phase)
	}
}

// ---------------------------------------------------------------------------
// 5. Proactive Expansion
// ---------------------------------------------------------------------------

func TestReconcile_ProactiveExpansion(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()

	// One share at 90% utilization (900/1000), above default 80% threshold
	pool := newTestPool("test-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 900, 5),
	)
	pool.Spec.ExpandThresholdPercent = 80
	pool.Spec.AutoExpand = true
	pool.Spec.MaxShares = 5
	pool.Spec.InitialShares = 1 // already have 1 stable share

	// Add matching SubVolumes so metrics reconciliation maintains the allocation
	sv := newTestSubVolume("pvc-sub1", "test-pool", "share-1", 900)
	k.addSubVolume(sv)
	k.addPool(pool)

	r := newReconciler(k, vpc)
	_, err := reconcilePool(r, "test-pool")
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	// Should have created 1 new share via VPC for proactive expansion
	if vpc.CreateCalls != 1 {
		t.Errorf("expected 1 VPC create call for proactive expansion, got %d", vpc.CreateCalls)
	}

	updated := k.getPool("test-pool")
	if len(updated.Status.Shares) != 2 {
		t.Errorf("expected 2 shares after expansion, got %d", len(updated.Status.Shares))
	}
}

// ---------------------------------------------------------------------------
// 6. Proactive Expansion — Below Threshold
// ---------------------------------------------------------------------------

func TestReconcile_ProactiveExpansion_BelowThreshold(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()

	// One share at 50% utilization (500/1000), below 80% threshold
	pool := newTestPool("test-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 500, 3),
	)
	pool.Spec.ExpandThresholdPercent = 80
	pool.Spec.AutoExpand = true
	pool.Spec.MaxShares = 5
	pool.Spec.InitialShares = 1

	// SubVolumes matching the allocation
	sv := newTestSubVolume("pvc-sub1", "test-pool", "share-1", 500)
	k.addSubVolume(sv)
	k.addPool(pool)

	r := newReconciler(k, vpc)
	_, err := reconcilePool(r, "test-pool")
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	// No expansion should have occurred
	if vpc.CreateCalls != 0 {
		t.Errorf("expected 0 VPC create calls, got %d", vpc.CreateCalls)
	}

	updated := k.getPool("test-pool")
	if len(updated.Status.Shares) != 1 {
		t.Errorf("expected 1 share (no expansion), got %d", len(updated.Status.Shares))
	}
}

// ---------------------------------------------------------------------------
// 7. Metrics Reconciliation
// ---------------------------------------------------------------------------

func TestReconcile_MetricsReconciliation(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()

	// Pool status has drifted values
	pool := newTestPool("test-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 999, 99), // intentionally wrong
	)
	pool.Spec.InitialShares = 1
	k.addPool(pool)

	// Actual SubVolumes show different totals
	k.addSubVolume(newTestSubVolume("pvc-1", "test-pool", "share-1", 10))
	k.addSubVolume(newTestSubVolume("pvc-2", "test-pool", "share-1", 20))

	r := newReconciler(k, vpc)
	_, err := reconcilePool(r, "test-pool")
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	updated := k.getPool("test-pool")
	// Metrics should be recalculated from SubVolume CRs
	if updated.Status.Shares[0].AllocatedGB != 30 {
		t.Errorf("expected share AllocatedGB=30 (10+20), got %d", updated.Status.Shares[0].AllocatedGB)
	}
	if updated.Status.Shares[0].PVCCount != 2 {
		t.Errorf("expected share PVCCount=2, got %d", updated.Status.Shares[0].PVCCount)
	}
	if updated.Status.TotalAllocatedGB != 30 {
		t.Errorf("expected TotalAllocatedGB=30, got %d", updated.Status.TotalAllocatedGB)
	}
	if updated.Status.TotalPVCCount != 2 {
		t.Errorf("expected TotalPVCCount=2, got %d", updated.Status.TotalPVCCount)
	}
}

// ---------------------------------------------------------------------------
// 8. Deletion Blocked by Active SubVolumes
// ---------------------------------------------------------------------------

func TestReconcile_DeletionBlocked_ActiveSubVolumes(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()

	now := metav1.Now()
	pool := newTestPool("test-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 10, 1),
	)
	pool.DeletionTimestamp = &now
	pool.Finalizers = []string{FinalizerName}
	k.addPool(pool)

	k.addSubVolume(newTestSubVolume("pvc-1", "test-pool", "share-1", 10))

	r := newReconciler(k, vpc)
	result, err := reconcilePool(r, "test-pool")
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	// Should requeue (deletion is blocked)
	if result.RequeueAfter == 0 {
		t.Error("expected requeue when deletion is blocked")
	}

	// Finalizer should still be present
	updated := k.getPool("test-pool")
	if !hasFinalizer(updated) {
		t.Error("expected finalizer to be retained while SubVolumes exist")
	}

	// VPC shares should NOT be deleted
	if vpc.DeleteCalls != 0 {
		t.Errorf("expected 0 VPC delete calls, got %d", vpc.DeleteCalls)
	}
}

// ---------------------------------------------------------------------------
// 9. Deletion with No SubVolumes
// ---------------------------------------------------------------------------

func TestReconcile_Deletion_NoSubVolumes(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()

	// Pre-create the VPC share
	shareInfo, _ := vpc.CreateFileShare(context.Background(), ibmcloudInput("test-pool", 1000))

	now := metav1.Now()
	pool := newTestPool("test-pool", "spread", 1000,
		v1alpha1.PoolShareStatus{
			ShareID:   shareInfo.ID,
			ShareName: shareInfo.Name,
			TotalGB:   1000,
			State:     "stable",
			Zone:      "us-south-1",
		},
	)
	pool.DeletionTimestamp = &now
	pool.Finalizers = []string{FinalizerName}
	k.addPool(pool)

	r := newReconciler(k, vpc)
	result, err := reconcilePool(r, "test-pool")
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	// Should not requeue (deletion complete)
	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue after successful deletion, got %v", result.RequeueAfter)
	}

	// VPC share should be deleted
	if vpc.DeleteCalls != 1 {
		t.Errorf("expected 1 VPC delete call, got %d", vpc.DeleteCalls)
	}

	// Finalizer should be removed
	updated := k.getPool("test-pool")
	if hasFinalizer(updated) {
		t.Error("expected finalizer to be removed after deletion")
	}
}

// ---------------------------------------------------------------------------
// 10. Determine Phase (table-driven)
// ---------------------------------------------------------------------------

func TestDeterminePhase(t *testing.T) {
	tests := []struct {
		name     string
		pool     *v1alpha1.FileSharePool
		expected string
	}{
		{
			name:     "no shares → Initializing",
			pool:     newTestPool("p", "spread", 1000),
			expected: "Initializing",
		},
		{
			name: "all stable, capacity available → Ready",
			pool: newTestPool("p", "spread", 1000,
				newStableShare("s1", "s1", 1000, 500, 3),
			),
			expected: "Ready",
		},
		{
			name: "one creating → Expanding",
			pool: func() *v1alpha1.FileSharePool {
				p := newTestPool("p", "spread", 1000,
					newStableShare("s1", "s1", 1000, 500, 3),
					v1alpha1.PoolShareStatus{ShareID: "s2", TotalGB: 1000, State: "creating"},
				)
				return p
			}(),
			expected: "Expanding",
		},
		{
			name: "one draining → Degraded",
			pool: func() *v1alpha1.FileSharePool {
				p := newTestPool("p", "spread", 1000,
					newStableShare("s1", "s1", 1000, 500, 3),
					v1alpha1.PoolShareStatus{ShareID: "s2", TotalGB: 1000, State: "draining"},
				)
				return p
			}(),
			expected: "Degraded",
		},
		{
			name: "all full at maxShares → Full",
			pool: func() *v1alpha1.FileSharePool {
				p := newTestPool("p", "spread", 1000,
					newStableShare("s1", "s1", 1000, 1000, 10),
					newStableShare("s2", "s2", 1000, 1000, 10),
				)
				p.Spec.MaxShares = 2
				return p
			}(),
			expected: "Full",
		},
		{
			name: "at maxShares but not full → Ready",
			pool: func() *v1alpha1.FileSharePool {
				p := newTestPool("p", "spread", 1000,
					newStableShare("s1", "s1", 1000, 500, 3),
					newStableShare("s2", "s2", 1000, 1000, 10),
				)
				p.Spec.MaxShares = 2
				return p
			}(),
			expected: "Ready",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := determinePhase(tt.pool)
			if got != tt.expected {
				t.Errorf("determinePhase() = %q, want %q", got, tt.expected)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 11. Sets LastReconcileTime
// ---------------------------------------------------------------------------

func TestReconcile_SetsLastReconcileTime(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()

	pool := newTestPool("test-pool", "spread", 1000)
	pool.Spec.InitialShares = 0
	k.addPool(pool)

	r := newReconciler(k, vpc)
	_, err := reconcilePool(r, "test-pool")
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	updated := k.getPool("test-pool")
	if updated.Status.LastReconcileTime == nil {
		t.Fatal("expected LastReconcileTime to be set")
	}

	// Should be recent (within last 5 seconds)
	if time.Since(updated.Status.LastReconcileTime.Time) > 5*time.Second {
		t.Errorf("LastReconcileTime is too old: %v", updated.Status.LastReconcileTime.Time)
	}
}

// ---------------------------------------------------------------------------
// 12. Creating Share Becomes Stable
// ---------------------------------------------------------------------------

func TestReconcile_CreatingShareBecomesStable(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()

	// Create a share in VPC (starts as stable in fake, but we mark pool status as "creating")
	shareInfo, _ := vpc.CreateFileShare(context.Background(), ibmcloudInput("test-pool", 1000))

	pool := newTestPool("test-pool", "spread", 1000,
		v1alpha1.PoolShareStatus{
			ShareID:   shareInfo.ID,
			ShareName: shareInfo.Name,
			TotalGB:   1000,
			State:     "creating", // pool thinks it's still creating
			Zone:      "us-south-1",
		},
	)
	pool.Spec.InitialShares = 1
	k.addPool(pool)

	r := newReconciler(k, vpc)
	_, err := reconcilePool(r, "test-pool")
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	updated := k.getPool("test-pool")
	if updated.Status.Shares[0].State != "stable" {
		t.Errorf("expected share state 'stable', got %q", updated.Status.Shares[0].State)
	}
	// Mount target IP should be backfilled
	if updated.Status.Shares[0].MountTargetIP == "" {
		t.Error("expected MountTargetIP to be backfilled")
	}
}

// ---------------------------------------------------------------------------
// 13. Initial Provisioning VPC Error
// ---------------------------------------------------------------------------

func TestReconcile_InitialProvisioning_VPCError(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	vpc.CreateErr = fmt.Errorf("VPC API unavailable")

	pool := newTestPool("test-pool", "spread", 1000)
	pool.Spec.InitialShares = 2
	pool.Status.Phase = ""
	k.addPool(pool)

	r := newReconciler(k, vpc)
	result, err := reconcilePool(r, "test-pool")
	if err != nil {
		t.Fatalf("expected nil error (soft failure), got: %v", err)
	}

	// Should requeue after 15s
	if result.RequeueAfter != 15*time.Second {
		t.Errorf("expected RequeueAfter=15s, got %v", result.RequeueAfter)
	}

	// Phase should be Initializing
	updated := k.getPool("test-pool")
	if updated.Status.Phase != "Initializing" {
		t.Errorf("expected phase 'Initializing', got %q", updated.Status.Phase)
	}

	// No shares should have been created
	if len(updated.Status.Shares) != 0 {
		t.Errorf("expected 0 shares, got %d", len(updated.Status.Shares))
	}
}

// ---------------------------------------------------------------------------
// 14. Prometheus Pool Gauge Metrics
// ---------------------------------------------------------------------------

func TestReconcile_EmitsPrometheusGauges(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()

	pool := newTestPool("gauge-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 0, 0),
		newStableShare("share-2", "s2", 2000, 0, 0),
	)
	pool.Spec.InitialShares = 2
	k.addPool(pool)

	// Add SubVolumes totalling 150 GB across 3 PVCs
	k.addSubVolume(newTestSubVolume("pvc-g1", "gauge-pool", "share-1", 50))
	k.addSubVolume(newTestSubVolume("pvc-g2", "gauge-pool", "share-1", 30))
	k.addSubVolume(newTestSubVolume("pvc-g3", "gauge-pool", "share-2", 70))

	r := newReconciler(k, vpc)
	_, err := reconcilePool(r, "gauge-pool")
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	// Check capacity gauge: 1000 + 2000 = 3000
	var m dto.Metric
	if err := metrics.PoolCapacityGB.WithLabelValues("gauge-pool").Write(&m); err != nil {
		t.Fatalf("failed to read PoolCapacityGB: %v", err)
	}
	if *m.Gauge.Value != 3000 {
		t.Errorf("expected PoolCapacityGB=3000, got %f", *m.Gauge.Value)
	}

	// Check allocated gauge: 50 + 30 + 70 = 150
	if err := metrics.PoolAllocatedGB.WithLabelValues("gauge-pool").Write(&m); err != nil {
		t.Fatalf("failed to read PoolAllocatedGB: %v", err)
	}
	if *m.Gauge.Value != 150 {
		t.Errorf("expected PoolAllocatedGB=150, got %f", *m.Gauge.Value)
	}

	// Check share count: 2
	if err := metrics.PoolShareCount.WithLabelValues("gauge-pool").Write(&m); err != nil {
		t.Fatalf("failed to read PoolShareCount: %v", err)
	}
	if *m.Gauge.Value != 2 {
		t.Errorf("expected PoolShareCount=2, got %f", *m.Gauge.Value)
	}

	// Check PVC count: 3
	if err := metrics.PoolPVCCount.WithLabelValues("gauge-pool").Write(&m); err != nil {
		t.Fatalf("failed to read PoolPVCCount: %v", err)
	}
	if *m.Gauge.Value != 3 {
		t.Errorf("expected PoolPVCCount=3, got %f", *m.Gauge.Value)
	}
}

// ---------------------------------------------------------------------------
// 15. Initial Provisioning with Tiers
// ---------------------------------------------------------------------------

func TestReconcile_InitialProvisioning_WithTiers(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()

	pool := newTestPoolWithTiers("test-pool", "spread",
		[]v1alpha1.ShareTier{
			{Name: "standard", Profile: "dp2", ShareSizeGB: 1000, MaxShares: 5, InitialShares: 2},
			{Name: "premium", Profile: "custom", ShareSizeGB: 500, MaxShares: 3, InitialShares: 1},
		},
	)
	pool.Status.Phase = "" // triggers initial provisioning
	k.addPool(pool)

	r := newReconciler(k, vpc)
	result, err := reconcilePool(r, "test-pool")
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	if result.RequeueAfter != ReconcileInterval {
		t.Errorf("expected RequeueAfter=%v, got %v", ReconcileInterval, result.RequeueAfter)
	}

	// 2 standard + 1 premium = 3 total
	if vpc.CreateCalls != 3 {
		t.Errorf("expected 3 VPC create calls, got %d", vpc.CreateCalls)
	}

	updated := k.getPool("test-pool")
	if len(updated.Status.Shares) != 3 {
		t.Errorf("expected 3 shares in status, got %d", len(updated.Status.Shares))
	}

	// Count by tier
	tierCounts := map[string]int{}
	for _, s := range updated.Status.Shares {
		tierCounts[s.Tier]++
	}
	if tierCounts["standard"] != 2 {
		t.Errorf("expected 2 standard shares, got %d", tierCounts["standard"])
	}
	if tierCounts["premium"] != 1 {
		t.Errorf("expected 1 premium share, got %d", tierCounts["premium"])
	}
}

// ---------------------------------------------------------------------------
// 16. Proactive Expansion Per Tier
// ---------------------------------------------------------------------------

func TestReconcile_ProactiveExpansion_PerTier(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()

	pool := newTestPoolWithTiers("test-pool", "spread",
		[]v1alpha1.ShareTier{
			{Name: "standard", Profile: "dp2", ShareSizeGB: 1000, MaxShares: 5, InitialShares: 1},
			{Name: "premium", Profile: "custom", ShareSizeGB: 500, MaxShares: 3, InitialShares: 1},
		},
		// standard at 50% — below threshold
		newStableShareWithTier("s1", "s1", 1000, 500, 3, "standard"),
		// premium at 90% — above threshold
		newStableShareWithTier("s2", "s2", 500, 450, 5, "premium"),
	)
	pool.Spec.ExpandThresholdPercent = 80
	pool.Spec.AutoExpand = true
	pool.Spec.InitialShares = 1
	k.addPool(pool)

	// Add SubVolumes matching allocations
	k.addSubVolume(newTestSubVolume("pvc-s1", "test-pool", "s1", 500))
	k.addSubVolume(newTestSubVolume("pvc-s2", "test-pool", "s2", 450))

	r := newReconciler(k, vpc)
	_, err := reconcilePool(r, "test-pool")
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	// Should create 1 new share for premium tier only
	if vpc.CreateCalls != 1 {
		t.Errorf("expected 1 VPC create call for premium tier expansion, got %d", vpc.CreateCalls)
	}

	updated := k.getPool("test-pool")
	if len(updated.Status.Shares) != 3 {
		t.Errorf("expected 3 shares (2 original + 1 new premium), got %d", len(updated.Status.Shares))
	}

	// New share should be premium tier
	newShare := updated.Status.Shares[2]
	if newShare.Tier != "premium" {
		t.Errorf("expected new share tier 'premium', got %q", newShare.Tier)
	}
}

// ---------------------------------------------------------------------------
// 17. Ensure Accessor Mount Targets
// ---------------------------------------------------------------------------

func TestReconcile_EnsureAccessorMountTargets(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()

	// Create a share in VPC first
	shareInfo, _ := vpc.CreateFileShare(context.Background(), ibmcloudInput("test-pool", 1000))

	pool := newTestPool("test-pool", "spread", 1000,
		v1alpha1.PoolShareStatus{
			ShareID:       shareInfo.ID,
			ShareName:     shareInfo.Name,
			MountTargetIP: shareInfo.MountTargets[0].IPAddress,
			MountTargetID: shareInfo.MountTargets[0].ID,
			TotalGB:       1000,
			State:         "stable",
			Zone:          "us-south-1",
			MountTargets: []v1alpha1.ZoneMountTarget{
				{Zone: "us-south-1", MountTargetID: shareInfo.MountTargets[0].ID, MountTargetIP: shareInfo.MountTargets[0].IPAddress},
			},
		},
	)
	pool.Spec.InitialShares = 1
	pool.Spec.AccessorZones = []v1alpha1.AccessorZone{
		{Zone: "us-south-2", SubnetID: "subnet-2"},
		{Zone: "us-south-3", SubnetID: "subnet-3"},
	}
	k.addPool(pool)

	r := newReconciler(k, vpc)
	r.SetVPCConfig("vpc-1", "subnet-1", "rg-1")
	_, err := reconcilePool(r, "test-pool")
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	// VPC mode: no VPC API calls to create additional mount targets (one per VPC limit).
	// Accessor zones are populated using the same VPC FQDN/IP.
	if vpc.CreateMountTargetCalls != 0 {
		t.Errorf("expected 0 CreateMountTarget calls (VPC mode uses same FQDN), got %d", vpc.CreateMountTargetCalls)
	}

	updated := k.getPool("test-pool")
	share := updated.Status.Shares[0]
	if len(share.MountTargets) != 3 {
		t.Errorf("expected 3 mount targets (home + 2 accessor), got %d", len(share.MountTargets))
	}

	// All zones should use the same VPC-mode server address
	homeIP := share.MountTargetIP
	for _, mt := range share.MountTargets {
		if mt.MountTargetIP != homeIP {
			t.Errorf("mount target zone %s has IP %s, expected same as home %s (VPC mode)", mt.Zone, mt.MountTargetIP, homeIP)
		}
	}
}

func TestReconcile_EnsureAccessorMountTargets_AlreadyExists(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()

	shareInfo, _ := vpc.CreateFileShare(context.Background(), ibmcloudInput("test-pool", 1000))

	pool := newTestPool("test-pool", "spread", 1000,
		v1alpha1.PoolShareStatus{
			ShareID:       shareInfo.ID,
			ShareName:     shareInfo.Name,
			MountTargetIP: shareInfo.MountTargets[0].IPAddress,
			MountTargetID: shareInfo.MountTargets[0].ID,
			TotalGB:       1000,
			State:         "stable",
			Zone:          "us-south-1",
			MountTargets: []v1alpha1.ZoneMountTarget{
				{Zone: "us-south-1", MountTargetID: shareInfo.MountTargets[0].ID, MountTargetIP: shareInfo.MountTargets[0].IPAddress},
				{Zone: "us-south-2", MountTargetID: "existing-mt", MountTargetIP: "10.0.1.1"},
			},
		},
	)
	pool.Spec.InitialShares = 1
	pool.Spec.AccessorZones = []v1alpha1.AccessorZone{
		{Zone: "us-south-2", SubnetID: "subnet-2"},
	}
	k.addPool(pool)

	r := newReconciler(k, vpc)
	r.SetVPCConfig("vpc-1", "subnet-1", "rg-1")
	_, err := reconcilePool(r, "test-pool")
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	// Should NOT create any mount targets (already exists)
	if vpc.CreateMountTargetCalls != 0 {
		t.Errorf("expected 0 CreateMountTarget calls (idempotent), got %d", vpc.CreateMountTargetCalls)
	}
}

func TestReconcile_EnsureAccessorMountTargets_SkipsCreatingShares(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()

	pool := newTestPool("test-pool", "spread", 1000,
		v1alpha1.PoolShareStatus{
			ShareID:       "share-creating",
			ShareName:     "s1",
			MountTargetIP: "",
			TotalGB:       1000,
			State:         "creating",
			Zone:          "us-south-1",
		},
	)
	pool.Spec.InitialShares = 1
	pool.Spec.AccessorZones = []v1alpha1.AccessorZone{
		{Zone: "us-south-2", SubnetID: "subnet-2"},
	}
	k.addPool(pool)

	r := newReconciler(k, vpc)
	r.SetVPCConfig("vpc-1", "subnet-1", "rg-1")
	_, err := reconcilePool(r, "test-pool")
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	// Should NOT create mount targets for non-stable shares
	if vpc.CreateMountTargetCalls != 0 {
		t.Errorf("expected 0 CreateMountTarget calls for creating shares, got %d", vpc.CreateMountTargetCalls)
	}
}

func TestReconcile_NoAccessorZones_BackwardCompat(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()

	shareInfo, _ := vpc.CreateFileShare(context.Background(), ibmcloudInput("test-pool", 1000))

	pool := newTestPool("test-pool", "spread", 1000,
		v1alpha1.PoolShareStatus{
			ShareID:       shareInfo.ID,
			ShareName:     shareInfo.Name,
			MountTargetIP: shareInfo.MountTargets[0].IPAddress,
			MountTargetID: shareInfo.MountTargets[0].ID,
			TotalGB:       1000,
			State:         "stable",
			Zone:          "us-south-1",
		},
	)
	pool.Spec.InitialShares = 1
	// No AccessorZones
	k.addPool(pool)

	r := newReconciler(k, vpc)
	_, err := reconcilePool(r, "test-pool")
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	// No mount target creation calls
	if vpc.CreateMountTargetCalls != 0 {
		t.Errorf("expected 0 CreateMountTarget calls, got %d", vpc.CreateMountTargetCalls)
	}
}

// ---------------------------------------------------------------------------
// 18. Drain Requests — Marks Shares as Draining
// ---------------------------------------------------------------------------

func TestReconcile_DrainRequests_MarksSharesDraining(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()

	// Create a share in VPC
	shareInfo, _ := vpc.CreateFileShare(context.Background(), ibmcloudInput("test-pool", 1000))

	pool := newTestPool("test-pool", "spread", 1000,
		v1alpha1.PoolShareStatus{
			ShareID:       shareInfo.ID,
			ShareName:     shareInfo.Name,
			MountTargetIP: "10.240.0.1",
			TotalGB:       1000,
			AllocatedGB:   100,
			PVCCount:      2,
			State:         "stable",
			Zone:          "us-south-1",
		},
	)
	pool.Spec.InitialShares = 1
	pool.Spec.DrainShares = []string{shareInfo.ID}
	k.addPool(pool)

	// Add SubVolumes on this share
	k.addSubVolume(newTestSubVolume("pvc-drain-1", "test-pool", shareInfo.ID, 50))
	k.addSubVolume(newTestSubVolume("pvc-drain-2", "test-pool", shareInfo.ID, 50))

	r := newReconciler(k, vpc)
	_, err := reconcilePool(r, "test-pool")
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	updated := k.getPool("test-pool")

	// Share state should be "draining"
	if updated.Status.Shares[0].State != "draining" {
		t.Errorf("expected share state 'draining', got %q", updated.Status.Shares[0].State)
	}

	// DrainStatus should exist with 2 remaining SubVolumes
	if len(updated.Status.DrainStatus) != 1 {
		t.Fatalf("expected 1 drain status entry, got %d", len(updated.Status.DrainStatus))
	}
	ds := updated.Status.DrainStatus[0]
	if ds.ShareID != shareInfo.ID {
		t.Errorf("expected drain status shareID %q, got %q", shareInfo.ID, ds.ShareID)
	}
	if ds.RemainingSubVolumes != 2 {
		t.Errorf("expected 2 remaining SubVolumes, got %d", ds.RemainingSubVolumes)
	}
	if ds.Drained {
		t.Error("expected Drained=false when SubVolumes remain")
	}
	if ds.DrainStartedAt == nil {
		t.Error("expected DrainStartedAt to be set")
	}
}

// ---------------------------------------------------------------------------
// 19. Drain Requests — Fully Drained
// ---------------------------------------------------------------------------

func TestReconcile_DrainRequests_FullyDrained(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()

	shareInfo, _ := vpc.CreateFileShare(context.Background(), ibmcloudInput("test-pool", 1000))

	pool := newTestPool("test-pool", "spread", 1000,
		v1alpha1.PoolShareStatus{
			ShareID:       shareInfo.ID,
			ShareName:     shareInfo.Name,
			MountTargetIP: "10.240.0.1",
			TotalGB:       1000,
			AllocatedGB:   0,
			PVCCount:      0,
			State:         "stable",
			Zone:          "us-south-1",
		},
	)
	pool.Spec.InitialShares = 1
	pool.Spec.DrainShares = []string{shareInfo.ID}
	k.addPool(pool)

	// No SubVolumes on this share — it should be reported as drained

	r := newReconciler(k, vpc)
	_, err := reconcilePool(r, "test-pool")
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	updated := k.getPool("test-pool")

	// DrainStatus should show drained=true
	if len(updated.Status.DrainStatus) != 1 {
		t.Fatalf("expected 1 drain status entry, got %d", len(updated.Status.DrainStatus))
	}
	ds := updated.Status.DrainStatus[0]
	if !ds.Drained {
		t.Error("expected Drained=true when no SubVolumes remain")
	}
	if ds.RemainingSubVolumes != 0 {
		t.Errorf("expected 0 remaining SubVolumes, got %d", ds.RemainingSubVolumes)
	}

	// DrainComplete condition should be True
	drainComplete := findCondition(updated.Status.Conditions, "DrainComplete")
	if drainComplete == nil {
		t.Fatal("expected DrainComplete condition")
	}
	if drainComplete.Status != metav1.ConditionTrue {
		t.Errorf("expected DrainComplete=True, got %s", drainComplete.Status)
	}
}

// ---------------------------------------------------------------------------
// 20. Drain Requests — Progress Tracking
// ---------------------------------------------------------------------------

func TestReconcile_DrainRequests_ProgressTracking(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()

	shareInfo1, _ := vpc.CreateFileShare(context.Background(), ibmcloudInput("test-pool", 1000))
	shareInfo2, _ := vpc.CreateFileShare(context.Background(), ibmcloud.CreateShareInput{
		Name: "test-pool-share-2", Zone: "us-south-1", Profile: "dp2", SizeGB: 1000,
	})

	pool := newTestPool("test-pool", "spread", 1000,
		v1alpha1.PoolShareStatus{
			ShareID: shareInfo1.ID, ShareName: shareInfo1.Name,
			MountTargetIP: "10.240.0.1", TotalGB: 1000, State: "stable", Zone: "us-south-1",
		},
		v1alpha1.PoolShareStatus{
			ShareID: shareInfo2.ID, ShareName: shareInfo2.Name,
			MountTargetIP: "10.240.0.2", TotalGB: 1000, State: "stable", Zone: "us-south-1",
		},
	)
	pool.Spec.InitialShares = 2
	pool.Spec.DrainShares = []string{shareInfo1.ID, shareInfo2.ID}
	k.addPool(pool)

	// share1 has SubVolumes, share2 is empty
	k.addSubVolume(newTestSubVolume("pvc-d1", "test-pool", shareInfo1.ID, 50))
	k.addSubVolume(newTestSubVolume("pvc-d2", "test-pool", shareInfo1.ID, 30))

	r := newReconciler(k, vpc)
	_, err := reconcilePool(r, "test-pool")
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	updated := k.getPool("test-pool")

	if len(updated.Status.DrainStatus) != 2 {
		t.Fatalf("expected 2 drain status entries, got %d", len(updated.Status.DrainStatus))
	}

	// Find drain statuses by share ID
	drainMap := make(map[string]*v1alpha1.ShareDrainStatus)
	for i := range updated.Status.DrainStatus {
		drainMap[updated.Status.DrainStatus[i].ShareID] = &updated.Status.DrainStatus[i]
	}

	ds1 := drainMap[shareInfo1.ID]
	if ds1 == nil {
		t.Fatalf("missing drain status for share %s", shareInfo1.ID)
	}
	if ds1.RemainingSubVolumes != 2 {
		t.Errorf("expected 2 remaining for share1, got %d", ds1.RemainingSubVolumes)
	}
	if ds1.Drained {
		t.Error("share1 should not be drained")
	}

	ds2 := drainMap[shareInfo2.ID]
	if ds2 == nil {
		t.Fatalf("missing drain status for share %s", shareInfo2.ID)
	}
	if ds2.RemainingSubVolumes != 0 {
		t.Errorf("expected 0 remaining for share2, got %d", ds2.RemainingSubVolumes)
	}
	if !ds2.Drained {
		t.Error("share2 should be drained")
	}

	// DrainComplete should be False (1/2 drained)
	drainComplete := findCondition(updated.Status.Conditions, "DrainComplete")
	if drainComplete == nil {
		t.Fatal("expected DrainComplete condition")
	}
	if drainComplete.Status != metav1.ConditionFalse {
		t.Errorf("expected DrainComplete=False (partial drain), got %s", drainComplete.Status)
	}
}

// ---------------------------------------------------------------------------
// 21. Drain Requests — No DrainShares Clears Status
// ---------------------------------------------------------------------------

func TestReconcile_DrainRequests_NoDrainSharesClearsStatus(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()

	shareInfo, _ := vpc.CreateFileShare(context.Background(), ibmcloudInput("test-pool", 1000))

	pool := newTestPool("test-pool", "spread", 1000,
		v1alpha1.PoolShareStatus{
			ShareID: shareInfo.ID, ShareName: shareInfo.Name,
			MountTargetIP: "10.240.0.1", TotalGB: 1000, State: "stable", Zone: "us-south-1",
		},
	)
	pool.Spec.InitialShares = 1
	// No DrainShares — but status has stale drain data
	pool.Status.DrainStatus = []v1alpha1.ShareDrainStatus{
		{ShareID: "old-share", RemainingSubVolumes: 5, Drained: false},
	}
	k.addPool(pool)

	r := newReconciler(k, vpc)
	_, err := reconcilePool(r, "test-pool")
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	updated := k.getPool("test-pool")
	if len(updated.Status.DrainStatus) != 0 {
		t.Errorf("expected drain status cleared, got %d entries", len(updated.Status.DrainStatus))
	}
}

// ---------------------------------------------------------------------------
// 22. Drain — Preserves DrainStartedAt Across Reconciles
// ---------------------------------------------------------------------------

func TestReconcile_DrainRequests_PreservesDrainStartedAt(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()

	shareInfo, _ := vpc.CreateFileShare(context.Background(), ibmcloudInput("test-pool", 1000))

	pool := newTestPool("test-pool", "spread", 1000,
		v1alpha1.PoolShareStatus{
			ShareID: shareInfo.ID, ShareName: shareInfo.Name,
			MountTargetIP: "10.240.0.1", TotalGB: 1000, State: "stable", Zone: "us-south-1",
		},
	)
	pool.Spec.InitialShares = 1
	pool.Spec.DrainShares = []string{shareInfo.ID}
	k.addPool(pool)

	k.addSubVolume(newTestSubVolume("pvc-d1", "test-pool", shareInfo.ID, 50))

	r := newReconciler(k, vpc)

	// First reconcile — sets DrainStartedAt
	_, err := reconcilePool(r, "test-pool")
	if err != nil {
		t.Fatalf("Reconcile 1 failed: %v", err)
	}

	updated := k.getPool("test-pool")
	if len(updated.Status.DrainStatus) != 1 {
		t.Fatalf("expected 1 drain status entry, got %d", len(updated.Status.DrainStatus))
	}
	firstStartedAt := updated.Status.DrainStatus[0].DrainStartedAt
	if firstStartedAt == nil {
		t.Fatal("expected DrainStartedAt to be set on first reconcile")
	}

	// Second reconcile — DrainStartedAt should be preserved
	_, err = reconcilePool(r, "test-pool")
	if err != nil {
		t.Fatalf("Reconcile 2 failed: %v", err)
	}

	updated = k.getPool("test-pool")
	if len(updated.Status.DrainStatus) != 1 {
		t.Fatalf("expected 1 drain status entry, got %d", len(updated.Status.DrainStatus))
	}
	secondStartedAt := updated.Status.DrainStatus[0].DrainStartedAt
	if secondStartedAt == nil {
		t.Fatal("expected DrainStartedAt to be preserved on second reconcile")
	}
	if !firstStartedAt.Time.Equal(secondStartedAt.Time) {
		t.Errorf("DrainStartedAt changed: %v -> %v", firstStartedAt.Time, secondStartedAt.Time)
	}
}

// ---------------------------------------------------------------------------
// 23. Drain — Draining Share Excluded from Proactive Expansion Calculation
// ---------------------------------------------------------------------------

func TestReconcile_DrainExcludesFromAllocation(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()

	shareInfo, _ := vpc.CreateFileShare(context.Background(), ibmcloudInput("test-pool", 1000))

	pool := newTestPool("test-pool", "spread", 1000,
		v1alpha1.PoolShareStatus{
			ShareID: shareInfo.ID, ShareName: shareInfo.Name,
			MountTargetIP: "10.240.0.1", MountTargetID: shareInfo.MountTargets[0].ID,
			TotalGB: 1000, State: "stable", Zone: "us-south-1",
		},
		newStableShare("share-stable", "s2", 1000, 500, 3),
	)
	pool.Spec.InitialShares = 2
	pool.Spec.DrainShares = []string{shareInfo.ID}
	k.addPool(pool)

	// Add SubVolumes on the stable share only
	k.addSubVolume(newTestSubVolume("pvc-s1", "test-pool", "share-stable", 500))

	r := newReconciler(k, vpc)
	_, err := reconcilePool(r, "test-pool")
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	updated := k.getPool("test-pool")

	// The drained share should be "draining"
	for _, s := range updated.Status.Shares {
		if s.ShareID == shareInfo.ID && s.State != "draining" {
			t.Errorf("expected share %s state 'draining', got %q", shareInfo.ID, s.State)
		}
	}
}

// ---------------------------------------------------------------------------
// 24. StorageClass Auto-creation on Reconcile
// ---------------------------------------------------------------------------

func TestReconcile_CreatesStorageClass(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()

	pool := newTestPool("test-pool", "spread", 1000)
	pool.Spec.InitialShares = 0
	pool.Spec.DefaultPermissions = "0755"
	k.addPool(pool)

	r := newReconciler(k, vpc)
	_, err := reconcilePool(r, "test-pool")
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	sc := k.getStorageClass("test-pool")
	if sc == nil {
		t.Fatal("expected StorageClass 'test-pool' to be created")
	}
	if sc.Provisioner != ProvisionerName {
		t.Errorf("expected provisioner %q, got %q", ProvisionerName, sc.Provisioner)
	}
	if sc.Parameters["pool"] != "test-pool" {
		t.Errorf("expected pool param 'test-pool', got %q", sc.Parameters["pool"])
	}
	if sc.Parameters["permissions"] != "0755" {
		t.Errorf("expected permissions '0755', got %q", sc.Parameters["permissions"])
	}
}

// ---------------------------------------------------------------------------
// 25. StorageClass Idempotency — Existing SC not overwritten
// ---------------------------------------------------------------------------

func TestReconcile_StorageClass_Idempotent(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()

	pool := newTestPool("test-pool", "spread", 1000)
	pool.Spec.InitialShares = 0
	k.addPool(pool)

	r := newReconciler(k, vpc)

	// First reconcile — creates SC
	_, err := reconcilePool(r, "test-pool")
	if err != nil {
		t.Fatalf("Reconcile 1 failed: %v", err)
	}
	if k.storageClassCount() != 1 {
		t.Fatalf("expected 1 StorageClass after first reconcile, got %d", k.storageClassCount())
	}

	// Second reconcile — SC should not be recreated (idempotent)
	_, err = reconcilePool(r, "test-pool")
	if err != nil {
		t.Fatalf("Reconcile 2 failed: %v", err)
	}
	if k.storageClassCount() != 1 {
		t.Errorf("expected 1 StorageClass after second reconcile, got %d", k.storageClassCount())
	}
}

// ---------------------------------------------------------------------------
// 26. StorageClass Skip Annotation
// ---------------------------------------------------------------------------

func TestReconcile_StorageClass_SkipAnnotation(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()

	pool := newTestPool("test-pool", "spread", 1000)
	pool.Spec.InitialShares = 0
	pool.Annotations = map[string]string{
		SkipStorageClassAnnotation: "true",
	}
	k.addPool(pool)

	r := newReconciler(k, vpc)
	_, err := reconcilePool(r, "test-pool")
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	if k.storageClassCount() != 0 {
		t.Errorf("expected 0 StorageClasses with skip annotation, got %d", k.storageClassCount())
	}
}

// ---------------------------------------------------------------------------
// 27. StorageClass for Tiered Pool
// ---------------------------------------------------------------------------

func TestReconcile_CreatesStorageClasses_Tiered(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()

	pool := newTestPoolWithTiers("tiered-pool", "spread",
		[]v1alpha1.ShareTier{
			{Name: "standard", Profile: "dp2", ShareSizeGB: 1000, MaxShares: 5, InitialShares: 0},
			{Name: "premium", Profile: "custom", ShareSizeGB: 500, MaxShares: 3, InitialShares: 0},
		},
	)
	pool.Status.Phase = "Ready"
	pool.Spec.InitialShares = 0
	k.addPool(pool)

	r := newReconciler(k, vpc)
	_, err := reconcilePool(r, "tiered-pool")
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	if k.storageClassCount() != 2 {
		t.Fatalf("expected 2 StorageClasses for tiered pool, got %d", k.storageClassCount())
	}

	standardSC := k.getStorageClass("tiered-pool-standard")
	if standardSC == nil {
		t.Fatal("expected StorageClass 'tiered-pool-standard'")
	}
	if standardSC.Parameters["tier"] != "standard" {
		t.Errorf("expected tier 'standard', got %q", standardSC.Parameters["tier"])
	}

	premiumSC := k.getStorageClass("tiered-pool-premium")
	if premiumSC == nil {
		t.Fatal("expected StorageClass 'tiered-pool-premium'")
	}
	if premiumSC.Parameters["tier"] != "premium" {
		t.Errorf("expected tier 'premium', got %q", premiumSC.Parameters["tier"])
	}
}

// ---------------------------------------------------------------------------
// Condition Helper
// ---------------------------------------------------------------------------

func findCondition(conditions []metav1.Condition, condType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Test Helpers
// ---------------------------------------------------------------------------

func ibmcloudInput(poolName string, sizeGB int64) ibmcloud.CreateShareInput {
	return ibmcloud.CreateShareInput{
		Name:    fmt.Sprintf("%s-share-1", poolName),
		Zone:    "us-south-1",
		Profile: "dp2",
		SizeGB:  sizeGB,
	}
}
