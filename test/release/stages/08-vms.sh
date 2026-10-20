#!/usr/bin/env bash
# 08-vms — A pool with golden images turned on makes a CentOS Stream 9 image,
# a VM template and a catalog entry, in a namespace with no extra
# permissions. A VM made from the template boots and its guest agent
# connects. The VM live-migrates to another worker and keeps running. A VM
# made from the catalog entry, with its boot disk on the pool, also boots.
# Deleting the pool removes everything the driver made for it.

RT_STAGE_TIMEOUT=3600

POOL=$(rt_name vm)
NS=$(rt_name vm)
VM=vm1
CATALOG_VM=vm2
IMAGES_NS=openshift-virtualization-os-images
POOL_LABEL="pool.storage.ibmcloud.io/pool"

create_pool() {
    oc apply -f - >/dev/null <<YAML
apiVersion: storage.ibmcloud.io/v1alpha1
kind: FileSharePool
metadata:
  name: $POOL
  labels:
    $RT_LABEL: "$RT_ID"
spec:
  zone: $(detect_zone)
  profile: dp2
  shareSizeGB: 500
  iops: 10000
  maxShares: 1
  initialShares: 1
  autoExpand: false
  allocationStrategy: spread
  defaultUID: 107
  defaultGID: 107
  defaultPermissions: "0777"
  goldenImages:
    enabled: true
    targetNamespaces: ["$NS"]
    imageFilter: ["centos-stream9"]
YAML
}

golden_image_ready() {
    pool_field "$POOL" .status.goldenImages | jq -e --arg ns "$NS" \
        '[.[]?.namespaces[]? | select(.namespace == $ns and .phase == "Ready")] | length > 0' >/dev/null
}

template_name() {
    oc get templates -n "$NS" -l "pool.storage.ibmcloud.io/golden-image=true" -o jsonpath='{.items[0].metadata.name}'
}

golden_claim() {
    pool_field "$POOL" .status.goldenImages | jq -r --arg ns "$NS" \
        '[.[]?.namespaces[]? | select(.namespace == $ns)][0].pvcName // empty'
}

# The catalog entry for this pool points at this pool's golden claim, and the
# cluster can use it.
catalog_entry_ready() {
    local claim; claim=$(golden_claim)
    oc get datasources.cdi.kubevirt.io -n "$IMAGES_NS" -l "$POOL_LABEL=$POOL" -o json | jq -e --arg ns "$NS" --arg claim "$claim" '
        [.items[] | select(.spec.source.pvc.namespace == $ns and .spec.source.pvc.name == $claim)
                  | select(any(.status.conditions[]?; .type == "Ready" and .status == "True"))] | length > 0' >/dev/null
}

# vmi_field JSONPATH [VM] — one field of a running VM (default: the template VM).
vmi_field() { oc get vmi -n "$NS" "${2:-$VM}" -o jsonpath="{$1}" 2>/dev/null; }
vmi_condition() { vmi_field ".status.conditions[?(@.type==\"$1\")].status" "${2:-$VM}"; }

vm_ready() { [ "$(vmi_field .status.phase "${1:-$VM}")" = "Running" ] && [ "$(vmi_condition Ready "${1:-$VM}")" = "True" ]; }
agent_connected() { [ "$(vmi_condition AgentConnected "${1:-$VM}")" = "True" ]; }

catalog_entry() {
    oc get datasources.cdi.kubevirt.io -n "$IMAGES_NS" -l "$POOL_LABEL=$POOL" -o jsonpath='{.items[0].metadata.name}'
}

# create_catalog_vm — a VM as the console's catalog makes it: an instance
# type, and a boot disk copied from the catalog entry onto the pool.
create_catalog_vm() {
    oc apply -n "$NS" -f - >/dev/null <<YAML
apiVersion: kubevirt.io/v1
kind: VirtualMachine
metadata:
  name: $CATALOG_VM
spec:
  runStrategy: Always
  instancetype:
    kind: VirtualMachineClusterInstancetype
    name: u1.medium
  preference:
    kind: VirtualMachineClusterPreference
    name: centos.stream9
  dataVolumeTemplates:
    - metadata:
        name: $CATALOG_VM-boot
      spec:
        sourceRef:
          kind: DataSource
          name: $(catalog_entry)
          namespace: $IMAGES_NS
        storage:
          storageClassName: $POOL
          resources:
            requests:
              storage: 20Gi
  template:
    spec:
      domain:
        devices: {}
      volumes:
        - name: rootdisk
          dataVolume:
            name: $CATALOG_VM-boot
        - name: cloudinit
          cloudInitNoCloud:
            userData: "#cloud-config\n"
YAML
}

catalog_disk_on_pool() {
    local sc; sc=$(oc get pvc -n "$NS" "$CATALOG_VM-boot" -o jsonpath='{.spec.storageClassName}')
    echo "catalog VM boot disk storage class: $sc"
    [ "$sc" = "$POOL" ]
}

guest_reports_centos() {
    local os; os=$(vmi_field .status.guestOSInfo.name)
    echo "guest OS: $os"
    echo "$os" | grep -qi centos
}

live_migratable() {
    local status; status=$(vmi_condition LiveMigratable)
    echo "LiveMigratable: $status ($(vmi_field '.status.conditions[?(@.type=="LiveMigratable")].message'))"
    [ "$status" = "True" ]
}

