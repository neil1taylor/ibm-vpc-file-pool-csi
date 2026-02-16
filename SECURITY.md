# Security Policy

## Supported Versions

| Version | Supported |
|---------|-----------|
| v0.2.x  | Yes       |
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
- **Soft NFS mounts** — mount options enforce `soft,timeo=600,retrans=3`. Hard mounts are deliberately not supported because they cause pods to hang indefinitely on NFS server failures.
- **RBAC least-privilege** — the controller ServiceAccount has only the permissions required to watch/update FileSharePool and SubVolume CRs, read ConfigMaps for VPC config, and manage PV/PVC objects. The node agent ServiceAccount is further restricted to node-level operations.
- **Secret isolation** — on managed ROKS/IKS clusters, IBM Cloud API keys are injected by the `storage-secret-sidecar` and never stored in user-accessible ConfigMaps or PVC annotations.

## Hardening Checklist

When deploying in production, consider the following hardening measures:

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

Run the CSI driver pods under the `restricted` Pod Security Standard where possible. The node agent requires `privileged` for mount operations, but the controller does not.

### Image Pinning

Pin container image tags to digests in production Helm values to prevent supply-chain attacks:

```yaml
image:
  repository: icr.io/ibm-vpc-file-pool-csi/driver
  tag: v0.2.0@sha256:<digest>
```

### API Key Rotation

- Rotate IBM Cloud API keys on a regular schedule
- On managed clusters, the `storage-secret-sidecar` handles key refresh automatically
- On self-managed clusters, update the Kubernetes Secret and restart the controller pod
