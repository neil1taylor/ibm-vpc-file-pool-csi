package ibmcloud

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/IBM/go-sdk-core/v5/core"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/metrics"
	secretlib "github.com/IBM/secret-common-lib/pkg/secret_provider"
	"github.com/IBM/secret-utils-lib/pkg/k8s_utils"
	sp "github.com/IBM/secret-utils-lib/pkg/secret_provider"
	"github.com/IBM/vpc-go-sdk/vpcv1"
	"golang.org/x/time/rate"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

// SecretProvider abstracts credential retrieval for testability.
// It mirrors the subset of secret-utils-lib SecretProviderInterface that we need.
type SecretProvider interface {
	GetDefaultIAMToken(freshTokenRequired bool, reasonForCall ...string) (string, uint64, error)
	GetRIAASEndpoint(readConfig bool) (string, error)
	GetPrivateRIAASEndpoint(readConfig bool) (string, error)
	GetResourceGroupID() string
}

// Client implements VPCFileClient by wrapping the IBM VPC Go SDK.
type Client struct {
	vpcService      *vpcv1.VpcV1
	secretProvider  SecretProvider
	region          string
	resourceGroupID string
	rateLimiter     *rate.Limiter
	pollInterval    time.Duration // default 10s, configurable for tests

	// getShareFunc allows tests to inject a fake for waitForShareStable polling.
	getShareFunc func(ctx context.Context, shareID string) (*ShareInfo, error)
}

// Verify Client implements VPCFileClient at compile time.
var _ VPCFileClient = (*Client)(nil)

// NewClient creates a VPC API client using the cluster's existing credentials.
// It uses secret-common-lib to read API keys or pod identity tokens from the
// same secrets that the stock IBM CSI drivers use.
func NewClient(k8sClient kubernetes.Interface, region string) (*Client, error) {
	k8sUtils := &k8s_utils.KubernetesClient{
		Namespace: "kube-system",
		Clientset: k8sClient,
	}

	secretProvider, err := secretlib.NewSecretProvider(k8sUtils)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize secret provider: %w", err)
	}

	return NewClientWithProvider(secretProvider, region)
}

