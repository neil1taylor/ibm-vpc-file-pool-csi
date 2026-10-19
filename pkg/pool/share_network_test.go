package pool

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/ibmcloud/fake"
)

// testNetwork registers one worker subnet per us-south zone and the cluster
// security group in the fake, and returns the matching ShareNetwork.
func testNetwork(vpc *fake.FakeVPCClient) *ShareNetwork {
	for _, zone := range []string{"us-south-1", "us-south-2", "us-south-3"} {
		vpc.SetSubnetZone("subnet-"+zone, zone)
	}
	vpc.AddSecurityGroup("kube-test-cluster", "sg-kube-test-cluster")
	return NewShareNetwork("vpc-test", []string{"subnet-us-south-1", "subnet-us-south-2", "subnet-us-south-3"}, "test-cluster")
}

func poolInZone(zone string) *v1alpha1.FileSharePool {
	pool := newTestPool("p", "spread", 100)
	pool.Spec.Zone = zone
	return pool
}

func TestShareNetwork_PicksSubnetInPoolZone(t *testing.T) {
	vpc := fake.NewFakeVPCClient()
	n := testNetwork(vpc)

	subnetID, sgIDs, err := n.mountTargetNetwork(context.Background(), vpc, poolInZone("us-south-2"))
	if err != nil {
		t.Fatalf("mountTargetNetwork() error = %v", err)
	}
	if subnetID != "subnet-us-south-2" {
		t.Errorf("subnet = %q, want subnet-us-south-2", subnetID)
	}
	if !slices.Equal(sgIDs, []string{"sg-kube-test-cluster"}) {
		t.Errorf("security groups = %v, want the cluster group", sgIDs)
	}
}

func TestShareNetwork_NoSubnetInPoolZone(t *testing.T) {
	vpc := fake.NewFakeVPCClient()
	n := testNetwork(vpc)

	_, _, err := n.mountTargetNetwork(context.Background(), vpc, poolInZone("eu-de-3"))
	if !errors.Is(err, ErrNoSubnetForZone) {
		t.Fatalf("error = %v, want ErrNoSubnetForZone", err)
	}
	if !strings.Contains(err.Error(), "eu-de-3") {
		t.Errorf("error %q does not name the zone", err)
	}
}

func TestShareNetwork_PoolSecurityGroupsOverrideClusterGroup(t *testing.T) {
	vpc := fake.NewFakeVPCClient()
	n := testNetwork(vpc)
	pool := poolInZone("us-south-1")
	pool.Spec.SecurityGroupIDs = []string{"sg-custom-a", "sg-custom-b"}

	_, sgIDs, err := n.mountTargetNetwork(context.Background(), vpc, pool)
	if err != nil {
		t.Fatalf("mountTargetNetwork() error = %v", err)
	}
	if !slices.Equal(sgIDs, pool.Spec.SecurityGroupIDs) {
		t.Errorf("security groups = %v, want %v", sgIDs, pool.Spec.SecurityGroupIDs)
	}
	if vpc.FindSecurityGroupCalls != 0 {
		t.Errorf("looked up the cluster group %d times, want 0", vpc.FindSecurityGroupCalls)
	}
}

func TestShareNetwork_NoClusterIDAndNoPoolGroups(t *testing.T) {
	vpc := fake.NewFakeVPCClient()
	vpc.SetSubnetZone("subnet-1", "us-south-1")
	n := NewShareNetwork("vpc-test", []string{"subnet-1"}, "")

	_, _, err := n.mountTargetNetwork(context.Background(), vpc, poolInZone("us-south-1"))
	if err == nil || !strings.Contains(err.Error(), "securityGroupIDs") {
		t.Fatalf("error = %v, want one that tells the user to set securityGroupIDs", err)
	}
}

func TestShareNetwork_CachesLookups(t *testing.T) {
	vpc := fake.NewFakeVPCClient()
	n := testNetwork(vpc)

	for range 3 {
		if _, _, err := n.mountTargetNetwork(context.Background(), vpc, poolInZone("us-south-3")); err != nil {
			t.Fatalf("mountTargetNetwork() error = %v", err)
		}
	}
	if vpc.GetSubnetZoneCalls != 3 {
		t.Errorf("GetSubnetZone calls = %d, want 3 (one per subnet, then cached)", vpc.GetSubnetZoneCalls)
	}
	if vpc.FindSecurityGroupCalls != 1 {
		t.Errorf("FindSecurityGroupID calls = %d, want 1", vpc.FindSecurityGroupCalls)
	}
}

func TestShareNetwork_FailedLookupIsRetried(t *testing.T) {
	vpc := fake.NewFakeVPCClient()
	n := NewShareNetwork("vpc-test", []string{"subnet-late"}, "test-cluster")
	vpc.AddSecurityGroup("kube-test-cluster", "sg-1")

	if _, _, err := n.mountTargetNetwork(context.Background(), vpc, poolInZone("us-south-1")); err == nil {
		t.Fatal("expected an error while the subnet is unknown")
	}
	vpc.SetSubnetZone("subnet-late", "us-south-1")
	subnetID, _, err := n.mountTargetNetwork(context.Background(), vpc, poolInZone("us-south-1"))
	if err != nil || subnetID != "subnet-late" {
		t.Errorf("after the subnet appears: subnet = %q, err = %v", subnetID, err)
	}
}
