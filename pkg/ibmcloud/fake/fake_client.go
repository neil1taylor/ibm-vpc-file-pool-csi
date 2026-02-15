package fake

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/ibmcloud"
)

// FakeVPCClient implements ibmcloud.VPCFileClient entirely in memory.
// Tests can inspect and manipulate its internal state.
type FakeVPCClient struct {
	mu     sync.Mutex
	shares map[string]*ibmcloud.ShareInfo
	nextID int

	// Test hooks — set these to inject failures.
	CreateErr error
	GetErr    error
	ExpandErr error
	DeleteErr error

	// Counters for assertions.
	CreateCalls int
	GetCalls    int
	ExpandCalls int
	DeleteCalls int
}

// Verify the fake implements the interface at compile time.
var _ ibmcloud.VPCFileClient = (*FakeVPCClient)(nil)

// NewFakeVPCClient creates a new in-memory fake VPC client.
func NewFakeVPCClient() *FakeVPCClient {
	return &FakeVPCClient{
		shares: make(map[string]*ibmcloud.ShareInfo),
	}
}

func (f *FakeVPCClient) CreateFileShare(_ context.Context, input ibmcloud.CreateShareInput) (*ibmcloud.ShareInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.CreateCalls++

	if f.CreateErr != nil {
		return nil, f.CreateErr
	}

	f.nextID++
	id := fmt.Sprintf("r006-fake-%04d", f.nextID)

	info := &ibmcloud.ShareInfo{
		ID:             id,
		Name:           input.Name,
		LifecycleState: "stable",
		SizeGB:         input.SizeGB,
		IOPS:           1000,
		Profile:        input.Profile,
		Zone:           input.Zone,
		MountTargets: []ibmcloud.MountTargetInfo{
			{
				ID:        id + "-mt",
				Name:      input.Name + "-mt",
				IPAddress: fmt.Sprintf("10.240.0.%d", f.nextID),
			},
		},
		CreatedAt: time.Now(),
	}

	f.shares[id] = info
	return info, nil
}

func (f *FakeVPCClient) GetFileShare(_ context.Context, shareID string) (*ibmcloud.ShareInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.GetCalls++

	if f.GetErr != nil {
		return nil, f.GetErr
	}

	info, ok := f.shares[shareID]
	if !ok {
		return nil, ibmcloud.ErrShareNotFound
	}
	return info, nil
}

func (f *FakeVPCClient) ExpandFileShare(_ context.Context, shareID string, newSizeGB int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ExpandCalls++

	if f.ExpandErr != nil {
		return f.ExpandErr
	}

	info, ok := f.shares[shareID]
	if !ok {
		return ibmcloud.ErrShareNotFound
	}
	info.SizeGB = newSizeGB
	return nil
}

func (f *FakeVPCClient) DeleteFileShare(_ context.Context, shareID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.DeleteCalls++

	if f.DeleteErr != nil {
		return f.DeleteErr
	}

	delete(f.shares, shareID)
	return nil
}

func (f *FakeVPCClient) ListFileShares(_ context.Context, _ string, _ []string) ([]*ibmcloud.ShareInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var result []*ibmcloud.ShareInfo
	for _, s := range f.shares {
		result = append(result, s)
	}
	return result, nil
}

// ShareCount returns the number of shares in the fake store.
func (f *FakeVPCClient) ShareCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.shares)
}

// GetShareDirect returns a share by ID without incrementing counters.
func (f *FakeVPCClient) GetShareDirect(id string) *ibmcloud.ShareInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.shares[id]
}
