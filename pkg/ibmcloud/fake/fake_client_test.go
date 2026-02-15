package fake

import (
	"context"
	"errors"
	"testing"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/ibmcloud"
)

func TestNewFakeVPCClient(t *testing.T) {
	c := NewFakeVPCClient()
	if c == nil {
		t.Fatal("NewFakeVPCClient() returned nil")
	}
	if c.ShareCount() != 0 {
		t.Errorf("new client should have 0 shares, got %d", c.ShareCount())
	}
}

func TestCreateFileShare_Success(t *testing.T) {
	c := NewFakeVPCClient()
	ctx := context.Background()

	input := ibmcloud.CreateShareInput{
		Name:    "test-share",
		Zone:    "us-south-1",
		Profile: "dp2",
		SizeGB:  500,
	}

	info, err := c.CreateFileShare(ctx, input)
	if err != nil {
		t.Fatalf("CreateFileShare() error: %v", err)
	}

	if info.ID == "" {
		t.Error("expected non-empty ID")
	}
	if info.Name != "test-share" {
		t.Errorf("Name = %q, want %q", info.Name, "test-share")
	}
	if info.Zone != "us-south-1" {
		t.Errorf("Zone = %q, want %q", info.Zone, "us-south-1")
	}
	if info.Profile != "dp2" {
		t.Errorf("Profile = %q, want %q", info.Profile, "dp2")
	}
	if info.SizeGB != 500 {
		t.Errorf("SizeGB = %d, want 500", info.SizeGB)
	}
	if info.LifecycleState != "stable" {
		t.Errorf("LifecycleState = %q, want %q", info.LifecycleState, "stable")
	}
	if len(info.MountTargets) != 1 {
		t.Fatalf("MountTargets count = %d, want 1", len(info.MountTargets))
	}
	if info.MountTargets[0].IPAddress == "" {
		t.Error("expected non-empty mount target IP")
	}

	if c.CreateCalls != 1 {
		t.Errorf("CreateCalls = %d, want 1", c.CreateCalls)
	}
	if c.ShareCount() != 1 {
		t.Errorf("ShareCount = %d, want 1", c.ShareCount())
	}
}

func TestCreateFileShare_UniqueIDs(t *testing.T) {
	c := NewFakeVPCClient()
	ctx := context.Background()

	input := ibmcloud.CreateShareInput{Name: "share", Zone: "us-south-1", Profile: "dp2", SizeGB: 100}
	info1, _ := c.CreateFileShare(ctx, input)
	info2, _ := c.CreateFileShare(ctx, input)

	if info1.ID == info2.ID {
		t.Errorf("two shares should have different IDs, both got %q", info1.ID)
	}
	if c.CreateCalls != 2 {
		t.Errorf("CreateCalls = %d, want 2", c.CreateCalls)
	}
	if c.ShareCount() != 2 {
		t.Errorf("ShareCount = %d, want 2", c.ShareCount())
	}
}

func TestCreateFileShare_Error(t *testing.T) {
	c := NewFakeVPCClient()
	c.CreateErr = ibmcloud.ErrShareCreationFailed

	_, err := c.CreateFileShare(context.Background(), ibmcloud.CreateShareInput{})
	if !errors.Is(err, ibmcloud.ErrShareCreationFailed) {
		t.Errorf("err = %v, want ErrShareCreationFailed", err)
	}
	if c.CreateCalls != 1 {
		t.Errorf("CreateCalls = %d, want 1", c.CreateCalls)
	}
	if c.ShareCount() != 0 {
		t.Error("no share should be created on error")
	}
}

func TestGetFileShare_Success(t *testing.T) {
	c := NewFakeVPCClient()
	ctx := context.Background()

	created, _ := c.CreateFileShare(ctx, ibmcloud.CreateShareInput{
		Name: "my-share", Zone: "us-south-2", Profile: "dp2", SizeGB: 200,
	})

	got, err := c.GetFileShare(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetFileShare() error: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("ID = %q, want %q", got.ID, created.ID)
	}
	if got.Name != "my-share" {
		t.Errorf("Name = %q, want %q", got.Name, "my-share")
	}
	if c.GetCalls != 1 {
		t.Errorf("GetCalls = %d, want 1", c.GetCalls)
	}
}

func TestGetFileShare_NotFound(t *testing.T) {
	c := NewFakeVPCClient()

	_, err := c.GetFileShare(context.Background(), "nonexistent")
	if !errors.Is(err, ibmcloud.ErrShareNotFound) {
		t.Errorf("err = %v, want ErrShareNotFound", err)
	}
}