// NewClientWithProvider creates a VPC API client with an explicit SecretProvider.
// This is the primary constructor, used by NewClient and directly in tests.
func NewClientWithProvider(sp SecretProvider, region string) (*Client, error) {
	// Get an initial IAM token to verify credentials are valid.
	token, _, err := sp.GetDefaultIAMToken(false, "vpc-file-pool-csi-init")
	if err != nil {
		return nil, fmt.Errorf("failed to get initial IAM token: %w", err)
	}

	// Determine VPC API endpoint: private → public → region-derived fallback.
	riaasEndpoint := ""
	if ep, epErr := sp.GetPrivateRIAASEndpoint(true); epErr == nil && ep != "" {
		riaasEndpoint = ep
	}
	if riaasEndpoint == "" {
		if ep, epErr := sp.GetRIAASEndpoint(true); epErr == nil && ep != "" {
			riaasEndpoint = ep
		}
	}

	// Auto-discover region from endpoint if not explicitly provided.
	if region == "" && riaasEndpoint != "" {
		region = ParseRegionFromEndpoint(riaasEndpoint)
		if region != "" {
			klog.V(2).InfoS("Auto-discovered region from endpoint", "region", region, "endpoint", riaasEndpoint)
		}
	}

	if riaasEndpoint == "" {
		riaasEndpoint = fmt.Sprintf("https://%s.iaas.cloud.ibm.com/v1", region)
	}

	// Ensure endpoint ends with /v1 — secret providers return the base URL
	// (e.g. "https://eu-de.private.iaas.cloud.ibm.com") but the VPC SDK
	// expects the versioned path.
	if !strings.HasSuffix(riaasEndpoint, "/v1") {
		riaasEndpoint = strings.TrimSuffix(riaasEndpoint, "/") + "/v1"
	}

	// Read resource group from secret provider config.
	resourceGroupID := sp.GetResourceGroupID()

	klog.V(2).InfoS("Creating VPC API client",
		"region", region,
		"endpoint", riaasEndpoint,
		"resourceGroupID", resourceGroupID,
	)

	authenticator := &core.BearerTokenAuthenticator{
		BearerToken: token,
	}

	vpcService, err := vpcv1.NewVpcV1(&vpcv1.VpcV1Options{
		Authenticator: authenticator,
		URL:           riaasEndpoint,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create VPC service: %w", err)
	}

	return &Client{
		vpcService:      vpcService,
		secretProvider:  sp,
		region:          region,
		resourceGroupID: resourceGroupID,
		rateLimiter:     rate.NewLimiter(rate.Every(time.Second/5), 1), // 5 req/sec
		pollInterval:    10 * time.Second,
	}, nil
}

// ResourceGroupID returns the resource group ID discovered from the secret provider.
func (c *Client) ResourceGroupID() string {
	return c.resourceGroupID
}

// refreshToken gets a fresh IAM token and updates the VPC service authenticator.
func (c *Client) refreshToken(_ context.Context, reason string) error {
	token, _, err := c.secretProvider.GetDefaultIAMToken(false, reason)
	if err != nil {
		return fmt.Errorf("failed to refresh IAM token: %w", err)
	}

	auth, ok := c.vpcService.Service.Options.Authenticator.(*core.BearerTokenAuthenticator)
	if !ok {
		return fmt.Errorf("unexpected authenticator type")
	}
	auth.BearerToken = token
	return nil
}

// withAuth wraps a VPC API call with rate limiting and token refresh.
func (c *Client) withAuth(ctx context.Context, operation string) error {
	if err := c.rateLimiter.Wait(ctx); err != nil {
		return fmt.Errorf("rate limiter: %w", err)
	}
	return c.refreshToken(ctx, operation)
}

// recordAPIMetrics records API call duration and status for a VPC operation.
func recordAPIMetrics(operation string, start time.Time, err error) {
	status := "success"
	if err != nil {
		status = "error"
	}
	metrics.VPCAPICallsTotal.WithLabelValues(operation, status).Inc()
	metrics.VPCAPICallDuration.WithLabelValues(operation).Observe(time.Since(start).Seconds())
}

// CreateFileShare creates a new VPC file share with an inline mount target.
func (c *Client) CreateFileShare(ctx context.Context, input CreateShareInput) (info *ShareInfo, retErr error) {
	start := time.Now()
	defer func() { recordAPIMetrics("create_share", start, retErr) }()

	if err := c.withAuth(ctx, "CreateFileShare"); err != nil {
		return nil, err
	}

	profileIdentity := &vpcv1.ShareProfileIdentityByName{
		Name: core.StringPtr(input.Profile),
	}

	sharePrototype, err := c.vpcService.NewSharePrototypeShareBySize(profileIdentity, input.SizeGB)
	if err != nil {
		return nil, fmt.Errorf("failed to build share prototype: %w", err)
	}

	sharePrototype.Name = core.StringPtr(input.Name)
	sharePrototype.Zone = &vpcv1.ZoneIdentityByName{
		Name: core.StringPtr(input.Zone),
	}

	// Set initial owner to nobody (65534) so the CSI node driver (running as root,
	// mapped to nobody by NFS root_squash) can create subdirectories on the share.
	nobodyID := int64(65534)
	sharePrototype.InitialOwner = &vpcv1.ShareInitialOwner{
		Uid: &nobodyID,
		Gid: &nobodyID,
	}

	if input.IOPS != nil {
		sharePrototype.Iops = input.IOPS
	}

	if input.ResourceGroupID != "" {
		sharePrototype.ResourceGroup = &vpcv1.ResourceGroupIdentityByID{
			ID: core.StringPtr(input.ResourceGroupID),
		}
	}

	if len(input.Tags) > 0 {
		sharePrototype.UserTags = input.Tags
	}

	// One inline mount target in security group access mode. IBM ended new
	// VPC-mode mount targets (HTTP 501), so a share must never be created
	// without a subnet for its virtual network interface.
	if input.SubnetID == "" {
		return nil, fmt.Errorf("create share %q: subnet ID is required for the mount target", input.Name)
	}
	sharePrototype.AccessControlMode = core.StringPtr("security_group")
	sharePrototype.MountTargets = []vpcv1.ShareMountTargetPrototypeIntf{
		securityGroupMountTarget(input.Name+"-mt", input.SubnetID, input.SecurityGroupIDs, input.EncryptInTransit, input.ResourceGroupID),
	}

	createOpts := c.vpcService.NewCreateShareOptions(sharePrototype)
	share, response, err := c.vpcService.CreateShareWithContext(ctx, createOpts)
	if err != nil {
		// Idempotency: if the share already exists, look it up and return it.
		// This handles retries where the share was created but the caller
		// didn't persist the result (e.g., status update conflict).
		if response != nil && response.StatusCode == 400 && strings.Contains(err.Error(), "already exists") {
			klog.V(2).InfoS("Share already exists, adopting", "name", input.Name)
			return c.getShareByName(ctx, input.Name)
		}
		statusCode := 0
		if response != nil {
			statusCode = response.StatusCode
		}
		return nil, mapHTTPError(statusCode, err)
	}

	klog.V(4).InfoS("VPC share created, waiting for stable",
		"shareID", *share.ID,
		"name", *share.Name,
	)

	shareInfo, err := c.waitForShareStable(ctx, *share.ID, 5*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("share %s did not become stable: %w", *share.ID, err)
	}

	if err := c.waitForMountPath(ctx, shareInfo); err != nil {
		return nil, err
	}

	return shareInfo, nil
}

// CreateShareMountTarget creates a security group mount target on an existing
// share. The reconciler uses it to recover a share that has no mount target.
func (c *Client) CreateShareMountTarget(ctx context.Context, shareID string, input CreateMountTargetInput) (_ *MountTargetInfo, retErr error) {
	start := time.Now()
	defer func() { recordAPIMetrics("create_mount_target", start, retErr) }()

	if err := c.withAuth(ctx, "CreateShareMountTarget"); err != nil {
		return nil, err
	}

	if input.SubnetID == "" {
		return nil, fmt.Errorf("create mount target %q: subnet ID is required", input.Name)
	}
	mtPrototype := securityGroupMountTarget(input.Name, input.SubnetID, input.SecurityGroupIDs, input.EncryptInTransit, "")

	opts := c.vpcService.NewCreateShareMountTargetOptions(shareID, mtPrototype)
	mt, response, err := c.vpcService.CreateShareMountTargetWithContext(ctx, opts)
	if err != nil {
		statusCode := 0
		if response != nil {
			statusCode = response.StatusCode
		}
		return nil, mapHTTPError(statusCode, err)
	}

	klog.V(4).InfoS("Mount target created, waiting for IP",
		"shareID", shareID,
		"mountTargetID", *mt.ID,
		"name", *mt.Name,
	)

	// Poll until the mount target has an IP address populated.
	deadline := time.Now().Add(5 * time.Minute)
	for {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("mount target %s IP not populated after timeout", *mt.ID)
		}

		mtInfo, err := c.getMountTarget(ctx, shareID, *mt.ID)
		if err != nil {
			return nil, fmt.Errorf("get mount target %s: %w", *mt.ID, err)
		}
		if mtInfo.IPAddress != "" {
			return mtInfo, nil
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(c.pollInterval):
			continue
		}
	}
}

