# Installation Guide — IBM VPC File Pool CSI Driver

## Prerequisites

### Cluster Requirements

- IBM Cloud Kubernetes Service (IKS) or Red Hat OpenShift on IBM Cloud (ROKS), version 1.28+
- VPC Gen2 networking (Classic infrastructure is not supported)
- Worker nodes with `topology.kubernetes.io/zone` and `topology.kubernetes.io/region` labels (set automatically by IBM Cloud)

### IBM Cloud Requirements

- An IBM Cloud account with VPC Infrastructure permissions
- An API key with the following IAM roles:
  - **VPC Infrastructure Services**: Editor or Administrator (to create/manage file shares)
  - **Kubernetes Service**: Editor (if deploying via managed add-on in the future)
- A VPC with at least one subnet in the target availability zone
- Security groups on worker nodes must allow **TCP port 2049** (NFS) inbound and outbound

### Tooling

- `kubectl` or `oc` CLI configured against your cluster
- `helm` v3.10+ (if using Helm installation)
- `docker` or `podman` (to build the driver image)
- `ibmcloud` CLI with the Container Registry plugin (to push the image)

### Verify Prerequisites

```bash
# Confirm cluster access
kubectl get nodes -o wide

# Check node labels (zone must be populated)
kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.metadata.labels.topology\.kubernetes\.io/zone}{"\n"}{end}'

# Verify NFS port is open in security group
# (check via IBM Cloud console or CLI — the VPC security group
# attached to worker nodes must have TCP 2049 allowed)
ibmcloud is security-groups --output json | jq '.[] | select(.name | contains("kube")) | .rules[] | select(.port_min == 2049)'
```

---

## Step 1: Build and Push the Driver Image

```bash
# Clone the repository
git clone https://github.com/IBM/ibm-vpc-file-pool-csi.git
cd ibm-vpc-file-pool-csi

# Build the binary
make build

# Build the container image
export REGISTRY=icr.io
export NAMESPACE=your-icr-namespace
export VERSION=$(git describe --tags --always)

make docker-build IMAGE_NAME=${REGISTRY}/${NAMESPACE}/vpc-file-pool-csi:${VERSION}

# Login to IBM Cloud Container Registry
ibmcloud cr login

# Push
docker push ${REGISTRY}/${NAMESPACE}/vpc-file-pool-csi:${VERSION}
```

If you're iterating locally and have a private registry or use OpenShift's internal registry:

```bash
# OpenShift internal registry
oc registry login
docker tag vpc-file-pool-csi:latest default-route-openshift-image-registry.apps.<cluster>/kube-system/vpc-file-pool-csi:${VERSION}
docker push default-route-openshift-image-registry.apps.<cluster>/kube-system/vpc-file-pool-csi:${VERSION}
```

---

## Step 2: Verify Credentials

The driver uses `secret-common-lib` to authenticate — the same library used by the stock IBM CSI drivers. This means it reads credentials from secrets that already exist on your cluster.

### Managed Clusters (ROKS / IKS) — Nothing to Do

The `storage-secret-store` secret is pre-populated by the IBM Cloud add-on system:

```bash
# Verify the credentials already exist
kubectl get secret storage-secret-store -n kube-system
kubectl get configmap ibm-cloud-provider-data -n kube-system
```

If both exist, skip to Step 3. The driver will use these automatically.

### Self-Managed Clusters — Create Credentials

If you're running on a self-managed OpenShift or Kubernetes cluster, create the credentials manually:

```bash
# Create a service ID with VPC Infrastructure Editor
ibmcloud iam service-id-create vpc-file-pool-csi \
  --description "Service ID for VPC File Pool CSI Driver"
ibmcloud iam service-policy-create vpc-file-pool-csi \
  --roles Editor --service-name is

# Create an API key
ibmcloud iam service-api-key-create vpc-file-pool-csi-key vpc-file-pool-csi \
  --description "API key for VPC File Pool CSI Driver" \
  --output json

# Store as a Kubernetes secret (newer ibm-cloud-credentials format)
cat > /tmp/ibm-credentials.env << 'EOF'
IBMCLOUD_AUTHTYPE=iam
IBMCLOUD_APIKEY=<paste-api-key-here>
EOF

kubectl create secret generic ibm-cloud-credentials \
  --namespace kube-system \
  --from-file=ibm-credentials.env=/tmp/ibm-credentials.env
rm /tmp/ibm-credentials.env

# Create the provider-data configmap
kubectl create configmap ibm-cloud-provider-data \
  --namespace kube-system \
  --from-literal=vpcID=<your-vpc-id> \
  --from-literal=subnetID=<your-subnet-id>
```