func TestGetFileShare_InjectedError(t *testing.T) {
	c := NewFakeVPCClient()
	c.GetErr = ibmcloud.ErrAuthentication

	// Create a share first so the not-found path isn't hit.
	created, _ := c.CreateFileShare(context.Background(), ibmcloud.CreateShareInput{
		Name: "s", Zone: "z", Profile: "p", SizeGB: 100,
	})

	_, err := c.GetFileShare(context.Background(), created.ID)
	if !errors.Is(err, ibmcloud.ErrAuthentication) {
		t.Errorf("err = %v, want ErrAuthentication", err)
	}
}

func TestExpandFileShare_Success(t *testing.T) {
	c := NewFakeVPCClient()
	ctx := context.Background()

	created, _ := c.CreateFileShare(ctx, ibmcloud.CreateShareInput{
		Name: "expand-me", Zone: "us-south-1", Profile: "dp2", SizeGB: 100,
	})

	err := c.ExpandFileShare(ctx, created.ID, 200)
	if err != nil {
		t.Fatalf("ExpandFileShare() error: %v", err)
	}

	got := c.GetShareDirect(created.ID)
	if got.SizeGB != 200 {
		t.Errorf("SizeGB after expand = %d, want 200", got.SizeGB)
	}
	if c.ExpandCalls != 1 {
		t.Errorf("ExpandCalls = %d, want 1", c.ExpandCalls)
	}
}

func TestExpandFileShare_NotFound(t *testing.T) {
	c := NewFakeVPCClient()

	err := c.ExpandFileShare(context.Background(), "nonexistent", 200)
	if !errors.Is(err, ibmcloud.ErrShareNotFound) {
		t.Errorf("err = %v, want ErrShareNotFound", err)
	}
}

func TestExpandFileShare_InjectedError(t *testing.T) {
	c := NewFakeVPCClient()
	c.ExpandErr = ibmcloud.ErrAPIRateLimit

	created, _ := c.CreateFileShare(context.Background(), ibmcloud.CreateShareInput{
		Name: "s", Zone: "z", Profile: "p", SizeGB: 100,
	})

	err := c.ExpandFileShare(context.Background(), created.ID, 200)
	if !errors.Is(err, ibmcloud.ErrAPIRateLimit) {
		t.Errorf("err = %v, want ErrAPIRateLimit", err)
	}
	// Verify size wasn't changed.
	got := c.GetShareDirect(created.ID)
	if got.SizeGB != 100 {
		t.Errorf("SizeGB should be unchanged, got %d", got.SizeGB)
	}
}

func TestDeleteFileShare_Success(t *testing.T) {
	c := NewFakeVPCClient()
	ctx := context.Background()

	created, _ := c.CreateFileShare(ctx, ibmcloud.CreateShareInput{
		Name: "delete-me", Zone: "us-south-1", Profile: "dp2", SizeGB: 100,
	})

	err := c.DeleteFileShare(ctx, created.ID)
	if err != nil {
		t.Fatalf("DeleteFileShare() error: %v", err)
	}

	if c.ShareCount() != 0 {
		t.Errorf("ShareCount after delete = %d, want 0", c.ShareCount())
	}
	if c.DeleteCalls != 1 {
		t.Errorf("DeleteCalls = %d, want 1", c.DeleteCalls)
	}
}

func TestDeleteFileShare_NonExistent(t *testing.T) {
	c := NewFakeVPCClient()

	// Deleting a non-existent share should succeed (idempotent behavior of the fake).
	err := c.DeleteFileShare(context.Background(), "nonexistent")
	if err != nil {
		t.Errorf("DeleteFileShare(nonexistent) error: %v", err)
	}
}

func TestDeleteFileShare_InjectedError(t *testing.T) {
	c := NewFakeVPCClient()
	c.DeleteErr = ibmcloud.ErrAuthentication

	err := c.DeleteFileShare(context.Background(), "any-id")
	if !errors.Is(err, ibmcloud.ErrAuthentication) {
		t.Errorf("err = %v, want ErrAuthentication", err)
	}
	if c.DeleteCalls != 1 {
		t.Errorf("DeleteCalls = %d, want 1", c.DeleteCalls)
	}
}

func TestListFileShares_Empty(t *testing.T) {
	c := NewFakeVPCClient()

	result, err := c.ListFileShares(context.Background(), "", nil)
	if err != nil {
		t.Fatalf("ListFileShares() error: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected 0 shares, got %d", len(result))
	}
}