// ListSharesByName returns every share with this exact name, with mount
// targets. A create call that failed while building the inline mount target
// can leave a share without one next to the complete share of the same name.
func (c *Client) ListSharesByName(ctx context.Context, name string) (_ []*ShareInfo, retErr error) {
	start := time.Now()
	defer func() { recordAPIMetrics("list_shares_by_name", start, retErr) }()

	if err := c.withAuth(ctx, "ListSharesByName"); err != nil {
		return nil, err
	}
	opts := c.vpcService.NewListSharesOptions()
	opts.SetName(name)
	opts.SetLimit(100)
	result, response, err := c.vpcService.ListSharesWithContext(ctx, opts)
	if err != nil {
		statusCode := 0
		if response != nil {
			statusCode = response.StatusCode
		}
		return nil, mapHTTPError(statusCode, err)
	}
	var out []*ShareInfo
	for _, s := range result.Shares {
		info, err := c.GetFileShare(ctx, *s.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, info)
	}
	return out, nil
}

// GetFileShare retrieves a VPC file share by ID, including mount target IPs.
func (c *Client) GetFileShare(ctx context.Context, shareID string) (_ *ShareInfo, retErr error) {
	start := time.Now()
	defer func() { recordAPIMetrics("get_share", start, retErr) }()

	if err := c.withAuth(ctx, "GetFileShare"); err != nil {
		return nil, err
	}

	opts := c.vpcService.NewGetShareOptions(shareID)
	share, response, err := c.vpcService.GetShareWithContext(ctx, opts)
	if err != nil {
		statusCode := 0
		if response != nil {
			statusCode = response.StatusCode
		}
		return nil, mapHTTPError(statusCode, err)
	}

	info := shareToInfo(share)

	// Fetch mount target details to get NFS IPs.
	for _, mtRef := range share.MountTargets {
		mtInfo, mtErr := c.getMountTarget(ctx, shareID, *mtRef.ID)
		if mtErr != nil {
			klog.V(4).InfoS("Failed to get mount target details, skipping",
				"shareID", shareID, "mountTargetID", *mtRef.ID, "error", mtErr)
			continue
		}
		info.MountTargets = append(info.MountTargets, *mtInfo)
	}

	return info, nil
}

