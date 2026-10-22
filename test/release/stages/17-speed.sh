#!/usr/bin/env bash
# 17-speed — fio runs five tests of 60 s each in three places: a pod on a pool
# volume, a pod on IBM's own ibmc-vpc-file-3000-iops class (the baseline), and
# inside a VM on a disk on the pool. Results go to speed.csv in the run folder
# and to the history file. A result more than 20% worse than the last run is a
# warning (a NOTE), not a failure, because cloud storage speed varies.

RT_STAGE_TIMEOUT=3600

POOL=$(rt_name speed)
NS=$(rt_name speed)
VM=fio-vm
BASELINE_SC="${RT_BASELINE_SC:-ibmc-vpc-file-3000-iops}"
HISTORY="${RT_HISTORY:-$RT_OUT/../history.csv}"
CATALOG_IMAGE="${RT_CATALOG_IMAGE:-centos-stream9}"
CATALOG_PREFERENCE="${RT_CATALOG_PREFERENCE:-centos.stream9}"
IMAGES_NS=openshift-virtualization-os-images

# name:readwrite:blocksize:iodepth[:rwmixread]
TESTS="seq-read:read:1M:8 seq-write:write:1M:8 rand-read:randread:4k:32 rand-write:randwrite:4k:32 rand-mix:randrw:4k:32:70"

# fio_script TARGET — shell that runs every test against TARGET (a file or a
# block device) and prints one "FIO <name> <json>" line per test.
fio_script() {
    local t name rw bs depth mix
    echo "set -e"
    for t in $TESTS; do
        IFS=: read -r name rw bs depth mix <<<"$t"
        printf '%s\n' "echo \"FIO $name \$(fio --name=$name --filename=$1 --size=4G --direct=1 --ioengine=libaio --rw=$rw --bs=$bs --iodepth=$depth ${mix:+--rwmixread=$mix} --runtime=60 --time_based --group_reporting --output-format=json | tr -d '\\n')\""
    done
}

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
  shareSizeGB: 200
  iops: 3000
  maxShares: 1
  initialShares: 1
  autoExpand: false
  allocationStrategy: spread
  defaultUID: 107
  defaultGID: 107
  defaultPermissions: "0777"
YAML
}

claim() {
    oc apply -n "$NS" -f - >/dev/null <<YAML
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: $1
spec:
  accessModes: [ReadWriteMany]
  storageClassName: $2
  resources:
    requests:
      storage: $3
YAML
}

# fio_pod NAME CLAIM — a pod that installs fio and runs every test on CLAIM.
fio_pod() {
    local script
    script="dnf install -y -q fio >/dev/null && $(fio_script /data/fio.dat | tr '\n' ';')"
    oc apply -n "$NS" -f - >/dev/null <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: $1
spec:
  restartPolicy: Never
  securityContext:
    runAsUser: 0
  containers:
    - name: fio
      image: $RT_IMAGE
      command: ["/bin/sh", "-c", $(jq -Rn --arg s "$script" '$s')]
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: $2
YAML
}

# fio_vm — a VM from the catalog image, with a second disk on the pool. At
# first boot it installs fio, runs every test on that disk, and prints the
# results on its serial console, which goes to the pod log.
fio_vm() {
    local script
    script="$(fio_script /dev/vdb | sed 's/^/                    /')"
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
        devices:
          logSerialConsole: true
      volumes:
        - name: rootdisk
          dataVolume:
            name: $VM-boot
        - name: fio-disk
          persistentVolumeClaim:
            claimName: vm-disk
        - name: cloudinit
          cloudInitNoCloud:
            userData: |
              #cloud-config
              write_files:
                - path: /root/fio.sh
                  permissions: "0755"
                  content: |
$script
              runcmd:
                - dnf install -y -q fio
                - sh -c '/root/fio.sh > /dev/ttyS0 2>&1; echo FIO-DONE > /dev/ttyS0'
YAML
}

pod_succeeded() { [ "$(oc get pod -n "$NS" "$1" -o jsonpath='{.status.phase}')" = "Succeeded" ]; }

