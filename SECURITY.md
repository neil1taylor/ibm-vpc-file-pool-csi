# Security Policy

## Supported Versions

| Version | Supported |
|---------|-----------|
| v0.5.x  | Yes       |
| v0.4.x  | Yes       |
| v0.3.x  | No        |
| v0.2.x  | No        |
| v0.1.x  | No        |

## Reporting a Vulnerability

**Do not open a public GitHub issue for security vulnerabilities.**

To report a vulnerability:

1. **GitHub Private Advisory** (preferred) — go to the repository's **Security** tab and click **Report a vulnerability** to open a private advisory.
2. **Email** — send details to the repository maintainers. Include a description of the vulnerability, steps to reproduce, and the potential impact.

You should receive an acknowledgment within 48 hours. We will work with you to understand the issue and coordinate a fix before any public disclosure.

## Security Design

The IBM VPC File Pool CSI Driver incorporates several security properties by design:

- **Leader election** — the controller uses controller-runtime leader election to prevent concurrent instances from corrupting pool state. Only one controller actively reconciles at a time.
- **Path validation** — all subdirectory operations validate paths against `^/pvcs/pvc-[a-f0-9-]{36}$` and reject directory traversal attempts (`..`, symlinks outside the share root). This prevents a malicious PVC name from escaping the share directory.
- **No secrets in logs** — API keys, tokens, and credentials are never logged. Share IDs, mount target IPs, and pool names are logged for debugging.
- **Soft NFS mounts by default** — mount options default to `soft,timeo=600,retrans=3`. Custom StorageClass mount flags are merged with (not replaced by) these safe defaults, so `soft` is always present unless explicitly overridden with `hard`. This prevents accidental removal of the `soft` safety net.
- **Webhook cert isolation** — webhook TLS certificate secret volumes use `defaultMode: 0400`, restricting private key access to the owner (the controller container).
- **RBAC least-privilege** — the controller ServiceAccount has only the permissions required to watch/update FileSharePool and SubVolume CRs, read ConfigMaps for VPC config, and manage PV/PVC objects. The node agent ServiceAccount is further restricted to node-level operations.
- **Secret isolation** — on managed ROKS/ROVS/IKS clusters, IBM Cloud API keys are injected by the `storage-secret-sidecar` and never stored in user-accessible ConfigMaps or PVC annotations.

## Hardening Checklist

When deploying in production, consider the following hardening measures:

### NFS Encryption in Transit

IBM Cloud VPC File Storage offers optional encryption in transit (IPsec), but it is **not supported** on bare metal servers or RHCOS-based instances (including ROKS and ROVS worker nodes). NFS traffic within the VPC is unencrypted. Rely on VPC network isolation and security groups to protect NFS traffic. See `KNOWN-LIMITATIONS.md` for details.

### Network Policies

Restrict NFS traffic to only the worker nodes and VPC file share mount targets:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-nfs-egress
  namespace: kube-system
spec:
  podSelector:
    matchLabels:
      app: ibm-vpc-file-pool-csi-node
  policyTypes:
    - Egress
  egress:
    - ports:
        - protocol: TCP
          port: 2049
```

### VPC Security Groups

Lock down the security group attached to VPC file share mount targets:

- Allow **TCP 2049** (NFS) inbound only from the worker node subnet CIDR
- Deny all other inbound traffic to mount targets
- Review security group rules after any subnet change

### Pod Security Standards

The image runs as user 65532 by default (`USER` in the Dockerfile). The chart sets a security context on each workload; every value can be overridden (see `HELM-VALUES.md`).

| Workload | Runs as | Hardening | Why |
|----------|---------|-----------|-----|
| Controller (`csi-controller`, includes the webhooks) | non-root (65532, or the OpenShift-assigned user) | no privilege escalation, drop ALL capabilities, `runAsNonRoot`, seccomp `RuntimeDefault` | It needs no root: it talks to the Kubernetes and VPC APIs only |
| Controller sidecars | unchanged | no privilege escalation, drop ALL capabilities | Their user is left alone so the sockets they share keep working |
| Node agent (`csi-node`) | root, privileged | none; `runAsUser: 0` is explicit | It mounts NFS on the host and needs root and mount propagation |
| Node driver registrar | root, unchanged | none | It writes into the root-owned kubelet registration directory |
| Console plugin | 1001 (nginx image) | no privilege escalation, drop ALL, `runAsNonRoot`, seccomp | Already non-root |
| Replication receiver | 107 | no privilege escalation, drop ALL, `runAsNonRoot`, seccomp | 107 is required to own the VM disks |
| Hook Jobs (sidecar gate, console plugin register) | 65534 | already hardened, read-only root | Unchanged |
| Jobs the controller creates (clone, copy, replication, backup, export, restore, Kopia, rsync) | root or the QEMU user (107), set in the Job | none | They read and write the NFS share under `root_squash` and `sec=sys`, which needs root or UID 107, and the OpenShift `anyuid` SCC. Locking them down is not safe without a lab test |

`readOnlyRootFilesystem` is not set on the controller, node agent, console plugin or receiver: it has not been tested that they write nowhere else.

The default image tag is `v` plus the chart `appVersion`, not `latest`.

### Image Pinning

Pin container image tags to digests in production Helm values to prevent supply-chain attacks:

```yaml
image:
  repository: de.icr.io/ibm-vpc-file-pool-csi/driver
  tag: v0.2.0@sha256:<digest>
```

### API Key Rotation

- Rotate IBM Cloud API keys on a regular schedule
- On managed clusters, the `storage-secret-sidecar` handles key refresh automatically
- On self-managed clusters, update the Kubernetes Secret and restart the controller pod
