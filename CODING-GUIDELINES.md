# Coding Guidelines

## Language & Toolchain

- **Go 1.22+** (match the version used by kubernetes/kubernetes at the time of development)
- **Module path:** `github.com/IBM/ibm-vpc-file-pool-csi`
- **Linter:** `golangci-lint` with the config below
- **Code generation:** `controller-gen` for CRD types and DeepCopy

## Go Module Dependencies

```bash
# Core CSI
go get github.com/container-storage-interface/spec@v1.10.0
go get google.golang.org/grpc@latest

# Kubernetes
go get k8s.io/client-go@latest
go get k8s.io/apimachinery@latest
go get k8s.io/mount-utils@latest
go get sigs.k8s.io/controller-runtime@latest

# IBM Cloud
go get github.com/IBM/vpc-go-sdk@latest
go get github.com/IBM/go-sdk-core/v5@latest
go get github.com/IBM/secret-common-lib@latest    # Shared auth library (API key + pod identity)

# Utilities
go get golang.org/x/time@latest          # Rate limiting
go get golang.org/x/sys@latest           # unix.Statfs
go get k8s.io/klog/v2@latest             # Structured logging
go get github.com/prometheus/client_golang@latest  # Metrics
```

## Project Layout Conventions

```
cmd/           → Only main.go. Parse flags, wire dependencies, start server. No business logic.
pkg/driver/    → CSI gRPC implementations. Thin handlers that delegate to pkg/pool/.
pkg/pool/      → Core business logic. Pool manager, allocation, deallocation.
pkg/ibmcloud/  → IBM VPC API client. Isolated from K8s and CSI concerns.
pkg/k8s/       → Kubernetes client and CRD reconcilers.
pkg/util/      → Shared utilities (mount helpers, path validation, quota tracking).
api/v1alpha1/  → CRD Go types. No business logic — just types and generated code.
config/        → Kubernetes manifests (CRDs, RBAC, Deployments).
charts/        → Helm chart.
hack/          → Build and codegen scripts.
test/          → Integration and e2e tests (unit tests live next to code).
```

## Code Style

### Naming

- Interfaces: describe behavior, not implementation (`VPCFileClient`, not `IBMVPCFileClientInterface`).
- Constructors: `NewPoolManager(...)`, `NewClient(...)`.
- Errors: `ErrPoolNotFound`, `ErrShareCreationPending`.
- CRD types: match Kubernetes conventions — `FileSharePoolSpec`, `FileSharePoolStatus`.

### Error Handling

```go
// GOOD: Wrap errors with context using %w
return fmt.Errorf("failed to create subdirectory %s on share %s: %w", subDir, shareID, err)

// GOOD: Define sentinel errors for expected conditions
var ErrPoolExhausted = errors.New("pool has no available capacity")

// GOOD: Check with errors.Is
if errors.Is(err, ErrPoolExhausted) {
    return status.Error(codes.ResourceExhausted, err.Error())
}

// BAD: Don't discard error context
return fmt.Errorf("operation failed") // Where? Why?

// BAD: Don't use string matching on errors
if err.Error() == "not found" { ... }
```

### Context

Every function that does I/O (API calls, NFS operations, K8s API) must accept `context.Context` as its first argument and respect cancellation:

```go
func (m *Manager) Allocate(ctx context.Context, req AllocationRequest) (*AllocationResult, error) {
    // Check context before expensive operations
    select {
    case <-ctx.Done():
        return nil, ctx.Err()
    default:
    }
    // ...
}
```

### Logging

Use `klog/v2` structured logging (standard for Kubernetes components):

```go
import "k8s.io/klog/v2"

// Levels:
// klog.V(2) = normal operations (share selected, subdir created)
// klog.V(4) = detailed operations (full request/response)
// klog.V(6) = trace-level (every function entry/exit)

klog.V(2).InfoS("Allocated subvolume",
    "pvName", req.PVName,
    "pool", req.PoolName,
    "shareID", result.ShareID,
    "subPath", result.SubPath,
    "requestedGB", req.RequestedGB,
)

klog.ErrorS(err, "Failed to create subdirectory",
    "shareID", shareID,
    "subDir", subDir,
)

// NEVER log: API keys, secrets, full share credentials
// OK to log: share IDs, PVC names, mount target IPs (these are internal cluster IPs)
```

### Concurrency

The Pool Manager must be safe for concurrent calls from the CSI gRPC server (multiple PVCs created simultaneously):

```go
type Manager struct {
    mu          sync.RWMutex
    pools       map[string]*poolState
    k8sClient   k8s.Client
    vpcClient   ibmcloud.VPCFileClient
}

// Read path — allow concurrent reads
func (m *Manager) getPoolState(poolName string) (*poolState, error) {
    m.mu.RLock()
    defer m.mu.RUnlock()
    // ...
}

// Write path — exclusive lock for allocation
func (m *Manager) Allocate(ctx context.Context, req AllocationRequest) (*AllocationResult, error) {
    m.mu.Lock()
    defer m.mu.Unlock()
    // Pick share, update in-memory tracking, then persist to CRD
    // The CRD update uses optimistic locking (resourceVersion) as a second safety net
}
```