For pod identity (Trusted Profiles) setup or more details, see `API-KEY-SETUP.md`.

---

## Step 3: Install the Driver

### Option A: Helm Chart (Recommended)

```bash
# Add the chart repo (if published) or install from local checkout
helm upgrade --install ibm-vpc-file-pool-csi \
  charts/ibm-vpc-file-pool-csi/ \
  --namespace kube-system \
  --set image.repository=${REGISTRY}/${NAMESPACE}/vpc-file-pool-csi \
  --set image.tag=${VERSION} \
  --set config.region=us-south \
  --set config.vpcID=<YOUR_VPC_ID> \
  --set config.subnetID=<YOUR_SUBNET_ID> \
  --set config.resourceGroupID=<YOUR_RESOURCE_GROUP_ID>
```

**Helm values reference:**

| Value | Required | Default | Description |
|-------|----------|---------|-------------|
| `image.repository` | Yes | — | Container image registry path |
| `image.tag` | Yes | — | Image tag |
| `image.pullPolicy` | No | `IfNotPresent` | Image pull policy |
| `config.region` | Yes | — | IBM Cloud region (e.g., `us-south`) |
| `config.vpcID` | Yes | — | VPC ID where shares will be created |
| `config.subnetID` | Yes | — | Subnet ID for NFS mount targets |
| `config.resourceGroupID` | No | Account default | Resource group for billing |
| `secret.name` | No | `ibm-vpc-file-pool-csi-secret` | Name of the API key secret |
| `secret.namespace` | No | `kube-system` | Namespace of the API key secret |
| `controller.replicas` | No | `2` | Controller replicas (leader-elected) |
| `controller.resources.requests.cpu` | No | `100m` | Controller CPU request |
| `controller.resources.requests.memory` | No | `128Mi` | Controller memory request |
| `node.resources.requests.cpu` | No | `50m` | Node agent CPU request |
| `node.resources.requests.memory` | No | `64Mi` | Node agent memory request |
| `logLevel` | No | `2` | klog verbosity (2=normal, 4=detailed, 6=trace) |

### Option B: Raw Manifests

```bash
# 1. Install CRDs
kubectl apply -f config/crd/

# 2. Install RBAC
kubectl apply -f config/rbac/

# 3. Edit the deployment manifests with your image and config
#    (or use kustomize — a kustomization.yaml is provided)
export IMAGE=${REGISTRY}/${NAMESPACE}/vpc-file-pool-csi:${VERSION}

# Using sed for quick substitution:
sed -i "s|image: .*vpc-file-pool-csi.*|image: ${IMAGE}|g" config/deploy/controller.yaml config/deploy/node.yaml

# 4. Deploy
kubectl apply -f config/deploy/csidriver.yaml
kubectl apply -f config/deploy/controller.yaml
kubectl apply -f config/deploy/node.yaml

# 5. Install default StorageClasses
kubectl apply -f config/deploy/storageclass.yaml
```

---

## Step 4: Create a File Share Pool

The driver won't provision any PVCs until at least one `FileSharePool` exists. Create one for your target zone (see also `examples/basic/pool.yaml` for a ready-to-use template):

```yaml
# pool-general.yaml
apiVersion: storage.ibmcloud.io/v1alpha1
kind: FileSharePool
metadata:
  name: general-purpose
spec:
  zone: us-south-1                   # Must match your worker node zone
  profile: dp2                        # VPC file storage profile
  shareSizeGB: 2000                   # 2 TB per share
  maxShares: 10                       # Up to 10 shares (20 TB total capacity)
  initialShares: 1                    # Create 1 share immediately
  autoExpand: true                    # Auto-create shares when pool fills
  expandThresholdPercent: 80          # Trigger at 80% allocated
  allocationStrategy: spread          # Distribute PVCs evenly across shares
  defaultPermissions: "0755"
  defaultUID: 1000
  defaultGID: 1000
  resourceGroup: "your-resource-group-id"
  tags:
    - "env:production"
    - "managed-by:file-pool-csi"
```

```bash
kubectl apply -f pool-general.yaml
```

Wait for the pool to initialize (the first VPC file share takes 30-90 seconds to create):

