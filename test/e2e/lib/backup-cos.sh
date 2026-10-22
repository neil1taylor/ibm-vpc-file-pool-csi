#!/usr/bin/env bash
# backup-cos.sh — COS export part of test/e2e/test-backup.sh (release test
# stage 18, second part). Sourced by that script and by the stage file.
#
# What it needs, none of it in a repository. Settings (not secret) go in
# $COS_CONFIG_DIR/cos-stage.env as NAME=value lines, or in the environment:
#   COS_BUCKET, COS_ENDPOINT          an existing bucket with object lock, and its S3 host
#   KP_INSTANCE_ID, KP_REGION, KP_ROOT_KEY_ID   the Key Protect root key that wrapped the backup key
# Files (default directory ~/.config/pool-csi, or $COS_CONFIG_DIR):
#   cos-key.json            the HMAC key (ibmcloud resource service-key-create output)
#   kp-apikey.json          an IBM Cloud API key with Reader on the root key (ibmcloud iam api-key-create output)
#   backup-key-secret.yaml  the Secret manifest `kubectl backup cos-setup --secret-file` wrote (the wrapped backup key)
# Optional:
#   COS_DR_KUBECONFIG       kubeconfig of the other cluster of the lab pair; the restore is repeated there
#   COS_PREFIX_BASE         folder for all test runs in the bucket (default stage18)
#   COS_EXPORT=0            leave the COS part out
# Secret values are read from the files into other files and then into
# Kubernetes Secrets. They are never put on a command line, in a variable or in the log.

# shellcheck disable=SC2154  # first_vm and first_files are set by test-backup.sh
COS_KEEP_FOR=25h
COS_ON=false
COS_ALLOWED_SETTINGS="COS_BUCKET COS_ENDPOINT KP_INSTANCE_ID KP_REGION KP_ROOT_KEY_ID COS_PREFIX_BASE"

cos_config_dir() { echo "${COS_CONFIG_DIR:-$HOME/.config/pool-csi}"; }
cos_key_file() { echo "${COS_KEY_FILE:-$(cos_config_dir)/cos-key.json}"; }
cos_kp_file() { echo "${KP_APIKEY_FILE:-$(cos_config_dir)/kp-apikey.json}"; }
cos_wrapped_file() { echo "${COS_WRAPPED_KEY_FILE:-$(cos_config_dir)/backup-key-secret.yaml}"; }

# cos_load_settings FILE — read NAME=value lines for the known settings. A name
# that is already set in the environment keeps its value. Other lines are ignored.
cos_load_settings() {
    local file="$1" line name value allowed
    [ -r "$file" ] || return 0
    while IFS= read -r line || [ -n "$line" ]; do
        case "$line" in ''|'#'*) continue ;; esac
        name="${line%%=*}"
        value="${line#*=}"
        [ "$name" != "$line" ] || continue
        for allowed in $COS_ALLOWED_SETTINGS; do
            if [ "$name" = "$allowed" ] && [ -z "${!name:-}" ]; then
                value="${value#\"}"; value="${value%\"}"
                printf -v "$name" '%s' "$value"
                export "${name?}"
            fi
        done
    done <"$file"
}

# cos_wanted — has the person asked for the COS part? Yes when a bucket is named
# (after cos_load_settings) and COS_EXPORT is not 0.
cos_wanted() { [ "${COS_EXPORT:-1}" != "0" ] && [ -n "${COS_BUCKET:-}" ]; }

# cos_missing_config — print what is missing, one item per line; exit 0 if nothing is.
cos_missing_config() {
    local v missing=0
    for v in COS_BUCKET COS_ENDPOINT KP_INSTANCE_ID KP_REGION KP_ROOT_KEY_ID; do
        if [ -z "${!v:-}" ]; then echo "$v is not set"; missing=1; fi
    done
    for v in "COS_KEY_FILE $(cos_key_file)" "KP_APIKEY_FILE $(cos_kp_file)" "COS_WRAPPED_KEY_FILE $(cos_wrapped_file)"; do
        if [ ! -r "${v#* }" ]; then echo "${v%% *}: cannot read ${v#* }"; missing=1; fi
    done
    if [ -n "${COS_DR_KUBECONFIG:-}" ] && [ ! -r "$COS_DR_KUBECONFIG" ]; then
        echo "COS_DR_KUBECONFIG: cannot read $COS_DR_KUBECONFIG"; missing=1
    fi
    return "$missing"
}

