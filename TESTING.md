# Testing Guide — IBM VPC File Pool CSI Driver

## Testing Pyramid

```
                    ┌─────────┐
                    │  E2E    │  Real ROKS cluster + real VPC shares
                    │ (manual)│  Run by humans before release
                   ─┼─────────┼─
                  / │ Integr. │ \  Local NFS server in Docker
                 /  │  Tests  │  \ No IBM Cloud needed
               ─┼───┼─────────┼───┼─
              / │   │  Unit   │   │ \  Pure Go, no external deps
             /  │   │  Tests  │   │  \ Run in CI, run in Claude Code
            ────┴───┴─────────┴───┴────
```

**Claude Code operates in the bottom two layers.** Every piece of code must have unit tests that run with `go test ./...` — no cluster, no NFS server, no IBM Cloud credentials. Integration tests are optional during development and use Docker for a local NFS server.

---

## Unit Testing Strategy

### What Gets Unit Tested

| Component | What to Test | Mocking Strategy |
|-----------|-------------|------------------|
| `pkg/pool/manager.go` | Allocation algorithm, share selection (spread/binpack), capacity tracking, idempotency, expand, deallocate, concurrent access | Fake K8s client, Fake VPC client, Fake NFS operations |
| `pkg/driver/controller.go` | Request validation, parameter parsing, volume ID construction, error mapping to gRPC codes, idempotency | Mock PoolManager interface |
| `pkg/driver/node.go` | Path validation, mount option construction, bind-mount logic, staging cache, NodeGetVolumeStats | Mock mounter (k8s.io/mount-utils/fake), mock filesystem |
| `pkg/ibmcloud/client.go` | Response parsing, error classification, rate limiter behavior, retry logic | HTTP test server (`httptest.NewServer`) or Fake VPC service |
| `pkg/k8s/reconciler.go` | Reconcile loop logic, pool phase transitions, share health detection, threshold calculations | Fake K8s client (`sigs.k8s.io/controller-runtime/pkg/client/fake`) |
| `pkg/util/mount.go` | Mount cache add/remove/lookup, ref counting, concurrent access | In-memory only, no real mounts |
| `pkg/util/quota.go` | Allocation arithmetic, overcommit detection | Pure functions, no mocks needed |
| `api/v1alpha1/*_types.go` | DeepCopy generated correctly, validation markers produce expected behavior | Standard Go tests on generated code |

### What Does NOT Get Unit Tested

- Actual NFS mount/unmount syscalls
- Actual IBM Cloud VPC API calls
- Actual Kubernetes API server communication
- Actual filesystem operations (mkdir on NFS)

These are all replaced with fakes/mocks in unit tests and tested for real in integration/e2e tests.

---

## Fakes and Mocks

### Fake VPC Client

Every unit test that touches the IBM Cloud API uses a fake. This must be built as part of the codebase, not as a test-only afterthought.

```go
// pkg/ibmcloud/fake/fake_client.go

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

    // Test hooks — set these to inject failures
    CreateErr  error  // If set, CreateFileShare returns this error
    GetErr     error  // If set, GetFileShare returns this error
    ExpandErr  error  // If set, ExpandFileShare returns this error
    DeleteErr  error  // If set, DeleteFileShare returns this error

    // Counters for assertions
    CreateCalls int
    GetCalls    int
    ExpandCalls int
    DeleteCalls int
}

func NewFakeVPCClient() *FakeVPCClient {
    return &FakeVPCClient{
        shares: make(map[string]*ibmcloud.ShareInfo),
    }
}

func (f *FakeVPCClient) CreateFileShare(ctx context.Context, input ibmcloud.CreateShareInput) (*ibmcloud.ShareInfo, error) {
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

func (f *FakeVPCClient) GetFileShare(ctx context.Context, shareID string) (*ibmcloud.ShareInfo, error) {
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

func (f *FakeVPCClient) ExpandFileShare(ctx context.Context, shareID string, newSizeGB int64) error {
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

func (f *FakeVPCClient) DeleteFileShare(ctx context.Context, shareID string) error {
    f.mu.Lock()
    defer f.mu.Unlock()
    f.DeleteCalls++

    if f.DeleteErr != nil {
        return f.DeleteErr
    }

    delete(f.shares, shareID)
    return nil
}

func (f *FakeVPCClient) ListFileShares(ctx context.Context, resourceGroupID string, tags []string) ([]*ibmcloud.ShareInfo, error) {
    f.mu.Lock()
    defer f.mu.Unlock()

    var result []*ibmcloud.ShareInfo
    for _, s := range f.shares {
        result = append(result, s)
    }
    return result, nil
}

// --- Test inspection helpers ---

func (f *FakeVPCClient) ShareCount() int {
    f.mu.Lock()
    defer f.mu.Unlock()
    return len(f.shares)
}

func (f *FakeVPCClient) GetShareDirect(id string) *ibmcloud.ShareInfo {
    f.mu.Lock()
    defer f.mu.Unlock()
    return f.shares[id]
}
```