### Path Validation (CRITICAL)

Any path derived from user input or volume context MUST be validated before use:

```go
// pkg/util/path.go

import (
    "fmt"
    "path/filepath"
    "regexp"
    "strings"
)

var validSubDirPattern = regexp.MustCompile(`^/pvcs/pvc-[a-f0-9-]{36}$`)

// ValidateSubDir ensures the subdirectory path is safe.
// Returns an error if the path could be used for directory traversal.
func ValidateSubDir(subDir string) error {
    // Must match our naming pattern
    if !validSubDirPattern.MatchString(subDir) {
        return fmt.Errorf("subDir %q does not match expected pattern", subDir)
    }

    // Must not contain path traversal
    cleaned := filepath.Clean(subDir)
    if cleaned != subDir {
        return fmt.Errorf("subDir %q is not clean (cleaned: %q)", subDir, cleaned)
    }

    if strings.Contains(subDir, "..") {
        return fmt.Errorf("subDir %q contains path traversal", subDir)
    }

    return nil
}

// SafeJoin joins a base path and a subdirectory, ensuring the result is under base.
func SafeJoin(base, sub string) (string, error) {
    if err := ValidateSubDir(sub); err != nil {
        return "", err
    }

    joined := filepath.Join(base, sub)
    // Verify the joined path is still under base
    if !strings.HasPrefix(filepath.Clean(joined), filepath.Clean(base)) {
        return "", fmt.Errorf("path %q escapes base %q", joined, base)
    }

    return joined, nil
}
```

---

## Testing

### Unit Tests

Every package should have `_test.go` files next to the code. Use table-driven tests:

```go
func TestPoolManager_Allocate(t *testing.T) {
    tests := []struct {
        name        string
        pool        *FileSharePool          // Pool state
        existing    []*SubVolume            // Existing allocations
        request     AllocationRequest
        wantShareID string                   // Expected share selection
        wantErr     error
    }{
        {
            name: "allocate to share with most free space (spread strategy)",
            pool: newPool("test-pool", "spread", 2000,
                shareStatus("share-1", 2000, 1500),  // 500 GB free
                shareStatus("share-2", 2000, 200),   // 1800 GB free
            ),
            request:     AllocationRequest{PVName: "pvc-test", PoolName: "test-pool", RequestedGB: 10},
            wantShareID: "share-2",  // Spread picks the emptiest
        },
        {
            name: "allocate to share with least free space (binpack strategy)",
            pool: newPool("test-pool", "binpack", 2000,
                shareStatus("share-1", 2000, 1500),  // 500 GB free
                shareStatus("share-2", 2000, 200),   // 1800 GB free
            ),
            request:     AllocationRequest{PVName: "pvc-test", PoolName: "test-pool", RequestedGB: 10},
            wantShareID: "share-1",  // Binpack picks the fullest that still has room
        },
        {
            name: "fail when pool is exhausted",
            pool: newPool("test-pool", "spread", 2000,
                shareStatus("share-1", 2000, 2000),  // Full
            ),
            request: AllocationRequest{PVName: "pvc-test", PoolName: "test-pool", RequestedGB: 10},
            wantErr: ErrPoolExhausted,
        },
        {
            name: "idempotent - return existing allocation if PV name matches",
            pool: newPool("test-pool", "spread", 2000,
                shareStatus("share-1", 2000, 500),
            ),
            existing: []*SubVolume{
                newSubVolume("pvc-existing", "test-pool", "share-1", 10),
            },
            request:     AllocationRequest{PVName: "pvc-existing", PoolName: "test-pool", RequestedGB: 10},
            wantShareID: "share-1",
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            mgr := newTestManager(tt.pool, tt.existing)
            result, err := mgr.Allocate(context.Background(), tt.request)

            if tt.wantErr != nil {
                if !errors.Is(err, tt.wantErr) {
                    t.Errorf("expected error %v, got %v", tt.wantErr, err)
                }
                return
            }

            if err != nil {
                t.Fatalf("unexpected error: %v", err)
            }

            if result.ShareID != tt.wantShareID {
                t.Errorf("expected share %s, got %s", tt.wantShareID, result.ShareID)
            }
        })
    }
}
```

### Test Helpers

Create helpers in `_test.go` files (not exported):

```go
func newTestManager(pool *FileSharePool, subVolumes []*SubVolume) *Manager {
    fakeVPC := fake.NewFakeClient()
    fakeK8s := fake.NewClientBuilder().
        WithScheme(scheme).
        WithObjects(pool).
        WithStatusSubresource(pool).
        Build()

    for _, sv := range subVolumes {
        fakeK8s.Create(context.Background(), sv)
    }

    return &Manager{
        pools:     map[string]*poolState{pool.Name: stateFromPool(pool)},
        k8sClient: fakeK8s,
        vpcClient: fakeVPC,
    }
}
```

### What to Test