# cos_prefix BASE RUN STAMP — the folder of one run in the bucket. STAMP makes it
# unique, so a rerun never meets the locked objects of an earlier run.
cos_prefix() {
    local p="${1:-stage18}/$2-$3"
    case "$p" in
        *[!A-Za-z0-9._/-]*|*..*) echo "the COS prefix '$p' has characters the restore does not allow" >&2; return 1 ;;
    esac
    echo "$p"
}

# cos_export_yaml — the spec.export block (two-space indent, under spec:). Names only, no secret values.
cos_export_yaml() { # prefix credentials-secret wrapped-key-secret apikey-secret
    cat <<YAML
  export:
    bucket: $COS_BUCKET
    endpoint: $COS_ENDPOINT
    prefix: $1
    credentialsSecret: $2
    backupKeySecret: $3
    keyProtect:
      instanceID: $KP_INSTANCE_ID
      region: $KP_REGION
      rootKeyID: $KP_ROOT_KEY_ID
      apiKeySecret: $4
    keepFor: $COS_KEEP_FOR
    lock: Compliance
YAML
}

# cos_volumes_exported POLICY_JSON — do exactly the wanted number of volumes (default 3)
# all have exportedTime and exportedBytes above 0, and no exportError?
cos_volumes_exported() {
    echo "$1" | jq -e --argjson n "${2:-3}" \
        '[.status.volumes[]? | select((.exportedTime // "") != "" and (.exportedBytes // 0) > 0 and (.exportError // "") == "")] | length == $n' >/dev/null
}

# cos_moment_count LISTING VOLUME — how many moments `kubectl backup cos-list` printed for VOLUME.
cos_moment_count() {
    echo "$1" | awk -v v="$2" '$1 == v && $2 ~ /^[0-9]{4}-[0-9]{2}-[0-9]{2}T/ {n++} END {print n + 0}'
}

# cos_private_dir — a private temporary folder for secret files.
cos_private_dir() { local d; d=$(mktemp -d) && chmod 700 "$d" && echo "$d"; }

cos_remove_dir() { [ -d "$1" ] && find "$1" -type f -delete && rmdir "$1"; return 0; }

# cos_make_secrets NAMESPACE PREFIX — the three Secrets the policy and the restore name:
# PREFIX-cos-credentials, PREFIX-cos-backup-key, PREFIX-kp-apikey. Values go from file to
# file to Secret; nothing is echoed. Does nothing for a Secret that exists.
cos_make_secrets() {
    local ns="$1" p="$2" d rc=0
    d=$(cos_private_dir) || return 1
    if ! oc get secret -n "$ns" "$p-cos-credentials" >/dev/null 2>&1; then
        jq -j '.credentials.cos_hmac_keys.access_key_id // empty' "$(cos_key_file)" >"$d/access_key_id" 2>/dev/null
        jq -j '.credentials.cos_hmac_keys.secret_access_key // empty' "$(cos_key_file)" >"$d/secret_access_key" 2>/dev/null
        if [ -s "$d/access_key_id" ] && [ -s "$d/secret_access_key" ]; then
            oc create secret generic "$p-cos-credentials" -n "$ns" \
                --from-file=access_key_id="$d/access_key_id" --from-file=secret_access_key="$d/secret_access_key" >/dev/null || rc=1
        else
            echo "cannot read the HMAC key from $(cos_key_file) (.credentials.cos_hmac_keys)"; rc=1
        fi
    fi
    if ! oc get secret -n "$ns" "$p-kp-apikey" >/dev/null 2>&1; then
        jq -j '.apikey // empty' "$(cos_kp_file)" >"$d/apikey" 2>/dev/null
        if [ -s "$d/apikey" ]; then
            oc create secret generic "$p-kp-apikey" -n "$ns" --from-file=apikey="$d/apikey" >/dev/null || rc=1
        else
            echo "cannot read the API key from $(cos_kp_file) (.apikey)"; rc=1
        fi
    fi
    if ! oc get secret -n "$ns" "$p-cos-backup-key" >/dev/null 2>&1; then
        oc create -f "$(cos_wrapped_file)" --dry-run=client -o json 2>/dev/null |
            jq -j '.stringData.wrappedKey // (.data.wrappedKey // "" | @base64d)' >"$d/wrappedKey" 2>/dev/null
        if [ -s "$d/wrappedKey" ]; then
            oc create secret generic "$p-cos-backup-key" -n "$ns" --from-file=wrappedKey="$d/wrappedKey" >/dev/null || rc=1
        else
            echo "cannot read the wrapped backup key from $(cos_wrapped_file) (a Secret manifest with wrappedKey)"; rc=1
        fi
    fi
    cos_remove_dir "$d"
    return "$rc"
}

