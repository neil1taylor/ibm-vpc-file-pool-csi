#!/bin/bash
# Create a VM from a golden image template.
#
# Prerequisites:
# - Golden image syncer has completed (check: kubectl get pvc -n pool-tutorial -l pool.storage.ibmcloud.io/golden-image=true)
# - Template exists (check: oc get templates -n pool-tutorial)
#
# Usage:
#   ./create-vm-from-template.sh [VM_NAME] [NAMESPACE] [TEMPLATE_NAME]

set -euo pipefail

VM_NAME="${1:-my-centos-vm}"
NAMESPACE="${2:-pool-tutorial}"
TEMPLATE="${3:-centos-stream9-nfs-pool}"

echo "Creating VM '$VM_NAME' in namespace '$NAMESPACE' from template '$TEMPLATE'..."

oc process "$TEMPLATE" -n "$NAMESPACE" \
  -p NAME="$VM_NAME" \
  -p CLOUD_USER_PASSWORD="$(openssl rand -base64 12)" \
  | oc apply -n "$NAMESPACE" -f -

echo ""
echo "VM created. Monitor startup:"
echo "  oc get vm $VM_NAME -n $NAMESPACE"
echo "  oc get vmi $VM_NAME -n $NAMESPACE"
echo ""
echo "Access console:"
echo "  virtctl console $VM_NAME -n $NAMESPACE"
