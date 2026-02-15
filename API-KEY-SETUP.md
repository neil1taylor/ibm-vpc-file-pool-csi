# Authentication & Credentials Setup

## Overview

The CSI driver authenticates to IBM Cloud using `secret-common-lib`, the same library used by the stock IBM block and file CSI drivers. This means:

- **On managed ROKS/IKS clusters:** Credentials already exist. No setup required — the driver reuses the cluster's existing `storage-secret-store` or `ibm-cloud-credentials` secret.
- **On self-managed clusters:** You create an `ibm-cloud-credentials` secret manually (one-time setup).

The library supports two authentication methods: API keys (traditional) and pod identity via Trusted Profiles (no long-lived secrets).

---

## Managed Clusters (ROKS / IKS) — Zero Setup

On managed ROKS/IKS clusters, the `storage-secret-store` secret in `kube-system` is pre-populated by the IBM Cloud add-on system. It contains the API key and VPC endpoint configuration that the stock IBM CSI drivers already use.

Our driver reads the same secret via `secret-common-lib`. There is nothing to create.

### Verify the credentials exist

```bash
# Check for the legacy secret (present on all managed clusters)
kubectl get secret storage-secret-store -n kube-system

# Check for the newer secret (may or may not exist yet)
kubectl get secret ibm-cloud-credentials -n kube-system

# Check the cluster configmaps
kubectl get configmap ibm-cloud-provider-data -n kube-system
kubectl get configmap ibm-cloud-cluster-info -n kube-system
```

If `storage-secret-store` exists, the driver will work. That's it.

### How It Works on Managed Clusters

1. The IBM Cloud add-on system creates `storage-secret-store` with a TOML config containing the API key, IAM endpoint, and VPC endpoint.
2. Our driver initializes `secret-common-lib` with `IKS_ENABLED=true` (managed mode).
3. The library runs a sidecar that watches the secret, exchanges the API key for IAM tokens, caches them, and auto-refreshes before expiry.
4. The driver calls `GetDefaultIAMToken()` before each VPC API operation to get a valid bearer token.

The managed provider also supports automatic token refresh without pod restarts when the API key changes.

---

## Self-Managed Clusters (OCP / Kubernetes) — Manual Setup

On self-managed clusters, you need to create the credentials secret yourself.

### Option A: API Key Authentication (Simpler)

```bash
# 1. Create a Service ID with VPC Infrastructure Editor permissions
ibmcloud iam service-id-create vpc-file-pool-csi \
  --description "Service ID for IBM VPC File Pool CSI Driver"

ibmcloud iam service-policy-create vpc-file-pool-csi \
  --roles Editor \
  --service-name is

# 2. Create an API key
ibmcloud iam service-api-key-create vpc-file-pool-csi-key vpc-file-pool-csi \
  --description "API key for VPC File Pool CSI Driver" \
  --output json

# 3. Create the Kubernetes secret (use the newer ibm-cloud-credentials format)
cat > /tmp/ibm-credentials.env << 'EOF'
IBMCLOUD_AUTHTYPE=iam
IBMCLOUD_APIKEY=<paste-api-key-here>
EOF

kubectl create secret generic ibm-cloud-credentials \
  --namespace kube-system \
  --from-file=ibm-credentials.env=/tmp/ibm-credentials.env

rm /tmp/ibm-credentials.env

# 4. Create the provider-data configmap
kubectl create configmap ibm-cloud-provider-data \
  --namespace kube-system \
  --from-literal=vpcID=<your-vpc-id> \
  --from-literal=subnetID=<your-subnet-id>
```

### Option B: Pod Identity with Trusted Profiles (No Long-Lived Keys)

Trusted Profiles eliminate the need for a static API key. The driver authenticates using a Kubernetes service account token that IBM Cloud IAM validates.

```bash
# 1. Create a Trusted Profile in IBM Cloud
ibmcloud iam tp-create vpc-file-pool-csi \
  --description "Trusted Profile for VPC File Pool CSI Driver"

# 2. Create a claim rule linking it to the Kubernetes service account
ibmcloud iam tp-rule-create vpc-file-pool-csi \
  --type Profile-SAML \
  --conditions <conditions-json>
# (see IBM docs for the exact claim rule format for your cluster)

# 3. Assign VPC Infrastructure Editor policy to the Trusted Profile
ibmcloud iam tp-policy-create vpc-file-pool-csi \
  --roles Editor \
  --service-name is

# 4. Create the Kubernetes secret with pod identity auth type
cat > /tmp/ibm-credentials.env << 'EOF'
IBMCLOUD_AUTHTYPE=pod-identity
IBMCLOUD_PROFILEID=<trusted-profile-id>
EOF

kubectl create secret generic ibm-cloud-credentials \
  --namespace kube-system \
  --from-file=ibm-credentials.env=/tmp/ibm-credentials.env

rm /tmp/ibm-credentials.env
```

The controller Deployment also needs a projected service account token:

```yaml
volumes:
  - name: vault-token
    projected:
      sources:
        - serviceAccountToken:
            path: vault-token
            expirationSeconds: 600
containers:
  - name: csi-controller
    volumeMounts:
      - mountPath: /var/run/secrets/tokens
        name: vault-token
```

Pod identity is the preferred approach for production because there are no static credentials to rotate or leak.

---

## Minimum IAM Permissions

Regardless of auth method (API key or Trusted Profile), the identity needs:

| IAM Service | Role | Why |
|-------------|------|-----|
| VPC Infrastructure Services (`is`) | **Editor** | Create, read, update, delete file shares and mount targets |