| Component | Must Test |
|-----------|-----------|
| Pool Manager | Allocation strategy (spread vs binpack), capacity tracking, idempotency, concurrent allocation, pool exhaustion, auto-expand trigger |
| CSI Controller | Idempotent CreateVolume, DeleteVolume for missing volume, volume ID parsing, parameter validation |
| CSI Node | Path validation, mount idempotency, bind mount source verification |
| IBM VPC Client | Mock API responses, error mapping, rate limiter behavior |
| Path utilities | Traversal prevention, edge cases (empty string, special chars) |

### What NOT to Test in Unit Tests

- Actual NFS mounts (needs a real NFS server → integration tests)
- Actual IBM VPC API calls (needs credentials → integration tests)
- Actual Kubernetes cluster (use fake client in unit tests)

---

## Metrics

Expose Prometheus metrics for observability:

```go
import "github.com/prometheus/client_golang/prometheus"

var (
    allocationsTotal = prometheus.NewCounterVec(
        prometheus.CounterOpts{
            Name: "vpc_file_pool_allocations_total",
            Help: "Total number of subvolume allocations",
        },
        []string{"pool", "status"}, // status: success, error
    )

    allocationDuration = prometheus.NewHistogramVec(
        prometheus.HistogramOpts{
            Name:    "vpc_file_pool_allocation_duration_seconds",
            Help:    "Time to allocate a subvolume",
            Buckets: prometheus.ExponentialBuckets(0.01, 2, 15), // 10ms to 163s
        },
        []string{"pool"},
    )

    poolCapacityGB = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{
            Name: "vpc_file_pool_capacity_gb",
            Help: "Total pool capacity in GB",
        },
        []string{"pool"},
    )

    poolAllocatedGB = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{
            Name: "vpc_file_pool_allocated_gb",
            Help: "Total allocated capacity in GB",
        },
        []string{"pool"},
    )

    poolShareCount = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{
            Name: "vpc_file_pool_share_count",
            Help: "Number of VPC file shares in the pool",
        },
        []string{"pool"},
    )

    poolPVCCount = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{
            Name: "vpc_file_pool_pvc_count",
            Help: "Number of PVCs in the pool",
        },
        []string{"pool"},
    )

    vpcAPICallsTotal = prometheus.NewCounterVec(
        prometheus.CounterOpts{
            Name: "vpc_file_pool_api_calls_total",
            Help: "Total VPC API calls",
        },
        []string{"operation", "status"}, // operation: create_share, get_share, etc.
    )

    vpcAPICallDuration = prometheus.NewHistogramVec(
        prometheus.HistogramOpts{
            Name:    "vpc_file_pool_api_call_duration_seconds",
            Help:    "VPC API call duration",
            Buckets: prometheus.ExponentialBuckets(0.1, 2, 15), // 100ms to 1638s
        },
        []string{"operation"},
    )
)
```

---

## Dockerfile

```dockerfile
# Build stage
FROM golang:1.22 AS builder

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /vpc-file-pool-csi ./cmd/

# Runtime stage
FROM registry.access.redhat.com/ubi9/ubi-minimal:latest

# Install NFS client utilities (needed by node agent for mounting)
RUN microdnf install -y nfs-utils && microdnf clean all

COPY --from=builder /vpc-file-pool-csi /usr/local/bin/vpc-file-pool-csi

ENTRYPOINT ["/usr/local/bin/vpc-file-pool-csi"]
```

Use UBI (Universal Base Image) for Red Hat / OpenShift compatibility.

---

## Makefile Targets

```makefile
BINARY_NAME := vpc-file-pool-csi
IMAGE_NAME := icr.io/ibm-vpc-file-pool-csi/driver
VERSION := $(shell git describe --tags --always --dirty)

.PHONY: build test lint docker-build generate install-crds deploy

build:
	CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=$(VERSION)" -o bin/$(BINARY_NAME) ./cmd/

test:
	go test ./... -v -race -count=1

test-coverage:
	go test ./... -v -race -coverprofile=coverage.out
	go tool cover -html=coverage.out -o coverage.html

lint:
	golangci-lint run ./...

generate:
	controller-gen object paths="./api/..."
	controller-gen crd paths="./api/..." output:crd:dir=config/crd

docker-build:
	docker build -t $(IMAGE_NAME):$(VERSION) .

install-crds:
	kubectl apply -f config/crd/

deploy: install-crds
	kubectl apply -f config/rbac/
	kubectl apply -f config/deploy/

helm-install:
	helm upgrade --install ibm-vpc-file-pool-csi charts/ibm-vpc-file-pool-csi/ \
		--namespace kube-system

run-local:
	go run ./cmd/ --mode=controller --endpoint=unix:///tmp/csi.sock --dry-run --v=4
```

---

## golangci-lint Configuration

```yaml
# .golangci.yml
linters:
  enable:
    - errcheck
    - govet
    - staticcheck
    - gosimple
    - ineffassign
    - unused
    - misspell
    - gofmt
    - goimports
    - gosec
    - prealloc

linters-settings:
  errcheck:
    check-type-assertions: true
  govet:
    enable-all: true
  gosec:
    excludes:
      - G104 # We handle errors, but some are intentionally ignored in defer
```
