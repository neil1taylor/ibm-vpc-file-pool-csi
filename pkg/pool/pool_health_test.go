package pool

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/ibmcloud/fake"
	"k8s.io/apimachinery/pkg/api/meta"
)

// shareWithoutMountTarget is a share the VPC reports as stable but that has no
// mount target, as seen on rovslab-prod-eude on 2026-10-19.
func shareWithoutMountTarget(id string, totalGB int64) v1alpha1.PoolShareStatus {
	return v1alpha1.PoolShareStatus{
		ShareID:   id,
		ShareName: id,
		TotalGB:   totalGB,
		State:     "stable",
		Zone:      "us-south-1",
	}
}

func assertSecurityGroupNetwork(t *testing.T, what, subnetID string, sgIDs []string) {
	t.Helper()
	if subnetID != "subnet-us-south-1" {
		t.Errorf("%s subnet = %q, want subnet-us-south-1", what, subnetID)
	}
	if !slices.Equal(sgIDs, []string{"sg-kube-test-cluster"}) {
		t.Errorf("%s security groups = %v, want the cluster group", what, sgIDs)
	}
}

func TestReconcile_InitialShareGetsSecurityGroupMountTarget(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	pool := newTestPool("p", "spread", 1000)
	pool.Status.Phase = ""
	k.addPool(pool)

	if _, err := reconcilePool(newReconciler(k, vpc), "p"); err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	if vpc.CreateCalls != 1 {
		t.Fatalf("CreateFileShare calls = %d, want 1", vpc.CreateCalls)
	}
	assertSecurityGroupNetwork(t, "share", vpc.LastCreateInput.SubnetID, vpc.LastCreateInput.SecurityGroupIDs)
	share := k.getPool("p").Status.Shares[0]
	if share.ExportPath == "" || share.ExportPath == "/" {
		t.Errorf("ExportPath = %q, want the share-specific export", share.ExportPath)
	}
}

func TestReconcile_NoSubnetInZoneCreatesNothing(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	pool := newTestPool("p", "spread", 1000)
	pool.Spec.Zone = "eu-de-3"
	pool.Status.Phase = ""
	k.addPool(pool)

	if _, err := reconcilePool(newReconciler(k, vpc), "p"); err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	if vpc.CreateCalls != 0 {
		t.Errorf("CreateFileShare calls = %d, want 0", vpc.CreateCalls)
	}
	cond := meta.FindStatusCondition(k.getPool("p").Status.Conditions, "SharesReady")
	if cond == nil || !strings.Contains(cond.Message, "eu-de-3") {
		t.Errorf("SharesReady condition = %+v, want a message that names the zone", cond)
	}
}

func TestReconcile_MountTargetRecoveryUsesSecurityGroupNetwork(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	r := newReconciler(k, vpc)
	info, _ := vpc.CreateFileShare(context.Background(), ibmcloudInput("p", 1000))
	vpc.ClearMountTargets(info.ID)
	pool := newTestPool("p", "spread", 1000, shareWithoutMountTarget(info.ID, 1000))
	k.addPool(pool)

	if _, err := reconcilePool(r, "p"); err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	if vpc.CreateMountTargetCalls != 1 {
		t.Fatalf("CreateShareMountTarget calls = %d, want 1", vpc.CreateMountTargetCalls)
	}
	in := vpc.LastMountTargetInput
	assertSecurityGroupNetwork(t, "mount target", in.SubnetID, in.SecurityGroupIDs)
}