The driver does NOT need:

- **Administrator** — it doesn't manage security groups, subnets, or access policies
- **Kubernetes Service roles** — it doesn't interact with the IKS/ROKS control plane
- **Cloud Object Storage roles** — no object storage involved
- **Key Protect / HPCS roles** — unless using customer-managed encryption keys (then add Reader on the KMS instance)

---

## API Key Rotation

### Managed Clusters

On managed ROKS/IKS clusters, the `storage-secret-store` is managed by the IBM Cloud add-on system. If the cluster's API key is rotated (e.g., via `ibmcloud ks api-key reset`), the add-on system updates the secret automatically.

With the managed secret provider (`IKS_ENABLED=true`), the sidecar detects the secret change and refreshes tokens without a pod restart.

### Self-Managed Clusters

```bash
# 1. Create a new API key
ibmcloud iam service-api-key-create vpc-file-pool-csi-key-new vpc-file-pool-csi \
  --description "Rotated key $(date +%Y-%m-%d)" \
  --output json

# 2. Update the credentials env file
cat > /tmp/ibm-credentials.env << 'EOF'
IBMCLOUD_AUTHTYPE=iam
IBMCLOUD_APIKEY=<new-api-key>
EOF

# 3. Update the Kubernetes secret
kubectl create secret generic ibm-cloud-credentials \
  --namespace kube-system \
  --from-file=ibm-credentials.env=/tmp/ibm-credentials.env \
  --dry-run=client -o yaml | kubectl apply -f -

rm /tmp/ibm-credentials.env

# 4. Restart the controller (unmanaged provider doesn't auto-detect changes)
kubectl rollout restart deployment -n kube-system vpc-file-pool-csi-controller

# 5. Verify and delete the old key
kubectl logs -n kube-system -l app=vpc-file-pool-csi-controller -c csi-controller --tail=20
ibmcloud iam service-api-key-delete vpc-file-pool-csi-key vpc-file-pool-csi
```

**Pod identity doesn't need rotation** — the Kubernetes service account token is short-lived (600 seconds) and automatically refreshed by the kubelet.

---

## What Happens When Credentials Are Invalid

| Scenario | Existing Mounts | New PVCs | Pool Reconciliation |
|----------|-----------------|----------|---------------------|
| API key expired/deleted | **Unaffected** | Fail (Pending) | Fails (no new shares) |
| Wrong IAM permissions | **Unaffected** | Fail (403 on share create) | Fails |
| Secret deleted from K8s | **Unaffected** | Fail | Fails |
| Pod identity profile revoked | **Unaffected** | Fail | Fails |
| IAM service outage | **Unaffected** | Fail (retry with backoff) | Fails (retry) |

**Key point:** Credential issues never affect running pods. NFS mounts are kernel-level and have no ongoing dependency on the IBM Cloud API. Credentials are only needed when the pool manager creates, expands, or deletes VPC file shares.

### Diagnosing Credential Issues

```bash
# Check controller logs for auth errors
kubectl logs -n kube-system -l app=vpc-file-pool-csi-controller -c csi-controller --tail=50 | grep -i "auth\|token\|401\|403\|iam"

# Verify the secret exists
kubectl get secret -n kube-system ibm-cloud-credentials storage-secret-store 2>/dev/null

# Test the API key manually
ibmcloud login --apikey <key>
ibmcloud is shares
```

---

## Private Network Access

`secret-common-lib` automatically uses private endpoints when configured in the credentials:

**Via `storage-secret-store`** (TOML format — managed clusters):
```toml
[VPC]
g2_riaas_endpoint_url = "https://us-south.private.iaas.cloud.ibm.com/v1"
g2_token_exchange_endpoint_url = "https://private.iam.cloud.ibm.com"
```

**Via `cloud-conf` configmap:**
```json
{
  "riaas_private_endpoint": "https://us-south.private.iaas.cloud.ibm.com",
  "token_exchange_url": "https://private.iam.cloud.ibm.com"
}
```

The driver calls `secretProvider.GetPrivateRIAASEndpoint()` and uses it if available, falling back to the public endpoint.

---

## Security Best Practices

1. **Use pod identity (Trusted Profiles) in production** — eliminates static API keys entirely. No rotation, no leakage risk.

2. **On managed clusters, don't create a separate key** — reuse `storage-secret-store`. Fewer secrets = smaller attack surface.

3. **On self-managed clusters, use a Service ID** — never a personal API key. Service IDs can be scoped and revoked independently.

4. **Scope IAM policies tightly** — Editor on VPC Infrastructure is the minimum. If resource-type scoping to `share` is available, use it.

5. **Never log credentials** — the controller MUST never log API keys or tokens. Log the secret name/namespace for debugging, never the contents.

6. **Restrict secret RBAC** — only the controller's ServiceAccount should have `get` access to credential secrets.

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: vpc-file-pool-csi-secret-reader
  namespace: kube-system
rules:
  - apiGroups: [""]
    resources: ["secrets"]
    resourceNames: ["ibm-cloud-credentials", "storage-secret-store"]
    verbs: ["get", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: vpc-file-pool-csi-secret-reader
  namespace: kube-system
subjects:
  - kind: ServiceAccount
    name: vpc-file-pool-csi-controller
    namespace: kube-system
roleRef:
  kind: Role
  name: vpc-file-pool-csi-secret-reader
  apiGroup: rbac.authorization.k8s.io
```

7. **Use private endpoints** — route IAM and VPC API traffic over the IBM private network.

8. **Enable Activity Tracker** — audit all VPC API calls made by the driver's identity.