// ExpandFileShare increases a VPC file share's size.
func (c *Client) ExpandFileShare(ctx context.Context, shareID string, newSizeGB int64) (retErr error) {
	start := time.Now()
	defer func() { recordAPIMetrics("expand_share", start, retErr) }()

	if err := c.withAuth(ctx, "ExpandFileShare"); err != nil {
		return err
	}

	sharePatch := map[string]interface{}{
		"size": newSizeGB,
	}
	opts := c.vpcService.NewUpdateShareOptions(shareID, sharePatch)
	_, response, err := c.vpcService.UpdateShareWithContext(ctx, opts)
	if err != nil {
		statusCode := 0
		if response != nil {
			statusCode = response.StatusCode
		}
		return mapHTTPError(statusCode, err)
	}

	_, err = c.waitForShareStable(ctx, shareID, 5*time.Minute)
	return err
}

// DeleteFileShare deletes a VPC file share and its mount targets.
// Idempotent: returns nil if the share is already gone.
func (c *Client) DeleteFileShare(ctx context.Context, shareID string) (retErr error) {
	start := time.Now()
	defer func() { recordAPIMetrics("delete_share", start, retErr) }()

	// Check if share exists first.
	share, err := c.GetFileShare(ctx, shareID)
	if err != nil {
		if errors.Is(err, ErrShareNotFound) {
			return nil
		}
		return err
	}

	// Delete all mount targets first.
	for _, mt := range share.MountTargets {
		if mtErr := c.withAuth(ctx, "DeleteShareMountTarget"); mtErr != nil {
			return mtErr
		}
		mtOpts := c.vpcService.NewDeleteShareMountTargetOptions(shareID, mt.ID)
		_, _, mtErr := c.vpcService.DeleteShareMountTargetWithContext(ctx, mtOpts)
		if mtErr != nil {
			klog.V(4).InfoS("Failed to delete mount target (may already be deleted)",
				"shareID", shareID, "mountTargetID", mt.ID, "error", mtErr)
		}
	}

	// Delete the share.
	if err = c.withAuth(ctx, "DeleteFileShare"); err != nil {
		return err
	}
	deleteOpts := c.vpcService.NewDeleteShareOptions(shareID)
	_, response, err := c.vpcService.DeleteShareWithContext(ctx, deleteOpts)
	if err != nil {
		if response != nil && response.StatusCode == 404 {
			return nil
		}
		statusCode := 0
		if response != nil {
			statusCode = response.StatusCode
		}
		return mapHTTPError(statusCode, err)
	}

	return nil
}