### Fake NFS Operations

The pool manager creates and deletes subdirectories on NFS shares. In unit tests, replace real NFS operations with an in-memory fake:

```go
// pkg/pool/fake_nfs_test.go (test-only file)

package pool

import (
    "fmt"
    "os"
    "sync"
)

type FakeNFSOperations struct {
    mu    sync.Mutex
    dirs  map[string]os.FileMode  // path → permissions

    MkdirErr  error  // Inject mkdir failures
    RemoveErr error  // Inject remove failures
}

func NewFakeNFSOperations() *FakeNFSOperations {
    return &FakeNFSOperations{
        dirs: make(map[string]os.FileMode),
    }
}

func (f *FakeNFSOperations) MkdirAll(path string, perm os.FileMode) error {
    f.mu.Lock()
    defer f.mu.Unlock()
    if f.MkdirErr != nil {
        return f.MkdirErr
    }
    f.dirs[path] = perm
    return nil
}

func (f *FakeNFSOperations) RemoveAll(path string) error {
    f.mu.Lock()
    defer f.mu.Unlock()
    if f.RemoveErr != nil {
        return f.RemoveErr
    }
    delete(f.dirs, path)
    return nil
}

func (f *FakeNFSOperations) Stat(path string) (os.FileInfo, error) {
    f.mu.Lock()
    defer f.mu.Unlock()
    if _, ok := f.dirs[path]; ok {
        return nil, nil // Exists
    }
    return nil, os.ErrNotExist
}

func (f *FakeNFSOperations) Exists(path string) bool {
    f.mu.Lock()
    defer f.mu.Unlock()
    _, ok := f.dirs[path]
    return ok
}

func (f *FakeNFSOperations) DirCount() int {
    f.mu.Lock()
    defer f.mu.Unlock()
    return len(f.dirs)
}
```

### Fake Mounter (for Node Agent)

Use the Kubernetes mount-utils fake mounter, not a custom one:

```go
import "k8s.io/mount-utils/fake"

mounter := &fake.FakeMounter{
    MountPoints: []fake.MountPoint{},
}
```

This tracks all mount/unmount calls and lets you assert what was mounted where.

### Fake Kubernetes Client

Use controller-runtime's fake client builder:

```go
import (
    "sigs.k8s.io/controller-runtime/pkg/client/fake"
    "k8s.io/apimachinery/pkg/runtime"
)

scheme := runtime.NewScheme()
v1alpha1.AddToScheme(scheme)

k8sClient := fake.NewClientBuilder().
    WithScheme(scheme).
    WithObjects(pool, subVolume1, subVolume2).  // Pre-populate with test data
    WithStatusSubresource(&v1alpha1.FileSharePool{}).  // Enable status updates
    Build()
```

---

## Interfaces for Testability

Every external dependency must be behind an interface. This is non-negotiable — without it, unit testing is impossible.

