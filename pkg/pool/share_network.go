package pool

import (
	"context"
	"errors"
	"fmt"
	"sync"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/ibmcloud"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ErrNoSubnetForZone means none of the cluster's worker subnets is in the
// pool's zone, so a share's mount target has nowhere to go.
var ErrNoSubnetForZone = errors.New("no cluster subnet in the pool's zone")

// ShareNetwork holds the cluster network that each new share's mount target
// joins. Every share is created in security group access mode: its mount
// target gets a virtual network interface in the worker subnet of the pool's
// zone, protected by the cluster's own security group (kube-<cluster ID>)
// unless the pool names its own groups.
type ShareNetwork struct {
	vpcID     string
	subnetIDs []string
	clusterID string

	mu          sync.Mutex
	subnetZones map[string]string // subnet ID -> zone, filled on first use
	clusterSGID string
}

// NewShareNetwork creates a ShareNetwork for the given VPC, worker subnets and
// cluster ID. The cluster ID may be empty if every pool sets securityGroupIDs.
func NewShareNetwork(vpcID string, subnetIDs []string, clusterID string) *ShareNetwork {
	return &ShareNetwork{
		vpcID:       vpcID,
		subnetIDs:   subnetIDs,
		clusterID:   clusterID,
		subnetZones: make(map[string]string),
	}
}

// mountTargetNetwork returns the subnet and security groups for a mount target
// of a share in the given pool. Lookups are cached; a failed lookup is retried
// on the next call.
func (n *ShareNetwork) mountTargetNetwork(ctx context.Context, vpc ibmcloud.VPCFileClient, pool *v1alpha1.FileSharePool) (string, []string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	subnetID, err := n.subnetInZone(ctx, vpc, pool.Spec.Zone)
	if err != nil {
		return "", nil, err
	}

	if len(pool.Spec.SecurityGroupIDs) > 0 {
		return subnetID, pool.Spec.SecurityGroupIDs, nil
	}
	if n.clusterSGID == "" {
		if n.clusterID == "" {
			return "", nil, fmt.Errorf("cluster ID is unknown, so the cluster security group cannot be found; set securityGroupIDs on pool %q", pool.Name)
		}
		id, err := vpc.FindSecurityGroupID(ctx, n.vpcID, "kube-"+n.clusterID)
		if err != nil {
			return "", nil, fmt.Errorf("find cluster security group: %w", err)
		}
		n.clusterSGID = id
	}
	return subnetID, []string{n.clusterSGID}, nil
}

// subnetInZone returns the first cluster subnet in zone. The caller holds n.mu.
func (n *ShareNetwork) subnetInZone(ctx context.Context, vpc ibmcloud.VPCFileClient, zone string) (string, error) {
	for _, id := range n.subnetIDs {
		subnetZone, ok := n.subnetZones[id]
		if !ok {
			var err error
			subnetZone, err = vpc.GetSubnetZone(ctx, id)
			if err != nil {
				return "", fmt.Errorf("look up zone of subnet %s: %w", id, err)
			}
			n.subnetZones[id] = subnetZone
		}
		if subnetZone == zone {
			return id, nil
		}
	}
	return "", fmt.Errorf("%w: zone %s, cluster subnets %v", ErrNoSubnetForZone, zone, n.subnetIDs)
}

// newShareInput builds the request for a new share in the given pool tier.
// Initial provisioning, proactive expansion and auto-expansion all use it, so
// no share can be created without its mount target network.
func newShareInput(ctx context.Context, vpc ibmcloud.VPCFileClient, network *ShareNetwork, pool *v1alpha1.FileSharePool, tier, defaultResourceGroup string) (ibmcloud.CreateShareInput, error) {
	profile, sizeGB, iops, _, _, err := pool.Spec.TierConfig(tier)
	if err != nil {
		return ibmcloud.CreateShareInput{}, err
	}
	if network == nil {
		return ibmcloud.CreateShareInput{}, errors.New("cluster network is not configured yet")
	}
	subnetID, sgIDs, err := network.mountTargetNetwork(ctx, vpc, pool)
	if err != nil {
		return ibmcloud.CreateShareInput{}, err
	}

	resourceGroup := pool.Spec.ResourceGroup
	if resourceGroup == "" {
		resourceGroup = defaultResourceGroup
	}

	return ibmcloud.CreateShareInput{
		Name:             nextShareName(pool, tier),
		Zone:             pool.Spec.Zone,
		Profile:          profile,
		SizeGB:           sizeGB,
		IOPS:             iops,
		ResourceGroupID:  resourceGroup,
		Tags:             pool.Spec.Tags,
		EncryptInTransit: pool.Spec.EncryptionInTransit,
		SubnetID:         subnetID,
		SecurityGroupIDs: sgIDs,
	}, nil
}

// nextShareName returns the name for the next share in the pool, including the
// tier when there is one.
func nextShareName(pool *v1alpha1.FileSharePool, tier string) string {
	if tier == "" {
		return fmt.Sprintf("%s-share-%d", pool.Name, len(pool.Status.Shares)+1)
	}
	tierShareCount := 0
	for _, s := range pool.Status.Shares {
		if s.Tier == tier {
			tierShareCount++
		}
	}
	return fmt.Sprintf("%s-%s-share-%d", pool.Name, tier, tierShareCount+1)
}

// newPoolShareStatus records a newly created share for the pool status. The
// share's state follows the VPC lifecycle state; shareReady decides whether it
// can serve volumes.
func newPoolShareStatus(info *ibmcloud.ShareInfo, tier string) v1alpha1.PoolShareStatus {
	now := metav1.Now()
	status := v1alpha1.PoolShareStatus{
		ShareID:   info.ID,
		ShareName: info.Name,
		TotalGB:   info.SizeGB,
		State:     "creating",
		Tier:      tier,
		Zone:      info.Zone,
		CreatedAt: &now,
	}
	if info.LifecycleState == "stable" {
		status.State = "stable"
	}
	if len(info.MountTargets) > 0 && info.MountTargets[0].IPAddress != "" {
		mt := info.MountTargets[0]
		status.MountTargetIP = mt.IPAddress
		status.MountTargetID = mt.ID
		status.ExportPath = mt.ExportPath
		status.MountTargets = []v1alpha1.ZoneMountTarget{{
			Zone:          info.Zone,
			MountTargetID: mt.ID,
			MountTargetIP: mt.IPAddress,
			ExportPath:    mt.ExportPath,
		}}
	}
	return status
}