cos_delete_secrets() { # namespace prefix
    oc delete secret -n "$1" "$2-cos-credentials" "$2-cos-backup-key" "$2-kp-apikey" --ignore-not-found >/dev/null 2>&1 || true
}

# cos_locked_note PREFIX — what the person must know about the objects left behind.
cos_locked_note() {
    echo "NOTE: objects under s3://$COS_BUCKET/$1/ are locked in Compliance mode for $COS_KEEP_FOR from the time each was written."
    echo "NOTE: they cannot be deleted by anyone, so the test leaves them; they are billed until the lock ends. The prefix is unique to this run."
}

# --- The phase itself. It uses the helpers of test-backup.sh (log, fail,
# --- wait_until, make_pool, pool_ready, vm_yaml, vssh, agent_connected,
# --- files_pod, check_files) and its variables, so it runs only from there.

cos_init() { # sets COS_ON, COS_PREFIX, the Secret names
    COS_ON=false
    cos_load_settings "$(cos_config_dir)/cos-stage.env"
    cos_wanted || { log "COS export is off: no COS_BUCKET set (see test/e2e/lib/backup-cos.sh)"; return 0; }
    local missing
    if ! missing=$(cos_missing_config); then
        echo "$missing"
        fail "the COS export is wanted (COS_BUCKET is set) but its configuration is incomplete; nothing was started"
    fi
    COS_ON=true
    COS_HOME_KC_SET=${KUBECONFIG+x}
    COS_HOME_KC=${KUBECONFIG:-}
    COS_PREFIX=$(cos_prefix "${COS_PREFIX_BASE:-stage18}" "$PREFIX" "$(date -u +%Y%m%d%H%M%S)") || fail "cannot make a COS prefix"
    COS_CRED_SECRET="$PREFIX-cos-credentials"
    COS_KEY_SECRET="$PREFIX-cos-backup-key"
    COS_KP_SECRET="$PREFIX-kp-apikey"
    COSR_POOL="$PREFIX-cosr"
    COS_NS="$NS-cos"
    DRIVER_NS=${DRIVER_NS:-kube-system}
    log "COS prefix: $COS_PREFIX (bucket $COS_BUCKET, keep $COS_KEEP_FOR, Compliance lock)"
    cos_locked_note "$COS_PREFIX"
}

cos_policy_export_yaml() { cos_export_yaml "$COS_PREFIX" "$COS_CRED_SECRET" "$COS_KEY_SECRET" "$COS_KP_SECRET"; }

cos_diagnose() {
    $COS_ON || return 0
    log "COS diagnostics (no secret values are shown)"
    oc get backuppolicy "$POLICY" -o json 2>/dev/null | jq -c '[.status.volumes[]? | {pvc: .pvcName, exportedTime, exportedBytes, exportError}]' || true
    [ -z "${COS_LIST_REPORT:-}" ] || echo "$COS_LIST_REPORT"
    oc get backuprestore -o json 2>/dev/null | jq -c --arg ns "$COS_NS" '.items[] | select(.spec.targetNamespace == $ns) | {name: .metadata.name, phase: .status.phase, fetched: .status.fetched, copied: .status.copied, message: .status.message}' || true
    oc get jobs,pods -n "$DRIVER_NS" -o wide 2>/dev/null | grep -Ei "export|cos|maint|share-job" || true
    oc logs -n "$DRIVER_NS" deploy/ibm-vpc-file-pool-csi-controller -c csi-controller --since=3h 2>/dev/null | grep -Ei "export|cos" | tail -30 || true
}

# cos_exports_ready — has every volume of the policy an exportedTime and exportedBytes?
cos_exports_ready() {
    local json
    json=$(oc get backuppolicy "$POLICY" -o json 2>/dev/null) || return 1
    cos_volumes_exported "$json"
}

# cos_exports_listed — does `kubectl backup cos-list --volume V` show at least one export of each of
# the three volumes? One export is enough for "two moments": an export holds the newest copy and the
# earlier moments as undo files, and cos-list prints one line for each export. A pod volume that
# does not change after its first backup is exported once. The output and exit status of each
# listing go to COS_LIST_REPORT, so a failure can be read without a rerun.
cos_exports_listed() {
    local pv listing rc ok=0
    COS_LIST_REPORT=""
    for pv in "$COS_PV_ROOT" "$COS_PV_DATA" "$COS_PV_FILES"; do
        rc=0
        listing=$("$BACKUP_BIN" cos-list "${COS_SOURCE_FLAGS[@]}" --volume "$pv" --timeout 10m 2>&1) || rc=$?
        COS_LIST_REPORT+="cos-list --volume $pv (exit $rc)"$'\n'"$listing"$'\n'
        if [ "$rc" -ne 0 ] || [ "$(cos_moment_count "$listing" "$pv")" -lt 1 ]; then ok=1; fi
    done
    return "$ok"
}