```go
// pkg/ibmcloud/client.go
type VPCFileClient interface {
    CreateFileShare(ctx context.Context, input CreateShareInput) (*ShareInfo, error)
    GetFileShare(ctx context.Context, shareID string) (*ShareInfo, error)
    ExpandFileShare(ctx context.Context, shareID string, newSizeGB int64) error
    DeleteFileShare(ctx context.Context, shareID string) error
    ListFileShares(ctx context.Context, resourceGroupID string, tags []string) ([]*ShareInfo, error)
}

// pkg/pool/manager.go
type PoolManager interface {
    Allocate(ctx context.Context, req AllocationRequest) (*AllocationResult, error)
    Deallocate(ctx context.Context, subVolumeName string) error
    Expand(ctx context.Context, subVolumeName string, newSizeGB int64) error
}

// pkg/pool/nfs_operations.go — wraps filesystem calls for testability
type NFSOperations interface {
    MkdirAll(path string, perm os.FileMode) error
    RemoveAll(path string) error
    Stat(path string) (os.FileInfo, error)
    Chown(path string, uid, gid int) error
    Chmod(path string, mode os.FileMode) error
}

// Real implementation just delegates to os.*
type realNFSOperations struct{}
func (r *realNFSOperations) MkdirAll(path string, perm os.FileMode) error { return os.MkdirAll(path, perm) }
func (r *realNFSOperations) RemoveAll(path string) error { return os.RemoveAll(path) }
// ...etc

// pkg/util/mount.go — wraps mount calls for testability
// Use k8s.io/mount-utils.Interface directly — it already has a fake
```

---

## Test Cases by Component

### Pool Manager Tests (the critical ones)

```
TestAllocate_SpreadStrategy
    Given: Pool with 2 shares, share-1 at 75% allocated, share-2 at 25% allocated
    When:  Allocate 10 GB
    Then:  Returns share-2 (most free space)

TestAllocate_BinpackStrategy
    Given: Pool with 2 shares, share-1 at 75% allocated, share-2 at 25% allocated
    When:  Allocate 10 GB
    Then:  Returns share-1 (least free space that still fits)

TestAllocate_Idempotent
    Given: SubVolume "pvc-xyz" already exists on share-1
    When:  Allocate for "pvc-xyz" again (same name)
    Then:  Returns share-1 with same subpath (no new allocation)

TestAllocate_PoolExhausted_NoAutoExpand
    Given: Pool with 1 share, fully allocated, autoExpand=false
    When:  Allocate 10 GB
    Then:  Returns ErrPoolExhausted

TestAllocate_PoolExhausted_AutoExpand
    Given: Pool with 1 share, fully allocated, autoExpand=true, maxShares=5
    When:  Allocate 10 GB
    Then:  Creates new share via VPC client, allocates on new share

TestAllocate_PoolExhausted_MaxSharesReached
    Given: Pool with 10 shares (all full), maxShares=10, autoExpand=true
    When:  Allocate 10 GB
    Then:  Returns ErrPoolExhausted (can't create more shares)

TestAllocate_RequestTooLarge
    Given: Pool with 1 share (2000 GB), all free
    When:  Allocate 3000 GB (larger than any single share)
    Then:  Returns error (request exceeds share size)

TestAllocate_Concurrent
    Given: Pool with 1 share, 100 GB free
    When:  10 goroutines each allocate 10 GB simultaneously
    Then:  Exactly 10 succeed, capacity tracking is correct, no race conditions

TestAllocate_ZoneMismatch
    Given: Pool in us-south-1
    When:  Allocate with zone=us-south-2
    Then:  Returns error (zone doesn't match pool)

TestAllocate_CreatesSubdirectory
    Given: Pool with healthy share
    When:  Allocate 5 GB for "pvc-test"
    Then:  NFSOperations.MkdirAll called with "/pvcs/pvc-test"

TestAllocate_CreatesSubVolumeCR
    Given: Pool with healthy share
    When:  Allocate 5 GB for "pvc-test"
    Then:  SubVolume CR exists in fake K8s client with correct spec

TestAllocate_MkdirFails_NoSubVolumeCreated
    Given: NFSOperations.MkdirErr = errors.New("permission denied")
    When:  Allocate 5 GB
    Then:  Returns error, no SubVolume CR created (atomic — either both or neither)

TestDeallocate_Normal
    Given: SubVolume "pvc-test" exists
    When:  Deallocate "pvc-test"
    Then:  Subdirectory removed, SubVolume CR deleted, pool allocated capacity decreased

TestDeallocate_NotFound
    Given: No SubVolume named "pvc-missing"
    When:  Deallocate "pvc-missing"
    Then:  Returns ErrSubVolumeNotFound (caller treats this as idempotent success)

TestDeallocate_RemoveFails_SubVolumeRetained
    Given: NFSOperations.RemoveErr = errors.New("I/O error")
    When:  Deallocate "pvc-test"
    Then:  Returns error, SubVolume CR still exists (don't lose tracking if cleanup fails)

TestExpand_WithinShareCapacity
    Given: SubVolume "pvc-test" at 5 GB on a share with 500 GB free
    When:  Expand "pvc-test" to 50 GB
    Then:  SubVolume CR updated, pool allocated capacity increased by 45 GB

TestExpand_ExceedsShareCapacity
    Given: SubVolume "pvc-test" at 5 GB on a share with 10 GB free
    When:  Expand "pvc-test" to 100 GB
    Then:  Returns ErrInsufficientShareCapacity
```