vm_log() { oc logs -n "$NS" "$(oc get pods -n "$NS" -l "vm.kubevirt.io/name=$VM" -o name | head -1)" -c guest-console-log 2>/dev/null; }
vm_done() { vm_log | grep -q "FIO-DONE"; }

# record PLACE — turn the "FIO <name> <json>" lines on stdin into CSV rows:
# run, place, test, read IOPS, write IOPS, read MB/s, write MB/s.
record() {
    local place="$1" line name json
    while read -r line; do
        case "$line" in *"FIO "*"{"*) ;; *) continue ;; esac
        line=${line#*FIO }
        name=${line%% *}
        json=${line#* }
        echo "$json" | jq -r --arg run "$RT_ID" --arg place "$place" --arg test "$name" \
            '.jobs[0] | [$run, $place, $test, (.read.iops|floor), (.write.iops|floor), ((.read.bw/1024)|floor), ((.write.bw/1024)|floor)] | @csv' 2>/dev/null
    done
}

ran_all_tests() {
    local n; n=$(grep -c "\"$1\"" "$RT_OUT/speed.csv")
    echo "$1: $n of 5 tests recorded"
    [ "$n" -eq 5 ]
}

# compare_with_last_run — a NOTE for each result more than 20% worse than the
# same place and test in the previous run in the history file.
compare_with_last_run() {
    [ -f "$HISTORY" ] || return 0
    local last; last=$(grep -v "^\"$RT_ID\"" "$HISTORY" | tail -1 | cut -d, -f1)
    [ -n "$last" ] || return 0
    while IFS=, read -r _run place test riops wiops _rbw _wbw; do
        local prev now before
        prev=$(grep "^$last,$place,$test," "$HISTORY" | head -1)
        [ -n "$prev" ] || continue
        now=$((riops + wiops))
        before=$(echo "$prev" | awk -F, '{print $4 + $5}')
        if [ "$before" -gt 0 ] && [ $((now * 100)) -lt $((before * 80)) ]; then
            _record NOTE "speed warning: $place $test fell from $before to $now IOPS (more than 20% below run $last)"
        fi
    done < "$RT_OUT/speed.csv"
}

run() {
    oc get sc "$BASELINE_SC" >/dev/null 2>&1 || skip "the cluster has no baseline class $BASELINE_SC"
    rt_namespace "$NS"
    : >"$RT_OUT/speed.csv"
    create_pool
    check "the speed pool reaches Ready" wait_until 900 10 pool_is_ready "$POOL"

    claim pool-disk "$POOL" 10Gi
    claim baseline-disk "$BASELINE_SC" 200Gi
    claim vm-disk "$POOL" 10Gi
    fio_pod fio-pool pool-disk
    check "fio runs on a pool volume" wait_until 900 15 pod_succeeded fio-pool
    oc logs -n "$NS" fio-pool | record pool >>"$RT_OUT/speed.csv"
    fio_pod fio-baseline baseline-disk
    check "fio runs on the baseline class" wait_until 900 15 pod_succeeded fio-baseline
    oc logs -n "$NS" fio-baseline | record "$BASELINE_SC" >>"$RT_OUT/speed.csv"

    if oc get datasources.cdi.kubevirt.io -n "$IMAGES_NS" "$CATALOG_IMAGE" >/dev/null 2>&1; then
        fio_vm
        check "fio runs inside a VM on a pool disk" wait_until 1800 30 vm_done
        vm_log | tr -d '\r' | record vm >>"$RT_OUT/speed.csv"
    else
        not_checked "fio inside a VM" "the cluster has no $CATALOG_IMAGE catalog image"
    fi

    check "every pool test has a result" ran_all_tests pool
    check "every baseline test has a result" ran_all_tests "$BASELINE_SC"
    compare_with_last_run
    cat "$RT_OUT/speed.csv" >>"$HISTORY"
}

cleanup() {
    oc delete namespace "$NS" --ignore-not-found --wait=true --timeout=600s
    oc delete filesharepool "$POOL" --ignore-not-found --wait=true --timeout=600s
}
