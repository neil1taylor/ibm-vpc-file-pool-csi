#!/usr/bin/env bash
# test-vm-clone.sh — E2E test for KubeVirt VM creation via CSI clone from golden image.
#
# Tests the full syncer-driven pipeline: pool with goldenImages.enabled=true
# -> syncer discovers images -> converter Jobs -> templates -> VM creation
# (CSI clone) -> clone Job completes -> VM boots -> virtctl connectivity.
#
# Usage:
#   make test-vm
#   ./test/e2e/test-vm-clone.sh --zone eu-de-1 --keep
#
# Requires: ROKS cluster with OpenShift Virtualization + CDI, oc, virtctl, kubectl, jq.

set -euo pipefail

###############################################################################
# Configuration defaults
###############################################################################

NAMESPACE="${NAMESPACE:-e2e-vm-test}"
POOL_NAME="${POOL_NAME:-e2e-vm-pool}"
ZONE="${ZONE:-eu-de-1}"
IMAGE_FILTER="${IMAGE_FILTER:-centos-stream9}"
SHARE_SIZE_GB="${SHARE_SIZE_GB:-300}"
SHARE_IOPS="${SHARE_IOPS:-}"
TOTAL_TIMEOUT="${TOTAL_TIMEOUT:-1200}"

KEEP=false
STAGE=""
STAGE_START=0
TEST_START=$(date +%s)

# Resource names (populated during test)
SC_NAME="${POOL_NAME}"
TEMPLATE_NAME=""
VM_NAME=""
BOOT_PVC_NAME=""

###############################################################################
# Parse flags
###############################################################################

while [[ $# -gt 0 ]]; do
    case "$1" in
        --keep)       KEEP=true; shift ;;
        --namespace)  NAMESPACE="$2"; shift 2 ;;
        --pool)       POOL_NAME="$2"; SC_NAME="$2"; shift 2 ;;
        --zone)       ZONE="$2"; shift 2 ;;
        --image)      IMAGE_FILTER="$2"; shift 2 ;;
        --timeout)    TOTAL_TIMEOUT="$2"; shift 2 ;;
        --help|-h)
            echo "Usage: $0 [--keep] [--namespace NS] [--pool POOL] [--zone ZONE] [--image FILTER] [--timeout SECS]"
            exit 0
            ;;
        *) echo "Unknown flag: $1"; exit 1 ;;
    esac
done

###############################################################################
# Helpers
###############################################################################

log()  { echo "[$(date +%H:%M:%S)] $*"; }
info() { log "INFO  $*"; }
warn() { log "WARN  $*"; }
err()  { log "ERROR $*"; }
pass() { log "PASS  $*"; }
fail() { log "FAIL  $*"; }

check_total_timeout() {
    local elapsed=$(( $(date +%s) - TEST_START ))
    if (( elapsed > TOTAL_TIMEOUT )); then
        err "Total timeout of ${TOTAL_TIMEOUT}s exceeded (elapsed: ${elapsed}s)"
        exit 1
    fi
}

# wait_for "description" timeout_secs "check_command" [poll_interval]
# check_command should return 0 on success, non-zero to retry.
wait_for() {
    local desc="$1"
    local timeout="$2"
    local cmd="$3"
    local interval="${4:-5}"
    local deadline=$(( $(date +%s) + timeout ))

    info "Waiting for $desc (timeout: ${timeout}s)..."
    while true; do
        check_total_timeout
        set +e
        (eval "$cmd") 2>/dev/null
        local rc=$?
        set -e
        if (( rc == 0 )); then
            info "$desc: done"
            return 0
        fi
        if (( $(date +%s) >= deadline )); then
            err "$desc: timed out after ${timeout}s"
            return 1
        fi
        sleep "$interval"
    done
}

enter_stage() {
    STAGE="$1"
    STAGE_START=$(date +%s)
    echo ""
    log "======== Stage: $STAGE ========"
}

stage_duration() {
    echo "$(( $(date +%s) - STAGE_START ))s"
}

###############################################################################
# Diagnostic collection (called on failure)
###############################################################################