migrate() {
    oc apply -n "$NS" -f - >/dev/null <<YAML
apiVersion: kubevirt.io/v1
kind: VirtualMachineInstanceMigration
metadata:
  name: $1
spec:
  vmiName: $VM
YAML
}

migration_succeeded() {
    local phase; phase=$(oc get virtualmachineinstancemigrations -n "$NS" "$1" -o jsonpath='{.status.phase}')
    [ "$phase" = "Failed" ] && { echo "migration failed"; return 1; }
    [ "$phase" = "Succeeded" ]
}

# The same VM instance now runs on another worker: it moved without a restart.
moved_without_restart() {
    local uid="$1" node="$2" now_uid now_node
    now_uid=$(vmi_field .metadata.uid)
    now_node=$(vmi_field .status.nodeName)
    echo "before: $node $uid, after: $now_node $now_uid"
    [ "$now_uid" = "$uid" ] && [ -n "$now_node" ] && [ "$now_node" != "$node" ]
}

gone() { ! oc get "$@" >/dev/null 2>&1; }

# shared_leftovers — objects the driver made for this pool in the shared
# OS images namespace.
shared_leftovers() {
    oc get datasources.cdi.kubevirt.io,rolebindings -n "$IMAGES_NS" -o json |
        jq -r --arg label "$POOL_LABEL" --arg pool "$POOL" --arg ns "$NS" '.items[]
            | select(.metadata.labels[$label] == $pool
                     or .spec.source.pvc.namespace == $ns
                     or any(.subjects[]?; .name == "system:serviceaccounts:" + $ns))
            | .kind + "/" + .metadata.name'
}

nothing_left_for_pool() {
    local left; left=$(shared_leftovers)
    echo "left in $IMAGES_NS: ${left:-none}"
    [ -z "$left" ]
}

run() {
    oc get crd virtualmachines.kubevirt.io >/dev/null 2>&1 || skip "OpenShift Virtualization is not installed"
    oc get dataimportcrons.cdi.kubevirt.io -n "$IMAGES_NS" -o name 2>/dev/null | grep -q centos-stream9 ||
        skip "the cluster has no CentOS Stream 9 image source"
    [ "$(worker_nodes | wc -l)" -ge 2 ] || skip "live migration needs two workers"

    rt_namespace "$NS"
    create_pool
    check "the VM pool reaches Ready" wait_until 900 10 pool_is_ready "$POOL"
    check "the golden image is ready in a namespace with no extra permissions" \
        wait_until 1200 15 golden_image_ready
    check "a VM template for the golden image exists" wait_until 120 5 template_name
    check "the VM catalog entry points at this pool's golden image" wait_until 300 10 catalog_entry_ready

    local template; template=$(template_name)
    [ -n "$template" ] || return 0
    oc process "$template" -n "$NS" -p NAME="$VM" | oc apply -n "$NS" -f - >/dev/null
    check "a VM made from the template boots" wait_until 900 10 vm_ready
    check "the VM's guest agent connects" wait_until 600 10 agent_connected
    check "the guest agent reports CentOS" guest_reports_centos

    check "the VM can live-migrate" live_migratable
    local uid node
    uid=$(vmi_field .metadata.uid)
    node=$(vmi_field .status.nodeName)
    migrate mig1
    check "the live migration succeeds" wait_until 600 10 migration_succeeded mig1
    check "the VM moved to another worker without a restart" moved_without_restart "$uid" "$node"
    check "the VM keeps running after the move" vm_ready
    check "the guest agent connects after the move" wait_until 300 10 agent_connected

    if [ -n "$(catalog_entry)" ]; then
        create_catalog_vm
        check "a VM made from the catalog entry boots" wait_until 1200 10 vm_ready "$CATALOG_VM"
        check "its boot disk is on the pool" catalog_disk_on_pool
        check "its guest agent connects" wait_until 600 10 agent_connected "$CATALOG_VM"
    fi

    oc delete vm -n "$NS" "$VM" "$CATALOG_VM" --ignore-not-found --wait=true --timeout=300s >/dev/null
    oc delete pvc -n "$NS" "$VM-boot" "$VM-data" --ignore-not-found --wait=true --timeout=300s >/dev/null
    oc delete filesharepool "$POOL" --wait=false >/dev/null
    check "the pool can be deleted while golden images are on" wait_until 900 10 gone filesharepool "$POOL"
    check "the pool's golden image and template are removed" \
        wait_until 300 10 sh -c "[ -z \"\$(oc get datavolumes,pvc,templates -n $NS -l $POOL_LABEL=$POOL -o name)\" ]"
    check "nothing is left for the pool in $IMAGES_NS" wait_until 300 10 nothing_left_for_pool
}

cleanup() {
    oc delete namespace "$NS" --ignore-not-found --wait=true --timeout=600s
    oc delete filesharepool "$POOL" --ignore-not-found --wait=true --timeout=600s
    # Objects the driver made in the shared namespace: removed so a failed run
    # leaves the lab clean. The check above has already recorded them.
    shared_leftovers | while read -r obj; do
        oc delete -n "$IMAGES_NS" "$obj" --ignore-not-found
    done
}
