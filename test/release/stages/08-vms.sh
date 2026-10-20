#!/usr/bin/env bash
# 08-vms — A VM made from the standard catalog image, with its boot disk on a
# pool, boots in a namespace with no extra permissions, and its guest agent
# connects. The VM live-migrates to another worker and keeps running. CDI
# copies 25 GB of data from Ceph onto the pool, and the copy matches.

RT_STAGE_TIMEOUT=3600

POOL=$(rt_name vm)
NS=$(rt_name vm)
COPY_NS=$(rt_name copy)
VM=vm1
IMAGES_NS=openshift-virtualization-os-images
CATALOG_IMAGE="${RT_CATALOG_IMAGE:-centos-stream9}"
CATALOG_PREFERENCE="${RT_CATALOG_PREFERENCE:-centos.stream9}"
CEPH_SC="${RT_CEPH_SC:-ocs-storagecluster-ceph-rbd-virtualization}"
COPY_BYTES=26843545600

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
YAML
}

# create_catalog_vm — a VM as the console's catalog makes it: an instance
# type, and a boot disk copied from the catalog image onto the pool.
create_catalog_vm() {
    oc apply -n "$NS" -f - >/dev/null <<YAML
apiVersion: kubevirt.io/v1
kind: VirtualMachine
metadata:
  name: $VM
spec:
  runStrategy: Always
  instancetype:
    kind: VirtualMachineClusterInstancetype
    name: u1.medium
  preference:
    kind: VirtualMachineClusterPreference
    name: $CATALOG_PREFERENCE
  dataVolumeTemplates:
    - metadata:
        name: $VM-boot
      spec:
        sourceRef:
          kind: DataSource
          name: $CATALOG_IMAGE
          namespace: $IMAGES_NS
        storage:
          storageClassName: $POOL
          resources:
            requests:
              storage: 30Gi
  template:
    spec:
      domain:
        devices: {}
      volumes:
        - name: rootdisk
          dataVolume:
            name: $VM-boot
        - name: cloudinit
          cloudInitNoCloud:
            userData: "#cloud-config\\n"
YAML
}

vmi_field() { oc get vmi -n "$NS" "$VM" -o jsonpath="{$1}" 2>/dev/null; }
vmi_condition() { vmi_field ".status.conditions[?(@.type==\"$1\")].status"; }

vm_ready() { [ "$(vmi_field .status.phase)" = "Running" ] && [ "$(vmi_condition Ready)" = "True" ]; }
agent_connected() { [ "$(vmi_condition AgentConnected)" = "True" ]; }

boot_disk_on_pool_without_restarts() {
    local sc restarts
    sc=$(oc get pvc -n "$NS" "$VM-boot" -o jsonpath='{.spec.storageClassName}')
    restarts=$(oc get dv -n "$NS" "$VM-boot" -o jsonpath='{.status.restartCount}')
    echo "boot disk storage class: $sc, copy restarts: ${restarts:-0}"
    [ "$sc" = "$POOL" ] && [ "${restarts:-0}" -eq 0 ]
}