collect_diagnostics() {
    echo ""
    log "======== DIAGNOSTICS ========"
    info "Collecting diagnostic data..."

    info "--- FileSharePool ---"
    oc get filesharepools.storage.ibmcloud.io "$POOL_NAME" -o yaml 2>/dev/null || echo "(not found)"

    info "--- FileSharePool goldenImages status ---"
    oc get filesharepools.storage.ibmcloud.io "$POOL_NAME" -o jsonpath='{.status.goldenImages}' 2>/dev/null | jq . 2>/dev/null || echo "(none)"

    info "--- SubVolumes ---"
    oc get subvolumes.storage.ibmcloud.io -o wide 2>/dev/null || echo "(none)"

    info "--- PVCs in $NAMESPACE ---"
    oc get pvc -n "$NAMESPACE" -o wide 2>/dev/null || echo "(none)"

    info "--- PVs ---"
    oc get pv -o wide 2>/dev/null | head -20 || echo "(none)"

    info "--- Clone Jobs in $NAMESPACE ---"
    oc get jobs -n "$NAMESPACE" -l storage.ibmcloud.io/clone-temp=true -o wide 2>/dev/null || echo "(none)"
    for job in $(oc get jobs -n "$NAMESPACE" -l storage.ibmcloud.io/clone-temp=true -o name 2>/dev/null); do
        info "Logs for $job:"
        oc logs -n "$NAMESPACE" "$job" --tail=50 2>/dev/null || echo "(no logs)"
    done

    info "--- Converter Jobs in $NAMESPACE ---"
    oc get jobs -n "$NAMESPACE" -l pool.storage.ibmcloud.io/golden-image=true -o wide 2>/dev/null || echo "(none)"
    for job in $(oc get jobs -n "$NAMESPACE" -l pool.storage.ibmcloud.io/golden-image=true -o name 2>/dev/null); do
        info "Logs for $job:"
        oc logs -n "$NAMESPACE" "$job" --tail=50 2>/dev/null || echo "(no logs)"
    done

    info "--- Templates in $NAMESPACE ---"
    oc get templates -n "$NAMESPACE" 2>/dev/null || echo "(none)"

    info "--- VM/VMI in $NAMESPACE ---"
    oc get vm,vmi -n "$NAMESPACE" -o wide 2>/dev/null || echo "(none)"
    if [[ -n "${VM_NAME:-}" ]]; then
        oc get vmi "$VM_NAME" -n "$NAMESPACE" -o yaml 2>/dev/null || echo "(VMI not found)"
        info "--- Events for VM ---"
        oc get events -n "$NAMESPACE" --field-selector "involvedObject.name=$VM_NAME" --sort-by=.lastTimestamp 2>/dev/null || true
    fi

    info "--- Controller logs (last 100 lines) ---"
    oc logs -n kube-system -l app.kubernetes.io/name=ibm-vpc-file-pool-csi,app.kubernetes.io/component=controller -c csi-controller --tail=100 2>/dev/null || echo "(no logs)"

    info "--- Node agent logs (last 50 lines per pod) ---"
    for pod in $(oc get pods -n kube-system -l app.kubernetes.io/name=ibm-vpc-file-pool-csi,app.kubernetes.io/component=node -o name 2>/dev/null); do
        info "  $pod:"
        oc logs -n kube-system "$pod" -c csi-node --tail=50 2>/dev/null || echo "(no logs)"
    done

    info "--- CSI provisioner logs (last 50 lines) ---"
    oc logs -n kube-system -l app.kubernetes.io/name=ibm-vpc-file-pool-csi,app.kubernetes.io/component=controller -c csi-provisioner --tail=50 2>/dev/null || echo "(no logs)"

    log "======== END DIAGNOSTICS ========"
}

###############################################################################
# Hardened cleanup — shared between EXIT trap and Stage 2
#
# Order:
#   1. Patch pool goldenImages.enabled=false (stop syncer re-creating PVCs)
#   2. Delete VMs, Jobs, force-delete pods, PVCs, templates
#   3. Grace period for CSI to cascade PVC → SubVolume deletion
#   4. Force-clean orphan SubVolumes (clear finalizers if needed)
#   5. Delete pool-owned PVs
#   6. Delete pool (force-remove finalizer if stuck)
#   7. Delete StorageClass
#   8. Delete namespace with stuck-recovery
###############################################################################

