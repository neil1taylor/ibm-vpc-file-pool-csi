# Contributing to IBM VPC File Pool CSI Driver

Thank you for your interest in contributing. This guide covers everything you need to get started.

## Quick Start

```bash
# 1. Fork and clone
git clone https://github.com/<your-user>/ibm-vpc-file-pool-csi.git
cd ibm-vpc-file-pool-csi

# 2. Build
make build

# 3. Run tests
make test

# 4. Run linter
make lint
```

If all three commands pass, your environment is ready.

## Dev Environment

### Required Tools

| Tool | Version | Purpose |
|------|---------|---------|
| Go | 1.25+ | Build and test |
| golangci-lint | v2 | Linting (install: `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest`) |
| controller-gen | v0.18+ | CRD generation (install: `go install sigs.k8s.io/controller-tools/cmd/controller-gen@latest`) |
| Docker or Podman | Latest | Container image builds |
| kubectl | 1.28+ | Cluster interaction |
| Helm | v3.10+ | Chart testing |

### Known Issue: controller-gen + Go 1.25

`controller-gen` (tested v0.18.0 through v0.20.1) generates incomplete `zz_generated.deepcopy.go` with Go 1.25. Root types get `DeepCopyInto` but nested Spec/Status types do not. Workaround: manually maintain `zz_generated.deepcopy.go` with methods for nested types. `make generate` only runs CRD schema generation, not `controller-gen object`.

### golangci-lint v2

This project uses golangci-lint v2, which has breaking config changes from v1:
- `gofmt` and `goimports` moved from `linters` to `formatters`
- `gosimple` merged into `staticcheck`
- `prealloc` removed

See `.golangci.yml` in the repo root for the current configuration.

## Build & Test

### Make Targets

| Target | Description |
|--------|-------------|
| `make build` | Build binary (`CGO_ENABLED=0`) |
| `make test` | Unit tests with race detector |
| `make test-coverage` | Tests with coverage report |
| `make lint` | Run golangci-lint |
| `make generate` | Regenerate CRD schemas |
| `make docker-build` | Build container image |
| `make deploy` | Apply CRDs + RBAC + manifests |
| `make helm-install` | Helm chart install |
| `make run-local` | Run controller locally (dry-run) |

### Running Specific Tests

```bash
# Single test
go test ./pkg/pool/ -v -race -run TestPoolManager_Allocate

# Single package
go test ./pkg/driver/... -v -race -count=1

# With coverage
go test ./... -v -race -coverprofile=coverage.out
go tool cover -func=coverage.out
```

### Coverage Targets

| Package | Target |
|---------|--------|
| `pkg/pool/` | 85% |
| `pkg/driver/` | 75% |
| `pkg/ibmcloud/` | 60% |
| `pkg/util/` | 90% |

See [TESTING.md](TESTING.md) for the complete testing strategy, fake/mock patterns, and test case specifications.

## Code Style

Follow the conventions in [CODING-GUIDELINES.md](CODING-GUIDELINES.md). Key points:

- **Error handling:** Wrap with `fmt.Errorf("context: %w", err)`, define sentinel errors, check with `errors.Is`
- **Context:** Every I/O function takes `context.Context` as its first argument
- **Logging:** Use `klog/v2` structured logging (`klog.V(2).InfoS`, `klog.ErrorS`)
- **Concurrency:** Pool manager uses `sync.RWMutex` — read path with `RLock`, write path with `Lock`
- **Path validation:** Always validate paths against `^/pvcs/pvc-[a-f0-9-]{36}$` before filesystem operations
- **Security:** Never log API keys or secrets

## Project Structure

Where to put new code:

| You're adding... | Put it in... |
|------------------|-------------|
| CSI gRPC handler logic | `pkg/driver/` |
| Pool allocation/deallocation logic | `pkg/pool/` |
| VPC API operations | `pkg/ibmcloud/` |
| CRD reconciler logic | `pkg/k8s/` |
| Mount helpers, path validation | `pkg/util/` |
| CRD type definitions | `api/v1alpha1/` |
| Kubernetes manifests | `config/` |
| Helm chart changes | `charts/` |
| Integration/e2e tests | `test/` |
| Unit tests | Next to the code (`_test.go`) |

See [SKILL.md](SKILL.md) for detailed guidance on implementing specific types of changes.

## Making Changes

### CRD Type Changes

1. Edit the type definitions in `api/v1alpha1/`
2. Run `make generate` to regenerate CRD schemas
3. Manually update `zz_generated.deepcopy.go` for any new nested types (see the controller-gen caveat above)
4. Update `config/crd/` with the regenerated YAML
5. Update [CRD-SPEC.md](CRD-SPEC.md) to reflect the changes

### New CSI Methods

1. Add the gRPC handler in `pkg/driver/`
2. Delegate business logic to `pkg/pool/` — keep handlers thin
3. Map errors to appropriate gRPC status codes
4. Add unit tests with a mock `PoolManager`

### New VPC API Operations

1. Add the method to the `VPCFileClient` interface in `pkg/ibmcloud/client.go`
2. Implement in the real client
3. Add the method to the fake client in `pkg/ibmcloud/fake/fake_client.go`
4. Add test hooks (error injection) to the fake

## Pull Request Process

### Before Submitting

- [ ] `make build` passes
- [ ] `make test` passes (all tests, with `-race`)
- [ ] `make lint` passes
- [ ] New code has unit tests
- [ ] Coverage targets are met for modified packages
- [ ] `make generate` was run if CRD types changed
- [ ] Documentation updated for user-facing changes

### PR Description

Include:
- **What** changed and **why**
- Link to the issue it addresses (if any)
- Testing done (which tests were added/modified)
- Breaking changes (if any)

### Review Process

1. All PRs require at least one approval
2. CI must pass (tests, lint, coverage)
3. Reviewers will check for adherence to coding guidelines, test coverage, and security concerns (especially path validation and secret handling)

## Commit Conventions

Use imperative mood with a scope prefix:

```
pool: add binpack allocation strategy
driver: fix idempotent CreateVolume for existing SubVolumes
helm: add ServiceMonitor for Prometheus Operator
docs: update USER-GUIDE with multi-zone examples
ibmcloud: handle 429 rate limit with exponential backoff
node: fix stale mount detection in staging cache
test: add concurrent allocation stress test
```

Keep the first line under 72 characters. Add a body for non-trivial changes.

## Reporting Issues

### Bug Reports

Include:
- Cluster type and version (IKS/ROKS, Kubernetes version)
- Driver version (container image tag)
- Steps to reproduce
- Expected vs. actual behavior
- Relevant logs (controller and node agent)
- Pool and PVC configuration (sanitized)

See [TROUBLESHOOTING.md](TROUBLESHOOTING.md) for diagnostic commands to gather this information.

### Feature Requests

Include:
- Use case description
- Proposed behavior
- Any alternative approaches you've considered

## License

By contributing, you agree that your contributions will be licensed under the [Apache License 2.0](LICENSE).