guest_reports_os() {
    local os; os=$(vmi_field .status.guestOSInfo.prettyName)
    echo "guest OS: $os"
    [ -n "$os" ]
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

# root_pod NS NAME PVC COMMAND — a root pod with PVC at /data.
root_pod() {
    oc apply -n "$1" -f - >/dev/null <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: $2
spec:
  restartPolicy: Never
  securityContext:
    runAsUser: 0
  containers:
    - name: main
      image: $RT_IMAGE
      command: ["/bin/sh", "-c", "$4"]
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: $3
YAML
}

pod_succeeded() { [ "$(oc get pod -n "$1" "$2" -o jsonpath='{.status.phase}')" = "Succeeded" ]; }

dv_done() {
    local phase; phase=$(oc get dv -n "$COPY_NS" "$1" -o jsonpath='{.status.phase}')
    [ "$phase" = "Failed" ] && return 1
    [ "$phase" = "Succeeded" ]
}

copy_matches_source() {
    local a b
    a=$(oc logs -n "$COPY_NS" sum-source | cut -d' ' -f1)
    b=$(oc logs -n "$COPY_NS" sum-copy | cut -d' ' -f1)
    echo "source $a, copy $b, copy restarts: $(oc get dv -n "$COPY_NS" big-copy -o jsonpath='{.status.restartCount}')"
    [ -n "$a" ] && [ "$a" = "$b" ]
}

# large_copy — CDI copies 25 GB of random data from Ceph onto the pool. With
# CDI's default memory limit this copy is killed (spec 2026-10-20-cdi-copy-limit).
# The pods that write and read the data run as root, so they use their own
# namespace, and the VM namespace keeps no extra permissions.
large_copy() {
    rt_namespace "$COPY_NS"
    oc adm policy add-scc-to-user anyuid -z default -n "$COPY_NS" >/dev/null
    oc apply -n "$COPY_NS" -f - >/dev/null <<YAML
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: big-source
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: $CEPH_SC
  resources:
    requests:
      storage: 30Gi
YAML
    root_pod "$COPY_NS" fill big-source "head -c $COPY_BYTES /dev/urandom > /data/disk.img && sync"
    if ! wait_until 900 10 pod_succeeded "$COPY_NS" fill; then
        not_checked "a 25 GB CDI copy onto the pool" "could not fill the Ceph source"
        return 0
    fi
    oc delete pod -n "$COPY_NS" fill --wait=true >/dev/null
    oc apply -n "$COPY_NS" -f - >/dev/null <<YAML
apiVersion: cdi.kubevirt.io/v1beta1
kind: DataVolume
metadata:
  name: big-copy
spec:
  source:
    pvc:
      name: big-source
      namespace: $COPY_NS
  storage:
    storageClassName: $POOL
    resources:
      requests:
        storage: 30Gi
YAML
    check "CDI copies 25 GB from Ceph onto the pool" wait_until 1500 10 dv_done big-copy
    root_pod "$COPY_NS" sum-source big-source "sha256sum /data/disk.img"
    root_pod "$COPY_NS" sum-copy big-copy "sha256sum /data/disk.img"
    wait_until 900 10 pod_succeeded "$COPY_NS" sum-source
    wait_until 900 10 pod_succeeded "$COPY_NS" sum-copy
    check "the 25 GB copy matches its source" copy_matches_source
}

run() {
    oc get crd virtualmachines.kubevirt.io >/dev/null 2>&1 || skip "OpenShift Virtualization is not installed"
    oc get datasources.cdi.kubevirt.io -n "$IMAGES_NS" "$CATALOG_IMAGE" >/dev/null 2>&1 ||
        skip "the cluster has no $CATALOG_IMAGE catalog image"
    [ "$(worker_nodes | wc -l)" -ge 2 ] || skip "live migration needs two workers"

    rt_namespace "$NS"
    create_pool
    check "the VM pool reaches Ready" wait_until 900 10 pool_is_ready "$POOL"

    create_catalog_vm
    check "a VM made from the catalog image boots" wait_until 1200 10 vm_ready
    check "its boot disk is on the pool, copied without restarts" boot_disk_on_pool_without_restarts
    check "the VM's guest agent connects" wait_until 600 10 agent_connected
    check "the guest agent reports its OS" guest_reports_os

    check "the VM can live-migrate" live_migratable
    local uid node
    uid=$(vmi_field .metadata.uid)
    node=$(vmi_field .status.nodeName)
    migrate mig1
    check "the live migration succeeds" wait_until 600 10 migration_succeeded mig1
    check "the VM moved to another worker without a restart" moved_without_restart "$uid" "$node"
    check "the VM keeps running after the move" vm_ready
    check "the guest agent connects after the move" wait_until 300 10 agent_connected
    oc delete vm -n "$NS" "$VM" --wait=true --timeout=300s >/dev/null

    if oc get sc "$CEPH_SC" >/dev/null 2>&1; then
        large_copy
    else
        not_checked "a 25 GB CDI copy onto the pool" "no StorageClass $CEPH_SC"
    fi
}

cleanup() {
    oc delete namespace "$NS" "$COPY_NS" --ignore-not-found --wait=true --timeout=600s
    oc delete filesharepool "$POOL" --ignore-not-found --wait=true --timeout=600s
}
