# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [v0.2.0] — 2026-02-16

### Added

- **Share tiering** for multi-performance pools — assign `dp2`, `custom`, or other VPC file share profiles to tiers within a single FileSharePool, enabling mixed-performance workloads
- **Per-PVC usage reporting** in `NodeGetVolumeStats` — returns `du`-based per-subdirectory usage via the CSI `NodeGetVolumeStats` RPC, surfacing storage consumption in `kubectl describe pvc`
- **Prometheus metrics and soft quota alerting** — exports pool utilization, allocation latency, share count, and PVC count metrics on the `/metrics` endpoint; includes PrometheusRule alerts at configurable utilization thresholds
- **Auto-discovery of VPC config** — region, VPC ID, and subnet ID are automatically read from the `ibm-cloud-provider-data` ConfigMap and secret provider endpoint on managed ROKS/IKS clusters, eliminating manual Helm value overrides
- **Troubleshooting guide** (`TROUBLESHOOTING.md`) — comprehensive runbook covering mount failures, stuck PVCs, capacity issues, VPC API errors, and node problems
- **Performance tuning guide** (`PERFORMANCE-TUNING.md`) — NFS mount option tuning, IOPS planning by profile, benchmarking procedures, and kernel parameter recommendations
- **Contributing guide** (`CONTRIBUTING.md`) — developer setup, PR process, coding standards, and DCO sign-off requirements
- **Ready-to-use examples** (`examples/`) — YAML manifests for basic PVC, StatefulSet, multi-pool, tiered storage, and monitoring configurations
- **MkDocs Material documentation site** with GitHub Pages deployment — full docs site with search, dark mode, and tabbed navigation
- **Phase 4 roadmap** — OpenShift Virtualization Ready milestones: snapshots, volume cloning, group snapshots, and cross-region DR

### Changed

- Centralized glossary into README for single-source definitions of Pool, Share, SubVolume, and related terms
- "Why Pooling?" section added to README explaining the cost, quota, and speed advantages over stock 1:1 CSI mapping

## [v0.1.0] — 2026-02-15

### Added

- **CSI Controller** (`pkg/driver/controller.go`) — gRPC server implementing `CreateVolume`, `DeleteVolume`, `ValidateVolumeCapabilities`, `ControllerExpandVolume`, and capability RPCs; delegates all allocation logic to the Pool Manager
- **CSI Node Agent** (`pkg/driver/node.go`) — `NodeStageVolume` mounts NFS shares once per node, `NodePublishVolume` bind-mounts subdirectories into pod paths, with mount caching to avoid redundant NFS mounts
- **Pool Manager** (`pkg/pool/manager.go`) — core allocation engine with `Allocate()` and `Deallocate()` methods; supports `spread` (most free space) and `binpack` (least free space) share selection strategies; tracks share capacity via SubVolume CRs
- **FileSharePool reconciler** (`pkg/pool/reconciler.go`) — controller-runtime reconciler watching FileSharePool CRs for background pool management: creates initial shares, handles auto-expansion when utilization exceeds threshold, and updates pool status
- **IBM VPC API client** (`pkg/ibmcloud/client.go`) — thin wrapper around `vpc-go-sdk` for file share CRUD, mount target management, and share capacity expansion; all operations idempotent with context-based timeouts
- **FileSharePool CRD** — cluster-scoped custom resource defining pool configuration: zone, profile, share size, max shares, allocation strategy, auto-expansion, and tier definitions
- **SubVolume CRD** — cluster-scoped custom resource tracking individual PVC allocations: share reference, subdirectory path, requested size, and pool membership
- **Helm chart** (`charts/ibm-vpc-file-pool-csi/`) — deploys controller Deployment, node DaemonSet, CSI sidecars (provisioner, resizer, registrar, liveness probe), RBAC, and StorageClass
- **Storage-secret-sidecar integration** — automatic IBM Cloud API key injection on managed ROKS/IKS clusters using `armada-storage-secret` sidecar
- **Path validation** — all subdirectory operations validate against `^/pvcs/pvc-[a-f0-9-]{36}$` regex and reject path traversal attempts
- **Leader election** — controller uses controller-runtime leader election to prevent concurrent pool state corruption

### Fixed

- VPC API endpoint construction for production IBM Cloud regions
- RBAC permissions for FileSharePool and SubVolume CR watch/update
- Subdirectory creation race condition during concurrent `NodePublishVolume` calls
- Mount target IP resolution when share has multiple mount targets across zones
- Makefile targets for end-to-end build pipeline

[v0.2.0]: https://github.com/IBM/ibm-vpc-file-pool-csi/compare/v0.1.0...v0.2.0
[v0.1.0]: https://github.com/IBM/ibm-vpc-file-pool-csi/releases/tag/v0.1.0