func TestListFileShares_ReturnsAll(t *testing.T) {
	c := NewFakeVPCClient()
	ctx := context.Background()

	_, _ = c.CreateFileShare(ctx, ibmcloud.CreateShareInput{Name: "s1", Zone: "z", Profile: "p", SizeGB: 100})
	_, _ = c.CreateFileShare(ctx, ibmcloud.CreateShareInput{Name: "s2", Zone: "z", Profile: "p", SizeGB: 200})
	_, _ = c.CreateFileShare(ctx, ibmcloud.CreateShareInput{Name: "s3", Zone: "z", Profile: "p", SizeGB: 300})

	result, err := c.ListFileShares(ctx, "any-rg", []string{"tag1"})
	if err != nil {
		t.Fatalf("ListFileShares() error: %v", err)
	}
	if len(result) != 3 {
		t.Errorf("expected 3 shares, got %d", len(result))
	}
}

func TestListFileShares_AfterDelete(t *testing.T) {
	c := NewFakeVPCClient()
	ctx := context.Background()

	s1, _ := c.CreateFileShare(ctx, ibmcloud.CreateShareInput{Name: "s1", Zone: "z", Profile: "p", SizeGB: 100})
	_, _ = c.CreateFileShare(ctx, ibmcloud.CreateShareInput{Name: "s2", Zone: "z", Profile: "p", SizeGB: 200})

	_ = c.DeleteFileShare(ctx, s1.ID)

	result, err := c.ListFileShares(ctx, "", nil)
	if err != nil {
		t.Fatalf("ListFileShares() error: %v", err)
	}
	if len(result) != 1 {
		t.Errorf("expected 1 share after delete, got %d", len(result))
	}
}

func TestGetShareDirect(t *testing.T) {
	c := NewFakeVPCClient()
	ctx := context.Background()

	created, _ := c.CreateFileShare(ctx, ibmcloud.CreateShareInput{
		Name: "direct", Zone: "us-south-1", Profile: "dp2", SizeGB: 100,
	})

	got := c.GetShareDirect(created.ID)
	if got == nil {
		t.Fatal("GetShareDirect returned nil for existing share")
	}
	if got.Name != "direct" {
		t.Errorf("Name = %q, want %q", got.Name, "direct")
	}

	// Non-existent returns nil.
	if c.GetShareDirect("bogus") != nil {
		t.Error("GetShareDirect should return nil for non-existent share")
	}
}

func TestShareCount(t *testing.T) {
	c := NewFakeVPCClient()
	ctx := context.Background()

	if c.ShareCount() != 0 {
		t.Errorf("initial ShareCount = %d, want 0", c.ShareCount())
	}

	s1, _ := c.CreateFileShare(ctx, ibmcloud.CreateShareInput{Name: "s1", Zone: "z", Profile: "p", SizeGB: 100})
	if c.ShareCount() != 1 {
		t.Errorf("ShareCount = %d, want 1", c.ShareCount())
	}

	_, _ = c.CreateFileShare(ctx, ibmcloud.CreateShareInput{Name: "s2", Zone: "z", Profile: "p", SizeGB: 100})
	if c.ShareCount() != 2 {
		t.Errorf("ShareCount = %d, want 2", c.ShareCount())
	}

	_ = c.DeleteFileShare(ctx, s1.ID)
	if c.ShareCount() != 1 {
		t.Errorf("ShareCount after delete = %d, want 1", c.ShareCount())
	}
}

func TestCallCounters(t *testing.T) {
	c := NewFakeVPCClient()
	ctx := context.Background()

	created, _ := c.CreateFileShare(ctx, ibmcloud.CreateShareInput{Name: "s", Zone: "z", Profile: "p", SizeGB: 100})
	_, _ = c.GetFileShare(ctx, created.ID)
	_, _ = c.GetFileShare(ctx, created.ID)
	_ = c.ExpandFileShare(ctx, created.ID, 200)
	_ = c.DeleteFileShare(ctx, created.ID)

	if c.CreateCalls != 1 {
		t.Errorf("CreateCalls = %d, want 1", c.CreateCalls)
	}
	if c.GetCalls != 2 {
		t.Errorf("GetCalls = %d, want 2", c.GetCalls)
	}
	if c.ExpandCalls != 1 {
		t.Errorf("ExpandCalls = %d, want 1", c.ExpandCalls)
	}
	if c.DeleteCalls != 1 {
		t.Errorf("DeleteCalls = %d, want 1", c.DeleteCalls)
	}
}
