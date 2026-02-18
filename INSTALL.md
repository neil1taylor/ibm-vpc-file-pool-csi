# Installation Guide — IBM VPC File Pool CSI Driver

## What This Driver Does

In Kubernetes, applications request storage using **Persistent Volume Claims (PVCs)**. Normally, each PVC creates its own dedicated VPC file share — which is slow (30-90 seconds per share) and wastes VPC quota.

This CSI driver takes a different approach: it creates a **pool** of large VPC file shares up front, then carves out subdirectories for each PVC. Think of it like a shared hard drive where each application gets its own folder. PVCs bind in seconds instead of minutes, and many PVCs share a single VPC file share.

A **CSI driver** (Container Storage Interface) is a plugin that teaches Kubernetes how to provision and mount a specific type of storage. This one teaches Kubernetes how to use IBM VPC file shares in a pooled way.

## Installation Overview

The end-to-end process from source code to a working driver on your cluster:

1. **`make build`** — compile the Go source code into a binary
2. **`docker build` / `podman build`** — package the binary into a container image
3. **`docker push` / `podman push`** — upload the image to a container registry so your cluster can pull it
4. **`helm upgrade --install`** — deploy the driver onto the cluster (creates controller, node agents, RBAC, etc.)
5. **`make test-e2e`** — run automated tests against the live driver to verify everything works

Each step is covered in detail below.

---

## Prerequisites

### Cluster Requirements