```bash
# Watch pool status
kubectl get filesharepools -w

# Detailed status
kubectl get filesharespool general-purpose -o yaml
```

The pool is ready when `status.phase` is `Ready` and at least one share shows `state: stable`.

---

## Step 5: Verify the Installation

### Check all components are running

```bash
# CSI Driver registered
kubectl get csidriver vpc-file-pool.csi.ibm.io

# Controller pod (should be Running, 1/1 or 2/2)
kubectl get pods -n kube-system -l app=vpc-file-pool-csi-controller

# Node agent pods (one per worker node, all Running)
kubectl get pods -n kube-system -l app=vpc-file-pool-csi-node

# CRDs installed
kubectl get crd filesharepools.storage.ibmcloud.io
kubectl get crd subvolumes.storage.ibmcloud.io

# Pool is ready
kubectl get filesharepools

# StorageClass available
kubectl get storageclass ibm-vpc-file-pool
```

### Smoke test with a PVC

```bash
# Create a test PVC
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: test-pool-pvc
  namespace: default
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: ibm-vpc-file-pool
  resources:
    requests:
      storage: 1Gi
EOF

# Should bind within seconds (not minutes!)
kubectl get pvc test-pool-pvc -w

# Check a SubVolume was created
kubectl get subvolumes

# Create a test pod that writes to the PVC
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: test-pool-pod
  namespace: default
spec:
  containers:
    - name: test
      image: busybox
      command: ["sh", "-c", "echo 'Hello from pool CSI' > /data/test.txt && cat /data/test.txt && sleep 3600"]
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: test-pool-pvc
EOF

# Verify the pod is running and the file was written
kubectl logs test-pool-pod

# Clean up
kubectl delete pod test-pool-pod
kubectl delete pvc test-pool-pvc
```

If the PVC binds and the pod writes successfully, the installation is complete.

---

## Upgrading

### Helm

```bash
helm upgrade ibm-vpc-file-pool-csi \
  charts/ibm-vpc-file-pool-csi/ \
  --namespace kube-system \
  --set image.tag=${NEW_VERSION}
```

The controller Deployment will rolling-update. The node DaemonSet will update node-by-node. Existing mounts are unaffected — they are kernel-level NFS mounts that survive pod restarts.

### Raw Manifests

```bash
# Update image in manifests, then:
kubectl apply -f config/deploy/controller.yaml
kubectl apply -f config/deploy/node.yaml
```

### CRD Upgrades

If the new version introduces CRD field changes:

```bash
# Apply updated CRDs (additive changes are safe)
kubectl apply -f config/crd/

# For breaking changes, check the release notes and migration guide
```

---

## Uninstalling

```bash
# 1. Delete all PVCs using the pool StorageClass first
kubectl delete pvc -l storageClassName=ibm-vpc-file-pool --all-namespaces

# 2. Wait for all SubVolumes to be cleaned up
kubectl get subvolumes  # Should be empty

# 3. Delete the FileSharePool(s)
#    This will delete the underlying VPC file shares
kubectl delete filesharespool --all

# 4. Uninstall the driver
helm uninstall ibm-vpc-file-pool-csi -n kube-system
# or
kubectl delete -f config/deploy/
kubectl delete -f config/rbac/

# 5. Remove CRDs (only if no resources remain)
kubectl delete -f config/crd/

# 6. Remove the API key secret
kubectl delete secret ibm-vpc-file-pool-csi-secret -n kube-system
```

**Warning:** Deleting a FileSharePool will delete all VPC file shares in the pool and all data on them. Ensure all PVCs are deleted and data is backed up before removing a pool.

---

## Installation Troubleshooting

If something goes wrong during installation, start with these two quick checks:

### Pool stuck in Initializing

```bash
kubectl describe filesharespool <pool-name>
kubectl logs -n kube-system -l app=vpc-file-pool-csi-controller -c csi-controller --tail=100 | grep -i "error\|fail"
```
Common causes: wrong zone, invalid profile, VPC quota exceeded, TCP 2049 blocked, API key permissions.

### Driver pods CrashLoopBackOff

```bash
kubectl logs -n kube-system <pod-name> -c csi-controller --previous
```
Common causes: missing API key secret, wrong image tag, RBAC misconfiguration.

For the full troubleshooting guide (PVC issues, mount failures, pool problems, VPC API errors, and more), see [TROUBLESHOOTING.md](TROUBLESHOOTING.md).