func TestReconcile_SharesWithoutMountTargetAreDegraded(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	r := newReconciler(k, vpc)
	vpc.CreateMountTargetErr = errors.New("mount target refused")
	var shares []v1alpha1.PoolShareStatus
	for range 2 {
		info, _ := vpc.CreateFileShare(context.Background(), ibmcloudInput("p", 1000))
		vpc.ClearMountTargets(info.ID)
		shares = append(shares, shareWithoutMountTarget(info.ID, 1000))
	}
	pool := newTestPool("p", "spread", 1000, shares...)
	k.addPool(pool)
	createsBefore := vpc.CreateCalls

	if _, err := reconcilePool(r, "p"); err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	got := k.getPool("p")
	if got.Status.Phase != "Degraded" {
		t.Errorf("phase = %q, want Degraded", got.Status.Phase)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, "SharesReady")
	if cond == nil || cond.Status != "False" || cond.Reason != "NoMountTarget" {
		t.Fatalf("SharesReady condition = %+v, want False/NoMountTarget", cond)
	}
	if !strings.Contains(cond.Message, shares[0].ShareID) {
		t.Errorf("condition message %q does not name the share", cond.Message)
	}
	if vpc.CreateCalls != createsBefore {
		t.Errorf("pool grew by %d shares while no share was usable", vpc.CreateCalls-createsBefore)
	}
}

func TestDeterminePhase_ReadyNeedsAMountTarget(t *testing.T) {
	tests := []struct {
		name   string
		shares []v1alpha1.PoolShareStatus
		want   string
	}{
		{"usable share", []v1alpha1.PoolShareStatus{newStableShare("a", "a", 100, 0, 0)}, "Ready"},
		{"no mount target", []v1alpha1.PoolShareStatus{shareWithoutMountTarget("a", 100)}, "Degraded"},
		{"one usable, one without", []v1alpha1.PoolShareStatus{
			newStableShare("a", "a", 100, 0, 0), shareWithoutMountTarget("b", 100),
		}, "Ready"},
		{"still creating", []v1alpha1.PoolShareStatus{{ShareID: "a", State: "creating"}}, "Expanding"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := determinePhase(newTestPool("p", "spread", 100, tt.shares...)); got != tt.want {
				t.Errorf("determinePhase() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestProactiveExpansion_WaitsForMountTargets(t *testing.T) {
	vpc := fake.NewFakeVPCClient()
	r := newReconciler(newFakeK8sClient(), vpc)
	// The usable share is 90% full, above the threshold, but the other share
	// has no mount target yet. Adding a third share would not help.
	pool := newTestPool("p", "spread", 100,
		newStableShare("a", "a", 100, 90, 9), shareWithoutMountTarget("b", 100))
	pool.Spec.ExpandThresholdPercent = 80

	r.proactiveExpansion(context.Background(), pool)

	if vpc.CreateCalls != 0 {
		t.Errorf("CreateFileShare calls = %d, want 0", vpc.CreateCalls)
	}
}

func TestAllocate_AutoExpandGetsSecurityGroupMountTarget(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	pool := newTestPool("p", "spread", 1000, newStableShare("a", "a", 1000, 1000, 10))
	pool.Spec.MaxShares = 5
	k.addPool(pool)

	_, err := newManagerForTest(k, vpc, newFakeNFSOperations()).Allocate(context.Background(), AllocationRequest{
		PVName: testPVName, PVCName: "c", PVCNamespace: "default", PoolName: "p", RequestedGB: 10,
	})
	if err != nil {
		t.Fatalf("Allocate failed: %v", err)
	}
	assertSecurityGroupNetwork(t, "share", vpc.LastCreateInput.SubnetID, vpc.LastCreateInput.SecurityGroupIDs)
}

func TestAllocate_NoAutoExpandWhileShareLacksMountTarget(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	pool := newTestPool("p", "spread", 1000, shareWithoutMountTarget("share-x", 1000))
	pool.Spec.MaxShares = 5
	k.addPool(pool)

	_, err := newManagerForTest(k, vpc, newFakeNFSOperations()).Allocate(context.Background(), AllocationRequest{
		PVName: testPVName, PVCName: "c", PVCNamespace: "default", PoolName: "p", RequestedGB: 10,
	})
	if !errors.Is(err, ErrNoUsableShare) {
		t.Fatalf("Allocate error = %v, want ErrNoUsableShare", err)
	}
	if !strings.Contains(err.Error(), "share-x") {
		t.Errorf("error %q does not name the unusable share", err)
	}
	if vpc.CreateCalls != 0 {
		t.Errorf("CreateFileShare calls = %d, want 0", vpc.CreateCalls)
	}
}