// ListFileShares lists VPC file shares filtered by resource group.
// Mount target IPs are NOT fetched during list (too expensive for N+1 calls).
func (c *Client) ListFileShares(ctx context.Context, resourceGroupID string, _ []string) (_ []*ShareInfo, retErr error) {
	startTime := time.Now()
	defer func() { recordAPIMetrics("list_shares", startTime, retErr) }()

	var allShares []*ShareInfo
	var start *string

	for {
		if err := c.withAuth(ctx, "ListFileShares"); err != nil {
			return nil, err
		}

		opts := c.vpcService.NewListSharesOptions()
		if resourceGroupID != "" {
			opts.SetResourceGroupID(resourceGroupID)
		}
		if start != nil {
			opts.SetStart(*start)
		}
		opts.SetLimit(100)

		result, _, err := c.vpcService.ListSharesWithContext(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("VPC API ListShares failed: %w", err)
		}

		for i := range result.Shares {
			info := shareToInfo(&result.Shares[i])
			// Populate mount target refs (ID/Name only, no IP).
			for _, mtRef := range result.Shares[i].MountTargets {
				mt := MountTargetInfo{
					ID:   *mtRef.ID,
					Name: *mtRef.Name,
				}
				info.MountTargets = append(info.MountTargets, mt)
			}
			allShares = append(allShares, info)
		}

		var nextErr error
		start, nextErr = result.GetNextStart()
		if nextErr != nil || start == nil {
			break
		}
	}

	return allShares, nil
}

// waitForShareStable polls until the share reaches "stable", or returns an
// error on "failed" or timeout.
func (c *Client) waitForShareStable(ctx context.Context, shareID string, timeout time.Duration) (*ShareInfo, error) {
	deadline := time.Now().Add(timeout)

	getShare := c.GetFileShare
	if c.getShareFunc != nil {
		getShare = c.getShareFunc
	}

	for {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%w: timed out after %v for share %s", ErrShareNotStable, timeout, shareID)
		}

		info, err := getShare(ctx, shareID)
		if err != nil {
			return nil, err
		}

		switch info.LifecycleState {
		case "stable":
			return info, nil
		case "failed":
			return nil, fmt.Errorf("%w: share %s", ErrShareCreationFailed, shareID)
		default:
			// "pending", "updating", etc. — wait and retry.
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(c.pollInterval):
				continue
			}
		}
	}
}

// getMountTarget fetches a single mount target to extract the NFS IP address.
func (c *Client) getMountTarget(ctx context.Context, shareID, mtID string) (*MountTargetInfo, error) {
	if err := c.withAuth(ctx, "GetShareMountTarget"); err != nil {
		return nil, err
	}

	opts := c.vpcService.NewGetShareMountTargetOptions(shareID, mtID)
	mt, response, err := c.vpcService.GetShareMountTargetWithContext(ctx, opts)
	if err != nil {
		statusCode := 0
		if response != nil {
			statusCode = response.StatusCode
		}
		return nil, mapHTTPError(statusCode, err)
	}

	info := &MountTargetInfo{
		ID:   *mt.ID,
		Name: *mt.Name,
	}

	// The NFS server and export path both come from MountPath
	// ("server:/export_path") in every access mode. The export is share-specific,
	// so a mount target without a MountPath is not usable yet, even if its
	// virtual network interface already has an IP address.
	if mt.MountPath != nil && *mt.MountPath != "" {
		info.IPAddress = parseMountPathServer(*mt.MountPath)
		info.ExportPath = parseMountPathExport(*mt.MountPath)
	}

	return info, nil
}