# cos_wait_listed SECONDS — wait until cos_exports_listed passes. Returns 1 at the limit and prints
# the last report. It does not end the script: the restore checks can still run for what is there.
cos_wait_listed() {
    local end=$((SECONDS + $1))
    until cos_exports_listed; do
        if [ "$SECONDS" -ge "$end" ]; then
            echo "timed out after ${1}s; the last listings:"
            echo "$COS_LIST_REPORT"
            return 1
        fi
        sleep "${COS_POLL_SECONDS:-10}"
    done
}

# cos_list_diagnose — the whole-policy listing with its exit status, then the last per-volume report.
cos_list_diagnose() {
    local out rc=0
    out=$("$BACKUP_BIN" cos-list "${COS_SOURCE_FLAGS[@]}" --timeout 5m 2>&1) || rc=$?
    echo "kubectl backup cos-list (exit $rc)"
    echo "$out"
    echo "${COS_LIST_REPORT:-}"
}

cos_source_flags() {
    COS_SOURCE_FLAGS=(--policy "$POLICY" --bucket "$COS_BUCKET" --endpoint "$COS_ENDPOINT" --prefix "$COS_PREFIX"
        --credentials-secret "$COS_CRED_SECRET" --backup-key-secret "$COS_KEY_SECRET" --kp-api-key-secret "$COS_KP_SECRET"
        --kp-instance "$KP_INSTANCE_ID" --kp-region "$KP_REGION" --kp-root-key "$KP_ROOT_KEY_ID"
        --driver-namespace "$DRIVER_NS")
}

# cos_restore PV TO SIZE_GB [MOMENT]
cos_restore() {
    local moment=()
    [ -z "${4:-}" ] || moment=(--moment "$4")
    "$BACKUP_BIN" restore --from-cos "${COS_SOURCE_FLAGS[@]}" --volume "$1" --to "$2" --size-gb "$3" \
        --storage-class "$COSR_POOL" --skip-marker-cleanup ${moment[@]+"${moment[@]}"} --wait --timeout 1h
}

# cos_restore_phase LABEL — restore the three volumes from COS in the current cluster
# (the cluster KUBECONFIG points at), boot the VM, check the checksums and the pod volume.
cos_restore_phase() {
    local label="$1" NS="$COS_NS" xfs result
    log "COS restore, $label: making pool $COSR_POOL and namespace $NS"
    make_pool "$COSR_POOL"
    oc create ns "$NS" --dry-run=client -o yaml | oc apply -f -
    oc adm policy add-scc-to-user anyuid -z default -n "$NS" >/dev/null
    cos_make_secrets "$DRIVER_NS" "$PREFIX" || fail "cannot make the COS Secrets in $DRIVER_NS"
    cos_source_flags
    wait_until 1200 "pool $COSR_POOL Ready" pool_ready "$COSR_POOL"

    log "COS restore, $label: VM disks at $first_vm"
    cos_restore "$COS_PV_ROOT" "$NS/root-c" 20 "$first_vm"
    cos_restore "$COS_PV_DATA" "$NS/data-c" 10 "$first_vm"
    vm_yaml cosvm root-c data-c no | oc apply -f -
    wait_until 900 "COS-restored guest agent" agent_connected cosvm
    cos_ssh_ok() { vssh cosvm true >/dev/null 2>&1; }
    wait_until 300 "ssh on the COS-restored VM" cos_ssh_ok
    xfs=$(vssh cosvm 'sudo journalctl -k --no-pager | grep -ci "xfs.*\(corrupt\|error\)" || true')
    [ "$xfs" = 0 ] || fail "XFS errors in the kernel log of the VM restored from COS ($label): $xfs"
    result=$(vssh cosvm 'sudo systemctl stop writer; wc -l < /var/log/writer.log; while read n s t; do echo "$s  /data/f$n"; done < /var/log/writer.log | sha256sum -c --quiet && echo CHECKSUMS_OK')
    echo "$result"
    echo "$result" | grep -q CHECKSUMS_OK || fail "files on the data disk restored from COS ($label) do not match the log on the root disk"
    log "PASS: COS restore, $label: the VM restored at $first_vm booted; $(echo "$result" | head -1) files logged, all checksums match"

    log "COS restore, $label: pod volume at $first_files (the first content)"
    cos_restore "$COS_PV_FILES" "$NS/files-c1" 1 "$first_files"
    check_files cos-check-1 files-c1 manifest-A.sha256 'test ! -e /data/b'
    log "PASS: COS restore, $label: the pod volume restored at $first_files holds the first content"

    log "COS restore, $label: pod volume at the latest moment (the changed content)"
    cos_restore "$COS_PV_FILES" "$NS/files-c2" 1
    check_files cos-check-2 files-c2 manifest-B.sha256 'test -e /data/b'
    log "PASS: COS restore, $label: the latest restore holds the changed content"
}