- **IBM Cloud Kubernetes Service (IKS)** or **Red Hat OpenShift on IBM Cloud (ROKS)**, version 1.28+
- **VPC Gen2 networking** (Classic infrastructure is not supported)
- Worker nodes with `topology.kubernetes.io/zone` and `topology.kubernetes.io/region` labels (set automatically by IBM Cloud — you don't need to do anything)

### IBM Cloud Requirements

- An IBM Cloud account with VPC Infrastructure permissions
- An API key with the following IAM roles:
  - **VPC Infrastructure Services**: Editor or Administrator (to create/manage file shares)
  - **Kubernetes Service**: Editor (if deploying via managed add-on in the future)
- A VPC with at least one subnet in the target availability zone
- Security groups on worker nodes must allow **TCP port 2049** (NFS) inbound and outbound — this is the port that NFS (Network File System) uses to share files over the network

### cert-manager (for Admission Webhooks)

The driver uses **admission webhooks** — these are Kubernetes guards that validate your configuration before accepting it (e.g., rejecting a FileSharePool with an invalid zone). Webhooks require TLS certificates for secure communication. [cert-manager](https://cert-manager.io/) automates certificate creation:

```bash
# Install cert-manager if not already present
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml

# Verify it's running (all pods should show STATUS=Running)
kubectl get pods -n cert-manager
```

If you prefer to manage webhook TLS certificates manually, set `webhook.certProvider=manual` in the Helm values and populate the `ibm-vpc-file-pool-csi-webhook-certs` Secret yourself. To disable webhooks entirely, set `webhook.enabled=false`.

### Tooling

You'll need these command-line tools installed on your workstation:

| Tool | What it does | Install |
|------|-------------|---------|
| `kubectl` or `oc` | Talks to your Kubernetes/OpenShift cluster | [kubectl](https://kubernetes.io/docs/tasks/tools/), [oc](https://docs.openshift.com/container-platform/latest/cli_reference/openshift_cli/getting-started-cli.html) |
| `helm` v3.10+ | Package manager for Kubernetes — installs the driver and all its components in one command | [helm](https://helm.sh/docs/intro/install/) |
| `docker` or `podman` | Builds container images from source code | [docker](https://docs.docker.com/get-docker/), [podman](https://podman.io/getting-started/installation) |
| `ibmcloud` CLI | Authenticates with IBM Cloud Container Registry so you can push images | [ibmcloud](https://cloud.ibm.com/docs/cli) |
| `make` | Runs predefined build commands from the project's `Makefile` — saves you from typing long `go build` commands | Pre-installed on macOS/Linux |

### Verify Prerequisites

Before starting, confirm you can reach your cluster and that it's properly configured:

```bash
# Confirm cluster access — this lists your worker nodes
kubectl get nodes -o wide

# Check node labels — each node must have a zone label so the driver
# knows which availability zone to create file shares in
kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.metadata.labels.topology\.kubernetes\.io/zone}{"\n"}{end}'

# Verify NFS port is open in security group
# The VPC security group attached to worker nodes must have TCP 2049 allowed,
# otherwise nodes won't be able to mount the NFS file shares
ibmcloud is security-groups --output json | jq '.[] | select(.name | contains("kube")) | .rules[] | select(.port_min == 2049)'
```

---

## Step 1: Build and Push the Driver Image

**Why this step?** Kubernetes runs applications as **container images** — lightweight, self-contained packages that include the application and everything it needs to run. We need to:
1. **Compile** the Go source code into a binary (`make build`)
2. **Package** that binary into a container image (`docker build` or `podman build`)
3. **Push** the image to a registry where your cluster can download it

`make build` is a shortcut that runs the Go compiler with the right flags. The project's `Makefile` defines these shortcuts so you don't need to remember long commands.

```bash
# Clone the repository (skip if you already have it)
git clone https://github.com/IBM/ibm-vpc-file-pool-csi.git
cd ibm-vpc-file-pool-csi

# Compile the Go source code into a binary
make build
```

Set your image coordinates — these tell the build tools where to store the image:

```bash
# The container registry to push to. IBM Cloud Container Registry (ICR)
# has regional endpoints: icr.io (US), de.icr.io (EU), au.icr.io (AP), etc.
export REGISTRY=icr.io

# Your ICR namespace (a folder in the registry to organize your images)
export NAMESPACE=your-icr-namespace

# A version tag based on the current git commit — ensures each build is unique
export VERSION=$(git describe --tags --always)

# The full image name including registry, namespace, and version tag
export IMAGE=${REGISTRY}/${NAMESPACE}/vpc-file-pool-csi:${VERSION}
```

Build the container image using `docker` or `podman` (they do the same thing — `podman` doesn't need a background daemon, which is useful on macOS):

```bash
# Option A: docker
docker build -t ${IMAGE} .

# Option B: podman (macOS, Fedora, RHEL — no Docker daemon required)
podman build -t ${IMAGE} .

# You can also use the Makefile target (auto-detects docker or podman):
#   make docker-build IMAGE_NAME=${REGISTRY}/${NAMESPACE}/vpc-file-pool-csi VERSION=${VERSION}
# Note: Do NOT include a tag in IMAGE_NAME — the Makefile appends :$(VERSION) automatically.
```

Push the image to IBM Cloud Container Registry so your cluster nodes can pull it:

```bash
# Authenticate with ICR — this stores a temporary token that docker/podman
# uses when pushing
ibmcloud cr login --region ${REGISTRY%%.*}

# Upload the image (use whichever tool you built with)
docker push ${IMAGE}
# or: podman push ${IMAGE}
```

If you're iterating locally and have a private registry or use OpenShift's internal registry:

```bash
# OpenShift internal registry
oc registry login
docker tag ${IMAGE} default-route-openshift-image-registry.apps.<cluster>/kube-system/vpc-file-pool-csi:${VERSION}
docker push default-route-openshift-image-registry.apps.<cluster>/kube-system/vpc-file-pool-csi:${VERSION}
```

---

## Step 2: Verify Credentials

**Why this step?** The driver needs to authenticate with the IBM Cloud VPC API to create and manage file shares. It uses an **API key** stored as a Kubernetes **Secret** (an encrypted key-value store that keeps sensitive data out of your YAML files and container images).

The driver uses `secret-common-lib` to authenticate — the same library used by the stock IBM CSI drivers. This means it reads credentials from secrets that already exist on your cluster.

### Managed Clusters (ROKS / IKS) — Nothing to Do

On managed clusters, IBM Cloud automatically creates and maintains the credentials for you. Just verify they exist:

```bash
# This secret contains the API key for VPC API access
kubectl get secret storage-secret-store -n kube-system

# This ConfigMap tells the driver which VPC and subnet to use
kubectl get configmap ibm-cloud-provider-data -n kube-system
```

If both exist, skip to Step 3. The driver will use these automatically.

### Self-Managed Clusters — Create Credentials

If you're running on a self-managed OpenShift or Kubernetes cluster, create the credentials manually:

```bash
# Create a service ID — a non-human identity for the driver to use
ibmcloud iam service-id-create vpc-file-pool-csi \
  --description "Service ID for VPC File Pool CSI Driver"

# Grant it permission to manage VPC Infrastructure (file shares, mount targets)
ibmcloud iam service-policy-create vpc-file-pool-csi \
  --roles Editor --service-name is

# Generate an API key for that service ID
ibmcloud iam service-api-key-create vpc-file-pool-csi-key vpc-file-pool-csi \
  --description "API key for VPC File Pool CSI Driver" \
  --output json

# Store the API key as a Kubernetes Secret
# (replace <paste-api-key-here> with the actual key from the previous command)
cat > /tmp/ibm-credentials.env << 'EOF'
IBMCLOUD_AUTHTYPE=iam
IBMCLOUD_APIKEY=<paste-api-key-here>
EOF

kubectl create secret generic ibm-cloud-credentials \
  --namespace kube-system \
  --from-file=ibm-credentials.env=/tmp/ibm-credentials.env
rm /tmp/ibm-credentials.env

# Tell the driver which VPC and subnet to use
kubectl create configmap ibm-cloud-provider-data \
  --namespace kube-system \
  --from-literal=vpcID=<your-vpc-id> \
  --from-literal=subnetID=<your-subnet-id>
```

For pod identity (Trusted Profiles) setup or more details, see `API-KEY-SETUP.md`.

---

## Step 3: Install the Driver

**Why Helm?** Installing a CSI driver involves creating many Kubernetes resources: Deployments, DaemonSets, ServiceAccounts, RBAC roles, a CSIDriver object, and more. **Helm** is a package manager for Kubernetes (like `apt` or `brew`, but for cluster applications). Instead of applying dozens of YAML files manually, Helm installs everything in one command and lets you customize settings with `--set` flags.

The `helm upgrade --install` command is idempotent — it installs the first time and upgrades on subsequent runs, so you can safely re-run it.

### Option A: Helm Chart (Recommended)

```bash
# Managed clusters (ROKS / IKS) — region, VPC, and subnet auto-discovered:
helm upgrade --install ibm-vpc-file-pool-csi \
  charts/ibm-vpc-file-pool-csi/ \
  --namespace kube-system \
  --set image.repository=${REGISTRY}/${NAMESPACE}/vpc-file-pool-csi \
  --set image.tag=${VERSION} \
  --set image.pullPolicy=Always \
  --set 'imagePullSecrets[0].name=all-icr-io'

# Self-managed clusters — provide VPC config explicitly:
# helm upgrade --install ibm-vpc-file-pool-csi \
#   charts/ibm-vpc-file-pool-csi/ \
#   --namespace kube-system \
#   --set image.repository=${REGISTRY}/${NAMESPACE}/vpc-file-pool-csi \
#   --set image.tag=${VERSION} \
#   --set image.pullPolicy=Always \
#   --set 'imagePullSecrets[0].name=all-icr-io' \
#   --set config.region=us-south \
#   --set config.vpcID=<YOUR_VPC_ID> \
#   --set config.subnetID=<YOUR_SUBNET_ID>
```

**What gets installed:**
- A **Deployment** (the controller) — the brain that manages file share pools, creates subdirectories for PVCs, and talks to the IBM VPC API
- A **DaemonSet** (the node agent) — runs on every worker node, responsible for mounting NFS shares and bind-mounting subdirectories into pods
- **RBAC roles** — permissions that allow the driver to read/write Kubernetes resources like PVCs and CRDs
- A **CSIDriver** object — registers the driver with Kubernetes so it knows to route storage requests to it
- A **StorageClass** — tells Kubernetes "when someone asks for storage of this type, use this CSI driver"

**Image pull secrets:** Your cluster nodes need permission to download the driver image from IBM Cloud Container Registry. Managed ROKS/IKS clusters have a pre-configured secret called `all-icr-io` that handles this. Without it, pods fail to start with an `unauthorized` / `ErrImagePull` error. Verify it exists:

```bash
kubectl get secret all-icr-io -n kube-system
```

**ROKS note:** On Red Hat OpenShift on IBM Cloud (ROKS), the kubelet root directory is `/var/data/kubelet` (not the standard `/var/lib/kubelet`). The kubelet is the agent on each node that manages pods — the CSI node agent needs to know where it stores volume data. Add `node.kubeletDir`:

```bash
helm upgrade --install ibm-vpc-file-pool-csi \
  charts/ibm-vpc-file-pool-csi/ \
  --namespace kube-system \
  --set image.repository=${REGISTRY}/${NAMESPACE}/vpc-file-pool-csi \
  --set image.tag=${VERSION} \
  --set image.pullPolicy=Always \
  --set 'imagePullSecrets[0].name=all-icr-io' \
  --set node.kubeletDir=/var/data/kubelet
```

**Helm values reference:**

| Value | Required | Default | Description |
|-------|----------|---------|-------------|
| `image.repository` | Yes | — | Container image registry path |
| `image.tag` | Yes | — | Image tag |
| `image.pullPolicy` | No | `IfNotPresent` | Image pull policy (`Always` recommended during iterative testing) |
| `imagePullSecrets` | No | `[]` | Image pull secrets for private registries. On managed ROKS/IKS, set to `[{name: all-icr-io}]` |
| `config.region` | No | Auto-discovered | IBM Cloud region (e.g., `us-south`). Auto-discovered from the secret provider RIAAS endpoint on managed ROKS/IKS clusters |
| `config.vpcID` | No | Auto-discovered | VPC ID where shares will be created. Auto-discovered from the `ibm-cloud-provider-data` ConfigMap on managed clusters |
| `config.subnetID` | No | Auto-discovered | Subnet ID for NFS mount targets. Auto-discovered from `ibm-cloud-provider-data` on managed clusters (uses first subnet) |
| `config.resourceGroupID` | No | Auto-discovered | Resource group for billing. Auto-discovered from the secret provider on managed clusters |
| `secret.name` | No | `ibm-vpc-file-pool-csi-secret` | Name of the API key secret |
| `secret.namespace` | No | `kube-system` | Namespace of the API key secret |
| `controller.replicas` | No | `1` | Controller replicas (leader-elected) |
| `controller.resources.requests.cpu` | No | `100m` | Controller CPU request |
| `controller.resources.requests.memory` | No | `128Mi` | Controller memory request |
| `node.kubeletDir` | No | `/var/lib/kubelet` | Kubelet root directory. Set to `/var/data/kubelet` on ROKS |
| `node.resources.requests.cpu` | No | `50m` | Node agent CPU request |
| `node.resources.requests.memory` | No | `64Mi` | Node agent memory request |
| `logLevel` | No | `4` | klog verbosity (2=normal, 4=detailed, 6=trace) |
| `webhook.enabled` | No | `true` | Enable validating admission webhooks |
| `webhook.port` | No | `9443` | Webhook server port |
| `webhook.certProvider` | No | `cert-manager` | TLS certificate provider: `cert-manager` or `manual` |
| `cloneWorker.interval` | No | `10s` | Background clone worker poll interval |
| `secretProvider.managed` | No | `true` | Enable managed secret provider sidecar (ROKS/IKS) |

### Option B: Raw Manifests

If you prefer not to use Helm, you can install the driver by applying Kubernetes YAML files directly. This gives you full visibility into what's being created, but you have to manage each file individually.

```bash
# 1. Install CRDs (Custom Resource Definitions)
#    CRDs extend Kubernetes with new resource types: FileSharePool, SubVolume, etc.
#    Without these, Kubernetes wouldn't understand what a FileSharePool is.
kubectl apply -f config/crd/

# 2. Install RBAC (Role-Based Access Control)
#    These give the driver permission to create PVs, read PVCs, update CRDs, etc.
kubectl apply -f config/rbac/

# 3. Edit the deployment manifests with your image and config
#    (or use kustomize — a kustomization.yaml is provided)
export IMAGE=${REGISTRY}/${NAMESPACE}/vpc-file-pool-csi:${VERSION}

# Using sed for quick substitution:
sed -i "s|image: .*vpc-file-pool-csi.*|image: ${IMAGE}|g" config/deploy/controller.yaml config/deploy/node.yaml

# 4. Deploy the controller and node agents
kubectl apply -f config/deploy/csidriver.yaml
kubectl apply -f config/deploy/controller.yaml
kubectl apply -f config/deploy/node.yaml

# 5. (Optional) Install custom StorageClasses
#    StorageClasses are auto-created when you create a FileSharePool.
#    Only apply this if you need custom SC parameters:
# kubectl apply -f config/deploy/storageclass.yaml
```

---

## Step 4: Create a File Share Pool

**Why this step?** The driver is now installed, but it doesn't have any storage to hand out yet. A **FileSharePool** tells the driver to create one or more VPC file shares and use them as a pool. When applications request storage (via PVCs), the driver allocates a subdirectory on an existing share in the pool — no need to create a new VPC share each time.

Think of it like pre-provisioning a bookshelf: you buy the shelf (the VPC file share) once, then add books (PVCs) to it as needed.

Create a pool for your target zone (see also `examples/basic/pool.yaml` for a ready-to-use template):

```yaml
# pool-general.yaml
apiVersion: storage.ibmcloud.io/v1alpha1
kind: FileSharePool
metadata:
  name: general-purpose
spec:
  zone: us-south-1                   # Must match your worker node zone
  profile: dp2                        # VPC file storage performance profile
  shareSizeGB: 2000                   # 2 TB per share
  maxShares: 10                       # Up to 10 shares (20 TB total capacity)
  initialShares: 1                    # Create 1 share immediately
  autoExpand: true                    # Auto-create new shares when pool fills up
  expandThresholdPercent: 80          # Trigger expansion at 80% allocated
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
# Watch pool status — press Ctrl+C when PHASE shows "Ready"
kubectl get filesharepools -w

# Detailed status (once ready)
kubectl get filesharepool general-purpose -o yaml
```

The pool is ready when `status.phase` is `Ready` and at least one share shows `state: stable`.

**Automatic StorageClass:** When the pool reaches `Ready`, the controller automatically creates a **StorageClass** named after the pool (e.g., `general-purpose`). A StorageClass is how Kubernetes knows which storage backend to use when you create a PVC — it's the link between "I want 10 Gi of storage" and "use this CSI driver with this pool". You do not need to create StorageClasses manually.

For tiered pools, one StorageClass per tier is created (e.g., `general-purpose-standard`, `general-purpose-premium`).

To opt out of automatic StorageClass creation, add the annotation `storage.ibmcloud.io/skip-storageclass: "true"` to the FileSharePool.

---

## Step 5: Verify the Installation

### Check all components are running

```bash
# CSI Driver registered — Kubernetes knows about our storage plugin
kubectl get csidriver vpc-file-pool.csi.ibm.io

# Controller pod (should be Running with all containers ready, e.g. 6/6)
kubectl get pods -n kube-system -l app.kubernetes.io/component=controller

# Node agent pods (one per worker node, all Running)
kubectl get pods -n kube-system -l app.kubernetes.io/component=node

# CRDs installed — the custom resource types the driver uses
kubectl get crd filesharepools.storage.ibmcloud.io
kubectl get crd subvolumes.storage.ibmcloud.io

# Pool is ready
kubectl get filesharepools

# StorageClass auto-created (named after the pool)
kubectl get storageclass general-purpose
```

### Smoke test with a PVC

This creates a **PVC** (Persistent Volume Claim) — a request for storage — and a pod that writes a file to it. If the file appears, everything is working end-to-end: Kubernetes asked the CSI driver for storage, the driver allocated a subdirectory on the pool, and the node agent mounted it into the pod.

```bash
# Create a test PVC (using the auto-created StorageClass named after the pool)
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: test-pool-pvc
  namespace: default
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: general-purpose
  resources:
    requests:
      storage: 1Gi
EOF

# Should bind within seconds (not minutes!) — press Ctrl+C once STATUS=Bound
kubectl get pvc test-pool-pvc -w

# Check a SubVolume was created — this is the internal record of the allocation
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

### Automated end-to-end tests

Instead of (or in addition to) the manual smoke test above, you can run the automated **e2e tests**. These are test scripts that live in `test/e2e/` and do the same thing — create pools, PVCs, and pods, then verify everything works — but automatically and with more thorough checks.

The tests run on your workstation using `go test`, but they talk to the cluster remotely via your kubeconfig. The driver must already be deployed on the cluster (Steps 1-4 above) — the tests don't install it, they just exercise it.

```bash
# Set required environment variables — these tell the tests which zone
# and subnet to use when creating pools
export E2E_HOME_ZONE=us-south-1           # Your worker node zone
export E2E_ACCESSOR_ZONE=us-south-2       # A second zone for cross-zone tests
export E2E_ACCESSOR_SUBNET_ID=<subnet-id> # Subnet ID in the accessor zone

# Run the tests (compiles and runs test/e2e/*.go from your laptop)
make test-e2e
```

The tests create resources with an `e2e-` prefix and clean them up automatically when done. Two scenarios are tested:

- **BasicPool** — creates a single-zone pool, provisions a PVC, mounts it in a pod, verifies it works
- **CrossZone** — creates a pool with accessor zones, verifies multi-zone NFS access

---

## Upgrading

**Why is upgrading safe?** The CSI driver runs as pods, but the actual NFS mounts are handled by the Linux kernel on each node. When the driver pods restart during an upgrade, existing mounts are unaffected — your applications keep reading and writing without interruption.

### Helm

```bash
helm upgrade ibm-vpc-file-pool-csi \
  charts/ibm-vpc-file-pool-csi/ \
  --namespace kube-system \
  --set image.tag=${NEW_VERSION}
```

The controller Deployment will rolling-update (replace one pod at a time). The node DaemonSet will update node-by-node.

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

**Important order:** Delete PVCs before the pool, and the pool before the driver. If you delete the driver first, there's nothing left to clean up the VPC file shares.

```bash
# 1. Delete all PVCs using the pool StorageClass first
kubectl delete pvc -l storageClassName=ibm-vpc-file-pool --all-namespaces

# 2. Wait for all SubVolumes to be cleaned up
kubectl get subvolumes  # Should be empty

# 3. Delete the FileSharePool(s)
#    WARNING: This deletes the underlying VPC file shares and all data on them
kubectl delete filesharepool --all

# 4. Uninstall the driver
helm uninstall ibm-vpc-file-pool-csi -n kube-system
# or, if you used raw manifests:
kubectl delete -f config/deploy/
kubectl delete -f config/rbac/

# 5. Remove CRDs (only if no resources remain)
kubectl delete -f config/crd/

# 6. Remove the API key secret (if you created one manually)
kubectl delete secret ibm-vpc-file-pool-csi-secret -n kube-system
```

**Warning:** Deleting a FileSharePool will delete all VPC file shares in the pool and all data on them. Ensure all PVCs are deleted and data is backed up before removing a pool.

---

## Installation Troubleshooting

If something goes wrong during installation, start with these quick checks:

### Pool stuck in Initializing

```bash
kubectl describe filesharepool <pool-name>
kubectl logs -n kube-system -l app.kubernetes.io/component=controller -c csi-controller --tail=100 | grep -i "error\|fail"
```

Common causes: wrong zone, invalid profile, VPC quota exceeded, TCP 2049 blocked, API key permissions.

### Driver pods CrashLoopBackOff

```bash
kubectl logs -n kube-system <pod-name> -c csi-controller --previous
```

Common causes: missing API key secret, wrong image tag, RBAC misconfiguration.

### Pods stuck in ErrImagePull

The cluster nodes can't download the driver image. This usually means the `imagePullSecrets` weren't set:

```bash
# Check if the all-icr-io secret exists
kubectl get secret all-icr-io -n kube-system

# Re-install with imagePullSecrets if missing from your Helm command
helm upgrade ibm-vpc-file-pool-csi charts/ibm-vpc-file-pool-csi/ \
  --namespace kube-system \
  --set 'imagePullSecrets[0].name=all-icr-io' \
  # ... other --set flags
```

### Pods stuck in ContainerCreating for a long time

Check the pod events for details:

```bash
kubectl describe pod <pod-name> -n kube-system
```

If you see transient `sd-bus` or `create container timeout` errors, these are typically temporary — the kubelet will retry automatically.

For the full troubleshooting guide (PVC issues, mount failures, pool problems, VPC API errors, and more), see [TROUBLESHOOTING.md](TROUBLESHOOTING.md).