hardened_cleanup() {
    local label="${1:-cleanup}"

    info "[$label] Step 1: Disabling golden image syncer..."
    oc patch filesharepools.storage.ibmcloud.io "$POOL_NAME" \
        --type=merge -p '{"spec":{"goldenImages":{"enabled":false}}}' 2>/dev/null || true

    if oc get namespace "$NAMESPACE" &>/dev/null; then
        info "[$label] Step 2: Deleting workloads and PVCs in $NAMESPACE..."

        # Delete VMs/VMIs so virt-launcher pods release PVC mounts
        oc delete vm --all -n "$NAMESPACE" --ignore-not-found --timeout=30s 2>/dev/null || true
        for i in $(seq 1 15); do
            if ! oc get vmi -n "$NAMESPACE" -o name 2>/dev/null | grep -q .; then break; fi
            sleep 2
        done

        # Delete Jobs (converter + clone)
        oc delete jobs --all -n "$NAMESPACE" --ignore-not-found --timeout=30s 2>/dev/null || true

        # Force-delete any pods still stuck in Terminating
        oc delete pods --all -n "$NAMESPACE" --force --grace-period=0 2>/dev/null || true

        # Delete PVCs — with pods gone, pvc-protection finalizer clears
        oc delete pvc --all -n "$NAMESPACE" --ignore-not-found --timeout=60s 2>/dev/null || true

        # Delete templates and other leftovers
        oc delete templates --all -n "$NAMESPACE" --ignore-not-found 2>/dev/null || true
    fi

    info "[$label] Step 3: Grace period for CSI SubVolume cascade..."
    sleep 5

    info "[$label] Step 4: Force-cleaning orphan SubVolumes..."
    local svs
    svs=$(oc get subvolumes.storage.ibmcloud.io \
        -l storage.ibmcloud.io/pool="$POOL_NAME" -o name 2>/dev/null || true)
    if [[ -n "$svs" ]]; then
        local sv_count
        sv_count=$(echo "$svs" | wc -l | tr -d ' ')
        info "[$label]   Found $sv_count orphan SubVolume(s), clearing finalizers..."
        for sv in $svs; do
            oc patch "$sv" --type=merge -p '{"metadata":{"finalizers":null}}' 2>/dev/null || true
        done
        oc delete subvolumes.storage.ibmcloud.io \
            -l storage.ibmcloud.io/pool="$POOL_NAME" --ignore-not-found --timeout=30s 2>/dev/null || true
    fi

    info "[$label] Step 5: Deleting pool-owned PVs..."
    # Delete clone-temp PVs
    oc delete pv -l storage.ibmcloud.io/clone-temp=true --ignore-not-found 2>/dev/null || true
    # Delete PVs belonging to our pool's CSI driver + pool
    local pvs
    pvs=$(oc get pv -o json 2>/dev/null | jq -r '
        .items[]
        | select(.spec.csi.driver == "vpc-file-pool.csi.ibm.io")
        | select(.spec.csi.volumeAttributes.pool == "'"$POOL_NAME"'")
        | .metadata.name
    ' 2>/dev/null || true)
    if [[ -n "$pvs" ]]; then
        local pv_count
        pv_count=$(echo "$pvs" | wc -l | tr -d ' ')
        info "[$label]   Deleting $pv_count pool PV(s)..."
        for pv in $pvs; do
            oc delete pv "$pv" --ignore-not-found --timeout=30s 2>/dev/null || true
        done
    fi

    info "[$label] Step 6: Deleting pool..."
    if oc get filesharepools.storage.ibmcloud.io "$POOL_NAME" &>/dev/null 2>&1; then
        oc delete filesharepools.storage.ibmcloud.io "$POOL_NAME" \
            --ignore-not-found --timeout=30s 2>/dev/null || {
            info "[$label]   Pool stuck — force-removing finalizer..."
            oc patch filesharepools.storage.ibmcloud.io "$POOL_NAME" \
                --type=merge -p '{"metadata":{"finalizers":null}}' 2>/dev/null || true
            oc delete filesharepools.storage.ibmcloud.io "$POOL_NAME" \
                --ignore-not-found --timeout=30s 2>/dev/null || true
        }
    fi

    info "[$label] Step 7: Deleting StorageClass..."
    oc delete storageclass "$SC_NAME" --ignore-not-found 2>/dev/null || true

    info "[$label] Step 8: Deleting namespace..."
    if oc get namespace "$NAMESPACE" &>/dev/null; then
        oc delete namespace "$NAMESPACE" --timeout=60s 2>/dev/null || true

        # Stuck-recovery: if namespace is still Terminating after 60s, clear PVC finalizers
        local ns_wait=0
        while oc get namespace "$NAMESPACE" &>/dev/null && (( ns_wait < 120 )); do
            if (( ns_wait == 60 )); then
                warn "[$label]   Namespace stuck in Terminating — clearing PVC finalizers..."
                for pvc in $(oc get pvc -n "$NAMESPACE" -o name 2>/dev/null); do
                    oc patch "$pvc" -n "$NAMESPACE" --type=merge \
                        -p '{"metadata":{"finalizers":null}}' 2>/dev/null || true
                done
                # Also clear any remaining SubVolume finalizers in the namespace
                for sv in $(oc get subvolumes.storage.ibmcloud.io -n "$NAMESPACE" -o name 2>/dev/null); do
                    oc patch "$sv" -n "$NAMESPACE" --type=merge \
                        -p '{"metadata":{"finalizers":null}}' 2>/dev/null || true
                done
            fi
            sleep 2
            ns_wait=$(( ns_wait + 2 ))
        done
    fi

    info "[$label] Cleanup complete"
}

###############################################################################
# Cleanup (EXIT trap)
###############################################################################

cleanup() {
    local exit_code=$?
    if (( exit_code != 0 )); then
        fail "Test failed at stage: $STAGE"
        collect_diagnostics
    fi

    if [[ "$KEEP" == "true" ]]; then
        info "Keeping test resources (--keep specified)"
        info "  Namespace: $NAMESPACE"
        info "  Pool:      $POOL_NAME"
        info "  VM:        ${VM_NAME:-not created}"
        return
    fi

    hardened_cleanup "exit-trap"
}

trap cleanup EXIT

###############################################################################
# Stage 1: Check prerequisites
###############################################################################

enter_stage "check_prerequisites"

# Check required tools
for cmd in oc kubectl virtctl jq; do
    if ! command -v "$cmd" &>/dev/null; then
        err "Required command not found: $cmd"
        exit 1
    fi
done
info "Required tools present: oc, kubectl, virtctl, jq"

# Check cluster auth
if ! oc whoami &>/dev/null; then
    err "Not logged into OpenShift cluster. Run 'oc login' first."
    exit 1
fi
info "Cluster auth: $(oc whoami) @ $(oc whoami --show-server)"

# Check OpenShift Virtualization is installed
if ! oc get crd virtualmachines.kubevirt.io &>/dev/null; then
    err "OpenShift Virtualization not installed (VirtualMachine CRD not found)"
    exit 1
fi
info "OpenShift Virtualization: installed"

# Check CDI DataImportCrons exist (golden image source)
DIC_COUNT=$(oc get dataimportcrons.cdi.kubevirt.io -n openshift-virtualization-os-images -o name 2>/dev/null | wc -l | tr -d ' ')
if (( DIC_COUNT < 1 )); then
    err "No CDI DataImportCrons found in openshift-virtualization-os-images"
    exit 1
fi
info "CDI DataImportCrons: $DIC_COUNT found"

# Check our CSI driver CRDs
if ! oc get crd filesharepools.storage.ibmcloud.io &>/dev/null; then
    err "IBM VPC File Pool CSI driver CRDs not installed"
    exit 1
fi
info "CSI driver CRDs: present"

# Check CSI driver pods are running
controller_ready=$(oc get pods -n kube-system -l app.kubernetes.io/name=ibm-vpc-file-pool-csi,app.kubernetes.io/component=controller --field-selector=status.phase=Running -o name 2>/dev/null | wc -l | tr -d ' ')
if (( controller_ready < 1 )); then
    err "CSI controller pod not running in kube-system"
    exit 1
fi
info "CSI controller pod: running"

pass "Prerequisites check ($(stage_duration))"

###############################################################################
# Stage 2: Cleanup previous test resources
###############################################################################

enter_stage "cleanup_previous"

hardened_cleanup "stage2"

# Verify namespace is fully gone before proceeding
if oc get namespace "$NAMESPACE" &>/dev/null; then
    warn "Namespace $NAMESPACE still exists after cleanup — waiting..."
    for i in $(seq 1 30); do
        if ! oc get namespace "$NAMESPACE" &>/dev/null; then break; fi
        sleep 2
    done
    if oc get namespace "$NAMESPACE" &>/dev/null; then
        err "Namespace $NAMESPACE stuck after hardened cleanup — cannot proceed"
        exit 1
    fi
fi

info "Previous test resources cleaned up ($(stage_duration))"

###############################################################################
# Stage 3: Create namespace
###############################################################################

enter_stage "create_namespace"

# Retry namespace creation in case the old one is still terminating
for i in $(seq 1 30); do
    if oc create namespace "$NAMESPACE" 2>/dev/null; then
        break
    fi
    if (( i == 30 )); then
        err "Failed to create namespace $NAMESPACE after 30 retries"
        exit 1
    fi
    sleep 2
done

# Grant anyuid SCC to default service account — converter Jobs need root (runAsUser: 0)
# for dnf install + qemu-img. OpenShift blocks this without an explicit SCC grant.
oc adm policy add-scc-to-user anyuid -z default -n "$NAMESPACE"
info "Namespace $NAMESPACE created with anyuid SCC ($(stage_duration))"

###############################################################################
# Stage 4: Create StorageClass and FileSharePool with goldenImages
###############################################################################

enter_stage "create_pool"

# Create StorageClass first (syncer needs it to provision golden PVCs)
cat <<EOF | oc apply -f -
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: ${SC_NAME}
provisioner: vpc-file-pool.csi.ibm.io
reclaimPolicy: Delete
volumeBindingMode: Immediate
parameters:
  pool: ${POOL_NAME}
mountOptions:
  - nfsvers=4.1
  - soft
  - timeo=600
  - retrans=3
  - sec=sys
EOF
info "StorageClass $SC_NAME created"

# Create FileSharePool with golden image syncer enabled
cat <<EOF | oc apply -f -
apiVersion: storage.ibmcloud.io/v1alpha1
kind: FileSharePool
metadata:
  name: ${POOL_NAME}
spec:
  zone: ${ZONE}
  profile: dp2
  shareSizeGB: ${SHARE_SIZE_GB}
$( [ -n "${SHARE_IOPS}" ] && echo "  iops: ${SHARE_IOPS}" )
  maxShares: 2
  initialShares: 1
  autoExpand: true
  expandThresholdPercent: 80
  allocationStrategy: spread
  defaultUID: 107
  defaultGID: 107
  defaultPermissions: "0777"
  goldenImages:
    enabled: true
    targetNamespaces:
      - ${NAMESPACE}
    pvcSizeGB: 15
EOF

info "FileSharePool $POOL_NAME created with goldenImages enabled ($(stage_duration))"

###############################################################################
# Stage 5: Wait for pool ready
###############################################################################

enter_stage "wait_pool_ready"

wait_for "pool $POOL_NAME to be Ready" 600 \
    "oc get filesharepools.storage.ibmcloud.io $POOL_NAME -o jsonpath='{.status.phase}' | grep -q Ready"

pass "Pool ready ($(stage_duration))"

###############################################################################
# Stage 6: Wait for golden images ready (syncer-driven)
###############################################################################

enter_stage "wait_golden_images_ready"

info "The golden image syncer will:"
info "  1. Discover DataImportCrons matching '$IMAGE_FILTER'"
info "  2. Create golden PVCs on StorageClass '$SC_NAME'"
info "  3. Run converter Jobs (download + qcow2-to-raw)"
info "  4. Mark PVCs ready and create OpenShift Templates"
info "Syncer interval: 5m (first cycle may take up to 5m to start)"

# Poll until the pool status shows at least one golden image in Ready phase
# in our target namespace, AND the corresponding template exists.
wait_for "golden image to be Ready in $NAMESPACE" 900 '
    # Check pool status for Ready golden images
    STATUS_JSON=$(oc get filesharepools.storage.ibmcloud.io '$POOL_NAME' \
        -o jsonpath="{.status.goldenImages}" 2>/dev/null)
    if [[ -z "$STATUS_JSON" || "$STATUS_JSON" == "null" ]]; then
        echo "  Syncer has not run yet (no goldenImages status)"
        exit 1
    fi

    # Look for any namespace entry with phase=Ready for our namespace
    READY=$(echo "$STATUS_JSON" | jq -r \
        "[.[] | .namespaces[]? | select(.namespace == \"'$NAMESPACE'\" and .phase == \"Ready\")] | length" 2>/dev/null)
    if [[ "$READY" == "0" || -z "$READY" ]]; then
        # Show current phases for debugging
        PHASES=$(echo "$STATUS_JSON" | jq -r \
            "[.[] | .namespaces[]? | select(.namespace == \"'$NAMESPACE'\")] | .[] | \"\(.pvcName): \(.phase) - \(.message // \"\")\""  2>/dev/null || echo "  (parsing)")
        echo "  Golden image phases: $PHASES"
        exit 1
    fi

    # Also verify a template actually exists in the namespace
    TMPL_COUNT=$(oc get templates -n '$NAMESPACE' \
        -l pool.storage.ibmcloud.io/golden-image=true -o name 2>/dev/null | wc -l | tr -d " ")
    if (( TMPL_COUNT < 1 )); then
        echo "  Pool status says Ready but no template found yet"
        exit 1
    fi

    echo "  $READY golden image(s) Ready, $TMPL_COUNT template(s) available"
    exit 0
' 15

pass "Golden images ready ($(stage_duration))"

###############################################################################
# Stage 7: Pick ready image and derive names
###############################################################################

enter_stage "pick_ready_image"

# Get the first ready template from the namespace
TEMPLATE_NAME=$(oc get templates -n "$NAMESPACE" \
    -l pool.storage.ibmcloud.io/golden-image=true \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)

if [[ -z "$TEMPLATE_NAME" ]]; then
    err "No golden image template found in $NAMESPACE"
    exit 1
fi

# Get the golden PVC name from the template's boot PVC dataSource
GOLDEN_PVC_NAME=$(oc get template "$TEMPLATE_NAME" -n "$NAMESPACE" \
    -o json 2>/dev/null | jq -r '
        .objects[]
        | select(.kind == "PersistentVolumeClaim" and .spec.dataSource != null)
        | .spec.dataSource.name
    ' | head -1)

# Derive VM name from template
GOLDEN_IMAGE_NAME=$(oc get template "$TEMPLATE_NAME" -n "$NAMESPACE" \
    -o jsonpath='{.metadata.labels.pool\.storage\.ibmcloud\.io/image}' 2>/dev/null)
CLEAN_NAME=$(echo "${GOLDEN_IMAGE_NAME:-unknown}" | tr '.' '-')
VM_NAME="${CLEAN_NAME}-e2e"
BOOT_PVC_NAME="${VM_NAME}-boot"

info "Template:      $TEMPLATE_NAME"
info "Golden PVC:    $GOLDEN_PVC_NAME"
info "Golden image:  $GOLDEN_IMAGE_NAME"
info "VM name:       $VM_NAME"
info "Boot PVC:      $BOOT_PVC_NAME"

pass "Ready image selected ($(stage_duration))"

###############################################################################
# Stage 8: Create VM from template
###############################################################################

enter_stage "create_vm_from_template"

oc process "$TEMPLATE_NAME" -n "$NAMESPACE" \
    -p NAME="$VM_NAME" \
    | oc apply -n "$NAMESPACE" --server-side --force-conflicts -f -

info "VM $VM_NAME and PVCs created via template ($(stage_duration))"

###############################################################################
# Stage 9: Wait for clone to complete
###############################################################################

enter_stage "wait_clone_complete"

info "Waiting for boot PVC clone (CSI clone Job)..."

wait_for "clone SubVolume to complete" 600 '
    SV_NAME=$(oc get subvolumes.storage.ibmcloud.io -o json 2>/dev/null | \
        jq -r ".items[] | select(.spec.pvcName == \"'$BOOT_PVC_NAME'\") | .metadata.name" | head -1)
    if [[ -z "$SV_NAME" ]]; then
        echo "  SubVolume for $BOOT_PVC_NAME not found yet"
        exit 1
    fi
    STATUS=$(oc get subvolumes.storage.ibmcloud.io "$SV_NAME" -o jsonpath="{.status.cloneStatus}" 2>/dev/null)
    if [[ "$STATUS" == "Complete" ]]; then
        exit 0
    elif [[ "$STATUS" == "Failed" ]]; then
        echo "  Clone FAILED!"
        oc get subvolumes.storage.ibmcloud.io "$SV_NAME" -o yaml 2>/dev/null
        exit 1
    fi
    echo "  Clone status: ${STATUS:-Pending}"
    exit 1
' 10

pass "Clone complete ($(stage_duration))"

###############################################################################
# Stage 10: Wait for VM running
###############################################################################

enter_stage "wait_vm_running"

wait_for "VMI $VM_NAME to be Running+Ready" 180 '
    PHASE=$(oc get vmi '$VM_NAME' -n '$NAMESPACE' -o jsonpath="{.status.phase}" 2>/dev/null)
    if [[ "$PHASE" != "Running" ]]; then
        echo "  VMI phase: ${PHASE:-not created}"
        exit 1
    fi
    READY=$(oc get vmi '$VM_NAME' -n '$NAMESPACE' -o jsonpath="{.status.conditions[?(@.type==\"Ready\")].status}" 2>/dev/null)
    if [[ "$READY" == "True" ]]; then
        exit 0
    fi
    echo "  VMI Running but Ready=$READY"
    exit 1
' 5

pass "VM running ($(stage_duration))"

###############################################################################
# Stage 11: Verify VM connectivity
###############################################################################

enter_stage "verify_vm_connectivity"

# Tier 1 (Required): VMI Phase=Running + Ready=True — already verified in stage 10
pass "Tier 1: VMI Running + Ready=True"

# Tier 2 (Best-effort): virtctl console check
info "Tier 2: Testing virtctl console (best-effort, 30s timeout)..."
CONSOLE_RESULT="unknown"
# macOS doesn't have coreutils 'timeout', use perl fallback
if command -v timeout &>/dev/null; then
    CONSOLE_OUTPUT=$(timeout 30 bash -c '(sleep 5; echo) | virtctl console '"$VM_NAME"' -n '"$NAMESPACE"' 2>&1' || true)
else
    CONSOLE_OUTPUT=$(perl -e 'alarm 30; exec @ARGV' bash -c '(sleep 5; echo) | virtctl console '"$VM_NAME"' -n '"$NAMESPACE"' 2>&1' || true)
fi

if echo "$CONSOLE_OUTPUT" | grep -qiE "login:|centos|cloud-user|connected|#|\\\$"; then
    pass "Tier 2: Console shows login prompt or shell"
    CONSOLE_RESULT="pass"
else
    warn "Tier 2: Console output did not match expected patterns (non-blocking)"
    CONSOLE_RESULT="inconclusive"
fi
info "Console output (first 5 lines):"
echo "$CONSOLE_OUTPUT" | head -5 || true

# Tier 3 (Best-effort): Guest OS info via guest agent
info "Tier 3: Checking guest agent OS info (best-effort)..."
GUEST_OS=$(oc get vmi "$VM_NAME" -n "$NAMESPACE" -o jsonpath='{.status.guestOSInfo.name}' 2>/dev/null || echo "")
if [[ -n "$GUEST_OS" ]]; then
    pass "Tier 3: Guest agent reports OS: $GUEST_OS"
else
    warn "Tier 3: Guest agent not available (non-blocking, may need more time)"
fi

pass "VM connectivity verified ($(stage_duration))"

###############################################################################
# Final summary
###############################################################################

echo ""
TOTAL_ELAPSED=$(( $(date +%s) - TEST_START ))
log "========================================================"
log "  E2E VM CLONE TEST: PASSED"
log "========================================================"
log "  Total time:     ${TOTAL_ELAPSED}s ($(( TOTAL_ELAPSED / 60 ))m $(( TOTAL_ELAPSED % 60 ))s)"
log "  Namespace:      $NAMESPACE"
log "  Pool:           $POOL_NAME"
log "  Golden image:   ${GOLDEN_IMAGE_NAME:-unknown}"
log "  Template:       $TEMPLATE_NAME"
log "  VM:             $VM_NAME"
log "  Console test:   $CONSOLE_RESULT"
log "  Guest agent:    ${GUEST_OS:-not available}"
log "========================================================"

if [[ "$KEEP" == "true" ]]; then
    echo ""
    info "Resources kept. Connect to VM with:"
    info "  virtctl console $VM_NAME -n $NAMESPACE"
fi