# cos_phase — the whole second part: wait for the exports, list, restore here and, if
# COS_DR_KUBECONFIG is set, in the other cluster too.
cos_phase() {
    local json soft_failed=false
    # COS_PV_ROOT, COS_PV_DATA and COS_PV_FILES are set by test-backup.sh before it deletes the claim "files".
    : "${COS_PV_ROOT:?}" "${COS_PV_DATA:?}" "${COS_PV_FILES:?}"
    cos_source_flags

    log "waiting for the export of every volume (exportedTime, exportedBytes)"
    wait_until "${COS_EXPORT_TIMEOUT:-1800}" "an export of every volume" cos_exports_ready
    json=$(oc get backuppolicy "$POLICY" -o json)
    echo "$json" | jq -c '.status.volumes[] | {pvc: .pvcName, exportedTime, exportedBytes, exportError}'
    log "PASS: COS export: all three volumes have exportedTime and exportedBytes above 0"

    # VM disks export every ~5 minutes, so 30 minutes is plenty. If this check fails, say why and go on:
    # the restores below pick the newest export when no export is at or after the moment.
    log "waiting for an export of every volume in the bucket (kubectl backup cos-list)"
    if cos_wait_listed "${COS_LIST_TIMEOUT:-1800}"; then
        echo "$COS_LIST_REPORT"
        log "PASS: COS export: kubectl backup cos-list shows an export of each of the three volumes (each export holds the earlier moments as undo files)"
    else
        cos_list_diagnose
        log "FAIL: COS export: kubectl backup cos-list does not show an export of each volume; going on with the restore checks"
        soft_failed=true
    fi

    cos_restore_phase "local cluster"
    if [ -n "${COS_DR_KUBECONFIG:-}" ]; then
        log "repeating the restore in the other cluster (kubeconfig $COS_DR_KUBECONFIG)"
        # Not in a subshell: set -e must stay on, and fail must end the script.
        cos_with_kubeconfig "$COS_DR_KUBECONFIG" cos_restore_phase "other cluster"
    fi
    cos_locked_note "$COS_PREFIX"
    ! $soft_failed || fail "a COS check failed (see the FAIL line above); the restore checks ran for what was in the bucket"
}

# cos_with_kubeconfig KUBECONFIG COMMAND... — run COMMAND against another cluster.
cos_with_kubeconfig() {
    local kc="$1" rc=0
    shift
    export KUBECONFIG="$kc"
    "$@" || rc=$?
    cos_restore_home_kubeconfig
    return "$rc"
}

cos_restore_home_kubeconfig() {
    if [ -n "${COS_HOME_KC_SET:-}" ]; then export KUBECONFIG="$COS_HOME_KC"; else unset KUBECONFIG; fi
}

# cos_cleanup_cluster — remove what the COS part made in the cluster KUBECONFIG points at.
# The objects in the bucket stay: Compliance locks cannot be removed.
cos_cleanup_cluster() {
    oc get backuprestore -o json 2>/dev/null | jq -r --arg ns "$COS_NS" '.items[] | select(.spec.targetNamespace == $ns) | .metadata.name' |
        while read -r name; do oc delete backuprestore "$name" --wait=false || true; done || true
    oc delete ns "$COS_NS" --ignore-not-found --wait=true --timeout=15m || true
    oc delete filesharepool "$COSR_POOL" --ignore-not-found --wait=false || true
    cos_delete_secrets "$DRIVER_NS" "$PREFIX"
}

cos_cleanup() {
    $COS_ON || return 0
    cos_restore_home_kubeconfig
    cos_cleanup_cluster
    if [ -n "${COS_DR_KUBECONFIG:-}" ] && [ -r "$COS_DR_KUBECONFIG" ]; then
        cos_with_kubeconfig "$COS_DR_KUBECONFIG" cos_cleanup_cluster || true
    fi
    cos_locked_note "$COS_PREFIX"
}