### CSI Controller Tests

```
TestCreateVolume_MissingName
    → Returns codes.InvalidArgument

TestCreateVolume_MissingPoolParameter
    → Returns codes.InvalidArgument

TestCreateVolume_PoolNotFound
    → PoolManager returns ErrPoolNotFound
    → Returns codes.NotFound

TestCreateVolume_PoolExhausted
    → PoolManager returns ErrPoolExhausted
    → Returns codes.ResourceExhausted

TestCreateVolume_Success
    → Returns correct volume ID, volume context, topology

TestCreateVolume_Idempotent
    → Call twice with same name → same response both times

TestDeleteVolume_MissingVolumeID
    → Returns codes.InvalidArgument

TestDeleteVolume_InvalidVolumeID
    → Returns codes.InvalidArgument

TestDeleteVolume_NotFound
    → PoolManager returns ErrSubVolumeNotFound
    → Returns success (idempotent)

TestDeleteVolume_Success
    → Returns success

TestExpandVolume_Success
    → Returns new capacity, NodeExpansionRequired=false
```

### CSI Node Tests

```
TestNodePublishVolume_ValidatesSubDir
    → subDir="/pvcs/pvc-abc123"         → OK
    → subDir="/../etc/passwd"           → codes.InvalidArgument
    → subDir="/pvcs/../../../etc"       → codes.InvalidArgument
    → subDir=""                         → codes.InvalidArgument
    → subDir="/pvcs/pvc-abc123/../../x" → codes.InvalidArgument

TestNodePublishVolume_BindMountCorrectSource
    → Verify mounter.Mount called with (staging+subDir, targetPath, "bind")

TestNodePublishVolume_ReadOnly
    → Verify mount options include "ro" when req.Readonly=true

TestNodeStageVolume_MountsNFS
    → Verify mounter.Mount called with (server:share, stagingPath, "nfs4", options)

TestNodeStageVolume_Idempotent
    → Already mounted → returns success without mounting again

TestNodeUnpublishVolume_Unmounts
    → Verify mounter.Unmount called with targetPath

TestNodeUnstageVolume_UnmountsNFS
    → Verify mounter.Unmount called with stagingPath
```

### Path Validation Tests

```
TestValidateSubDir_Valid
    → "/pvcs/pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab" → nil

TestValidateSubDir_Traversal
    → "/pvcs/../etc/passwd" → error
    → "/pvcs/pvc-abc/../../" → error
    → "/../../../" → error

TestValidateSubDir_InvalidPattern
    → "/pvcs/not-a-uuid" → error
    → "/something-else/pvc-abc" → error
    → "" → error
    → "/" → error

TestSafeJoin_PreventEscape
    → SafeJoin("/mnt/share", "/pvcs/pvc-abc") → "/mnt/share/pvcs/pvc-abc", nil
    → SafeJoin("/mnt/share", "/../../../etc") → "", error
```

---

## Running Tests

### During Development (Claude Code)

After implementing or modifying any Go file, immediately run:

