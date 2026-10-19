---
name: deploy
description: Build the CSI driver container image on the build server, push it to the registry, update CRDs, and upgrade the Helm release on the cluster. Use after code changes that need to be tested on a live cluster.
argument-hint: "[image-tag] (default: git version)"
allowed-tools: Bash(make *), Bash(ssh *), Bash(git *), Bash(ibmcloud *), Bash(jq *), Bash(oc *), Bash(kubectl *), Bash(helm *)
---

Deploy the CSI driver to the cluster. Uses the Makefile targets and Helm chart.

Image tag: $ARGUMENTS (default to the git-derived version if no argument provided).

## Where to build

Build on the build server, not on this Mac. The server is an x86 VPC instance on the
cluster's own subnet, with no floating IP. Reach it over the client VPN.

| | |
|---|---|
| Instance | `pool-csi-build` (resource group `rovslab-prod-eude-rg`, eu-de) |
| Address | `10.250.0.46`, user `ubuntu` |
| SSH | `ssh -i ~/.ssh/pool-csi-build -o IdentitiesOnly=yes ubuntu@10.250.0.46` |
| Tools | podman, Go 1.25, make, helm, oc, golangci-lint (from cloud-init) |

If the instance is stopped, start it: `ibmcloud is instance-start pool-csi-build`.
If SSH times out, check the VPN route first (see the `vpn-route-overlap-rovslab` memory),
not the security group.

## Steps

### 1. Confirm the cluster

```bash
oc config current-context | cut -d/ -f1   # must be the cluster you mean to change
oc get secret all-icr-io -n kube-system   # pull secret; copy it from `default` if missing
```

### 2. Build the image on the build server

Send the working tree (committed or not) and build. `make docker-build` stamps the version.

```bash
TAG=${TAG:-$(git describe --tags --always --dirty)}
SSH="ssh -i ~/.ssh/pool-csi-build -o IdentitiesOnly=yes ubuntu@10.250.0.46"
git ls-files -co --exclude-standard -z | xargs -0 tar -czf - | $SSH 'rm -rf ~/src && mkdir ~/src && tar -xzf - -C ~/src'
$SSH "cd ~/src && make docker-build CONTAINER_ENGINE=podman VERSION=$TAG"
```

### 3. Push the image

Log in with a short-lived IAM token, so no credential is stored on the server:

```bash
ibmcloud iam oauth-tokens --output json | jq -r '.iam_token' | sed 's/^Bearer //' \
  | $SSH 'podman login -u iambearer --password-stdin de.icr.io'
$SSH "podman push de.icr.io/ibm-vpc-file-pool-csi/driver:$TAG"
```

### 4. Apply CRD updates

```bash
make install-crds
```

### 5. Upgrade the Helm release

```bash
helm upgrade --install ibm-vpc-file-pool-csi charts/ibm-vpc-file-pool-csi/ \
  --namespace kube-system \
  --reuse-values \
  --set image.tag=$TAG \
  --set image.pullPolicy=Always
```

`--reuse-values` keeps the earlier settings. On a first install, also set:

- `imagePullSecrets[0].name=all-icr-io`
- `webhook.certProvider=openshift` on ROKS and ROVS (no cert-manager there)

If the user has a custom values file, check for `values.yaml` or `values-dev.yaml` in the project root and use it with `--values`.

### 6. Wait for rollout

```bash
kubectl rollout status deployment/ibm-vpc-file-pool-csi-controller -n kube-system --timeout=180s
kubectl rollout status daemonset/ibm-vpc-file-pool-csi-node -n kube-system --timeout=180s
```

### 7. Verify

```bash
kubectl get pods -n kube-system -l app.kubernetes.io/name=ibm-vpc-file-pool-csi -o wide
kubectl get deployment -n kube-system ibm-vpc-file-pool-csi-controller \
  -o jsonpath='{.spec.template.spec.containers[0].image}'
kubectl logs -n kube-system deploy/ibm-vpc-file-pool-csi-controller -c csi-controller | grep -m1 'Starting IBM VPC File Pool CSI Driver'
```

The start-up line must show `version=<tag>`, not `dev`. Report success or failure with the deployed image tag.