// getShareByName looks up a VPC file share by name and returns its full info
// (including waiting for stable and populating mount target IPs).
// Used by CreateFileShare for idempotent retry when a share already exists.
func (c *Client) getShareByName(ctx context.Context, name string) (*ShareInfo, error) {
	if err := c.withAuth(ctx, "getShareByName"); err != nil {
		return nil, err
	}

	opts := c.vpcService.NewListSharesOptions()
	opts.SetName(name)
	opts.SetLimit(100)

	result, _, err := c.vpcService.ListSharesWithContext(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("lookup share by name %q: %w", name, err)
	}

	if len(result.Shares) == 0 {
		return nil, fmt.Errorf("share %q not found after 'already exists' error", name)
	}

	// More than one share can carry the name when an earlier create failed
	// while building the mount target. Adopt the one that has a mount target.
	chosen := result.Shares[0]
	for i := range result.Shares {
		s := result.Shares[i]
		if len(s.MountTargets) > 0 {
			chosen = s
			break
		}
	}
	shareID := *chosen.ID

	// Wait for the share to be stable (it may still be provisioning).
	shareInfo, err := c.waitForShareStable(ctx, shareID, 5*time.Minute)
	if err != nil {
		return nil, err
	}

	if err := c.waitForMountPath(ctx, shareInfo); err != nil {
		return nil, err
	}

	klog.V(2).InfoS("Adopted existing share", "name", name, "id", shareID,
		"state", shareInfo.LifecycleState)
	return shareInfo, nil
}

// waitForMountPath polls the share's first mount target until its mount path
// is known, then copies the server and export path into shareInfo. A share
// may reach "stable" before its mount target does. If the mount path is still
// unknown after two minutes, shareInfo is left without it and the reconciler's
// health check fills it in later.
func (c *Client) waitForMountPath(ctx context.Context, shareInfo *ShareInfo) error {
	if len(shareInfo.MountTargets) == 0 || shareInfo.MountTargets[0].IPAddress != "" {
		return nil
	}
	mt := &shareInfo.MountTargets[0]
	klog.V(4).InfoS("Mount path not yet available, polling", "shareID", shareInfo.ID, "mountTargetID", mt.ID)
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.pollInterval):
		}
		mtInfo, err := c.getMountTarget(ctx, shareInfo.ID, mt.ID)
		if err != nil {
			continue
		}
		if mtInfo.IPAddress != "" {
			mt.IPAddress = mtInfo.IPAddress
			mt.ExportPath = mtInfo.ExportPath
			klog.V(4).InfoS("Mount path now available",
				"shareID", shareInfo.ID, "ip", mt.IPAddress, "exportPath", mt.ExportPath)
			return nil
		}
	}
	return nil
}

// securityGroupMountTarget builds a mount target prototype whose virtual
// network interface is placed in subnetID and protected by securityGroupIDs.
func securityGroupMountTarget(name, subnetID string, securityGroupIDs []string, encryptInTransit bool, resourceGroupID string) *vpcv1.ShareMountTargetPrototype {
	transitEncryption := "none"
	if encryptInTransit {
		transitEncryption = "user_managed"
	}
	vni := &vpcv1.ShareMountTargetVirtualNetworkInterfacePrototype{
		Name:   core.StringPtr(name + "-vni"),
		Subnet: &vpcv1.SubnetIdentityByID{ID: core.StringPtr(subnetID)},
	}
	for _, id := range securityGroupIDs {
		vni.SecurityGroups = append(vni.SecurityGroups, &vpcv1.SecurityGroupIdentityByID{ID: core.StringPtr(id)})
	}
	if resourceGroupID != "" {
		vni.ResourceGroup = &vpcv1.ResourceGroupIdentityByID{ID: core.StringPtr(resourceGroupID)}
	}
	return &vpcv1.ShareMountTargetPrototype{
		Name:                    core.StringPtr(name),
		VirtualNetworkInterface: vni,
		AccessProtocol:          core.StringPtr("nfs4"),
		TransitEncryption:       core.StringPtr(transitEncryption),
	}
}