```bash
# Run all unit tests
go test ./... -v -race -count=1

# Run tests for a specific package
go test ./pkg/pool/... -v -race -count=1

# Run a specific test
go test ./pkg/pool/... -v -race -run TestAllocate_SpreadStrategy

# With coverage
go test ./... -v -race -coverprofile=coverage.out
go tool cover -func=coverage.out
```

**The `-race` flag is mandatory.** The pool manager has concurrent access from multiple CSI gRPC calls. Race detector must pass.

**The `-count=1` flag disables test caching.** During active development, always get fresh results.

### Coverage Expectations

| Package | Minimum Coverage |
|---------|-----------------|
| `pkg/pool/` | 85% — this is the core logic |
| `pkg/driver/` | 75% — mostly delegation + validation |
| `pkg/ibmcloud/` | 60% — hard to unit-test API client deeply |
| `pkg/util/` | 90% — small utility functions, easy to test |
| `api/v1alpha1/` | Generated code, no coverage target |

### CI Pipeline

```yaml
# .github/workflows/test.yaml
name: Test
on: [push, pull_request]
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.22'
      - name: Unit tests
        run: go test ./... -v -race -count=1 -coverprofile=coverage.out
      - name: Check coverage
        run: |
          go tool cover -func=coverage.out
          # Fail if total coverage drops below 70%
          COVERAGE=$(go tool cover -func=coverage.out | grep total | awk '{print $3}' | tr -d '%')
          if (( $(echo "$COVERAGE < 70" | bc -l) )); then
            echo "Coverage ${COVERAGE}% is below 70% threshold"
            exit 1
          fi
      - name: Lint
        uses: golangci/golangci-lint-action@v6
        with:
          version: latest
```

---

## Integration Tests (Optional During Development)

These use a real NFS server in Docker to test actual mount/mkdir/rm operations. They don't need IBM Cloud or Kubernetes.

### Local NFS Server

```bash
# Start a local NFS server in Docker
docker run -d --name nfs-test \
  --privileged \
  -p 2049:2049 \
  -e SHARED_DIRECTORY=/export \
  -e SYNC=true \
  itsthenetwork/nfs-server-alpine:latest

# NFS server is now available at localhost:2049, export path /export
```

### Integration Test Structure

```go
// test/integration/pool_integration_test.go

//go:build integration
// +build integration

package integration

import (
    "testing"
    "os"
)

func TestMain(m *testing.M) {
    // Skip if NFS server not available
    nfsServer := os.Getenv("TEST_NFS_SERVER")
    if nfsServer == "" {
        fmt.Println("Skipping integration tests: TEST_NFS_SERVER not set")
        os.Exit(0)
    }
    os.Exit(m.Run())
}

func TestCreateAndDeleteSubdirectory(t *testing.T) {
    // Mount the NFS share
    // Create a subdirectory
    // Verify it exists
    // Write a file
    // Delete the subdirectory
    // Verify it's gone
}
```

Run with:
```bash
TEST_NFS_SERVER=localhost go test -tags=integration ./test/integration/... -v
```

### Integration tests are NOT required for Claude Code to run during development. They are a nice-to-have for local validation before pushing to a real cluster.

---

## Test File Naming Convention

```
pkg/pool/manager.go           → pkg/pool/manager_test.go
pkg/pool/share.go             → pkg/pool/share_test.go
pkg/driver/controller.go      → pkg/driver/controller_test.go
pkg/driver/node.go            → pkg/driver/node_test.go
pkg/ibmcloud/client.go        → pkg/ibmcloud/client_test.go
pkg/util/mount.go             → pkg/util/mount_test.go
pkg/util/quota.go             → pkg/util/quota_test.go

# Fake implementations (exported, reusable across packages)
pkg/ibmcloud/fake/fake_client.go

# Test-only fakes (unexported, package-local)
pkg/pool/fake_nfs_test.go
```

---

## Golden Rule for Claude Code

**Every function you write must have a corresponding test. Write the test immediately after writing the function, not as a separate task later.** If you find yourself writing code that's hard to test, that's a design signal — extract an interface, inject a dependency, or restructure.

The test must run with `go test ./... -race` and pass. If it doesn't, fix the code or the test before moving on.