// GetSubnetZone returns the zone name of a VPC subnet.
func (c *Client) GetSubnetZone(ctx context.Context, subnetID string) (_ string, retErr error) {
	start := time.Now()
	defer func() { recordAPIMetrics("get_subnet", start, retErr) }()

	if err := c.withAuth(ctx, "GetSubnet"); err != nil {
		return "", err
	}
	subnet, response, err := c.vpcService.GetSubnetWithContext(ctx, c.vpcService.NewGetSubnetOptions(subnetID))
	if err != nil {
		statusCode := 0
		if response != nil {
			statusCode = response.StatusCode
		}
		return "", fmt.Errorf("get subnet %s: %w", subnetID, mapHTTPError(statusCode, err))
	}
	if subnet.Zone == nil || subnet.Zone.Name == nil {
		return "", fmt.Errorf("subnet %s has no zone", subnetID)
	}
	return *subnet.Zone.Name, nil
}

// FindSecurityGroupID returns the ID of the security group with the given name
// in the given VPC. Security group names are unique within a VPC.
func (c *Client) FindSecurityGroupID(ctx context.Context, vpcID, name string) (_ string, retErr error) {
	start := time.Now()
	defer func() { recordAPIMetrics("list_security_groups", start, retErr) }()

	opts := c.vpcService.NewListSecurityGroupsOptions()
	opts.SetVPCID(vpcID)
	for {
		if err := c.withAuth(ctx, "ListSecurityGroups"); err != nil {
			return "", err
		}
		result, response, err := c.vpcService.ListSecurityGroupsWithContext(ctx, opts)
		if err != nil {
			statusCode := 0
			if response != nil {
				statusCode = response.StatusCode
			}
			return "", fmt.Errorf("list security groups in VPC %s: %w", vpcID, mapHTTPError(statusCode, err))
		}
		for _, sg := range result.SecurityGroups {
			if sg.Name != nil && *sg.Name == name && sg.ID != nil {
				return *sg.ID, nil
			}
		}
		if result.Next == nil || result.Next.Href == nil {
			break
		}
		next := parseStartFromURL(*result.Next.Href)
		if next == nil {
			break
		}
		opts.Start = next
	}
	return "", fmt.Errorf("security group %q not found in VPC %s", name, vpcID)
}

// shareToInfo converts a VPC SDK Share to our ShareInfo type.
// Mount target IPs are NOT populated — callers that need IPs must call getMountTarget.
func shareToInfo(share *vpcv1.Share) *ShareInfo {
	info := &ShareInfo{
		ID:             *share.ID,
		Name:           *share.Name,
		LifecycleState: *share.LifecycleState,
		SizeGB:         *share.Size,
		IOPS:           *share.Iops,
	}

	if share.Profile != nil && share.Profile.Name != nil {
		info.Profile = *share.Profile.Name
	}
	if share.Zone != nil && share.Zone.Name != nil {
		info.Zone = *share.Zone.Name
	}
	if share.ResourceGroup != nil && share.ResourceGroup.ID != nil {
		info.ResourceGroupID = *share.ResourceGroup.ID
	}
	info.Tags = share.UserTags
	if share.CreatedAt != nil {
		info.CreatedAt = time.Time(*share.CreatedAt)
	}

	return info
}

// Ensure the real secret-utils-lib SecretProviderInterface satisfies our local interface.
var _ SecretProvider = (sp.SecretProviderInterface)(nil)
