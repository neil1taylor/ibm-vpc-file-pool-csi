#!/usr/bin/env bash
# test-backup-cos.sh — Tests for the helpers of the COS part of the backup
# end-to-end test (backup-cos.sh), with a stub oc. Needs no cluster and no COS.
#
# Usage: bash test/e2e/lib/test-backup-cos.sh

set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=backup-cos.sh
source "$HERE/backup-cos.sh"
FAILED=0
WORK="$(mktemp -d)"
trap 'find "$WORK" -type f -delete; find "$WORK" -depth -type d -empty -delete' EXIT

pass() { echo "PASS  $*"; }
fail() { echo "FAIL  $*"; FAILED=1; }
# ok DESC CMD... — pass when CMD succeeds.
ok() { local d="$1"; shift; if "$@" >/dev/null 2>&1; then pass "$d"; else fail "$d"; fi; }
# no DESC CMD... — pass when CMD fails.
no() { local d="$1"; shift; if "$@" >/dev/null 2>&1; then fail "$d"; else pass "$d"; fi; }
eq() { if [ "$2" = "$3" ]; then pass "$1"; else fail "$1: got '$2', want '$3'"; fi; }

clear_env() { unset COS_BUCKET COS_ENDPOINT KP_INSTANCE_ID KP_REGION KP_ROOT_KEY_ID COS_PREFIX_BASE COS_DR_KUBECONFIG COS_KEY_FILE KP_APIKEY_FILE COS_WRAPPED_KEY_FILE COS_EXPORT; }

# --- settings file ---
clear_env
cat >"$WORK/cos-stage.env" <<'ENV'
# a comment
COS_BUCKET=my-bucket
COS_ENDPOINT="s3.eu-de.cloud-object-storage.appdomain.cloud"
KP_REGION=eu-de
SOMETHING_ELSE=bad
ENV
KP_REGION=us-south
cos_load_settings "$WORK/cos-stage.env"
eq "a setting is read from the file" "${COS_BUCKET:-}" "my-bucket"
eq "quotes around a value are removed" "${COS_ENDPOINT:-}" "s3.eu-de.cloud-object-storage.appdomain.cloud"
eq "the environment wins over the file" "$KP_REGION" "us-south"
eq "an unknown name is not set" "${SOMETHING_ELSE:-unset}" "unset"
ok "a missing settings file is not an error" cos_load_settings "$WORK/none.env"

# --- wanted / missing ---
clear_env
no "COS is not wanted without a bucket" cos_wanted
COS_BUCKET=b
ok "COS is wanted with a bucket" cos_wanted
COS_EXPORT=0
no "COS_EXPORT=0 turns it off" cos_wanted
unset COS_EXPORT

COS_CONFIG_DIR="$WORK/conf"
mkdir -p "$COS_CONFIG_DIR"
out=$(cos_missing_config); rc=$?
eq "missing config is reported" "$rc" "1"
for want in "COS_ENDPOINT is not set" "KP_INSTANCE_ID is not set" "KP_ROOT_KEY_ID is not set" "COS_KEY_FILE: cannot read $WORK/conf/cos-key.json" "KP_APIKEY_FILE: cannot read $WORK/conf/kp-apikey.json" "COS_WRAPPED_KEY_FILE: cannot read $WORK/conf/backup-key-secret.yaml"; do
    if echo "$out" | grep -qF "$want"; then pass "missing list has: $want"; else fail "missing list has: $want"; fi
done
COS_ENDPOINT=e KP_INSTANCE_ID=i KP_REGION=r KP_ROOT_KEY_ID=k
touch "$COS_CONFIG_DIR/cos-key.json" "$COS_CONFIG_DIR/kp-apikey.json" "$COS_CONFIG_DIR/backup-key-secret.yaml"
ok "nothing missing when all is given" cos_missing_config
COS_DR_KUBECONFIG="$WORK/nokc"
no "an unreadable DR kubeconfig is reported" cos_missing_config

# --- prefix ---
eq "prefix joins base, run and stamp" "$(cos_prefix stage18 rt-1022-0900-backup 20261022090000)" "stage18/rt-1022-0900-backup-20261022090000"
if [ "$(cos_prefix stage18 a 1)" != "$(cos_prefix stage18 a 2)" ]; then pass "two stamps give two prefixes"; else fail "two stamps give two prefixes"; fi
no "a prefix with a space is refused" cos_prefix "stage 18" a 1
no "a prefix with .. is refused" cos_prefix stage18 .. 1

# --- export block ---
COS_BUCKET=b COS_ENDPOINT=e.example KP_INSTANCE_ID=inst KP_REGION=eu-de KP_ROOT_KEY_ID=root
y=$(cos_export_yaml stage18/x-1 cred wrapped apik)
for want in "  export:" "    bucket: b" "    prefix: stage18/x-1" "    credentialsSecret: cred" "    backupKeySecret: wrapped" "      apiKeySecret: apik" "    keepFor: 25h" "    lock: Compliance"; do
    if echo "$y" | grep -qxF -- "$want"; then pass "export block has: $want"; else fail "export block has: $want"; fi
done

# --- status and listing ---
good='{"status":{"volumes":[{"exportedTime":"2026-10-22T09:00:00Z","exportedBytes":10},{"exportedTime":"2026-10-22T09:00:00Z","exportedBytes":5},{"exportedTime":"2026-10-22T09:01:00Z","exportedBytes":1}]}}'
ok "three exported volumes pass" cos_volumes_exported "$good"
no "two exported volumes fail" cos_volumes_exported '{"status":{"volumes":[{"exportedTime":"2026-10-22T09:00:00Z","exportedBytes":10}]}}'
no "zero bytes fails" cos_volumes_exported '{"status":{"volumes":[{"exportedTime":"t","exportedBytes":0},{"exportedTime":"t","exportedBytes":1},{"exportedTime":"t","exportedBytes":1}]}}'
no "an export error fails" cos_volumes_exported '{"status":{"volumes":[{"exportedTime":"t","exportedBytes":1,"exportError":"boom"},{"exportedTime":"t","exportedBytes":1},{"exportedTime":"t","exportedBytes":1}]}}'
no "no volumes fails" cos_volumes_exported '{"status":{}}'
listing=$'VOLUME  MOMENT\npvc-aaa  2026-10-22T09:00:00Z\npvc-aaa  2026-10-22T09:05:00Z\npvc-bbb  2026-10-22T09:00:00Z\npvc-bbb  -'
eq "two moments counted" "$(cos_moment_count "$listing" pvc-aaa)" "2"
eq "a volume with no moment counts 0" "$(cos_moment_count "$listing" pvc-bbb)" "1"
eq "an unknown volume counts 0" "$(cos_moment_count "$listing" pvc-ccc)" "0"

# --- secrets: values go file to file to Secret, never on a command line ---
mkdir -p "$WORK/bin"
SECRET_AK="AKIA-secret-access-id" SECRET_SK="sk-very-secret-value" SECRET_API="apikey-very-secret" SECRET_WRAP="wrapped-key-value"
cat >"$WORK/conf/cos-key.json" <<JSON
{"credentials":{"cos_hmac_keys":{"access_key_id":"$SECRET_AK","secret_access_key":"$SECRET_SK"}}}
JSON
echo "{\"apikey\":\"$SECRET_API\"}" >"$WORK/conf/kp-apikey.json"
printf 'apiVersion: v1\nkind: Secret\nstringData:\n  wrappedKey: "%s"\n' "$SECRET_WRAP" >"$WORK/conf/backup-key-secret.yaml"
cat >"$WORK/bin/oc" <<'STUB'
#!/usr/bin/env bash
echo "$*" >>"$STUB_ARGS"
case "$1 $2" in
    "get secret") exit 1 ;;
    "create -f") printf '{"stringData":{"wrappedKey":"%s"}}' "$STUB_WRAP" ;;
    "create secret")
        for a in "$@"; do
            case "$a" in --from-file=*) kv="${a#--from-file=}"; echo "${kv%%=*}=$(cat "${kv#*=}")" >>"$STUB_SEEN"; echo "${kv#*=}" >>"$STUB_PATHS" ;; esac
        done ;;
esac
STUB
chmod +x "$WORK/bin/oc"
: >"$WORK/args"; : >"$WORK/seen"; : >"$WORK/paths"
out=$(PATH="$WORK/bin:$PATH" STUB_ARGS="$WORK/args" STUB_SEEN="$WORK/seen" STUB_PATHS="$WORK/paths" STUB_WRAP="$SECRET_WRAP" cos_make_secrets kube-system rt1 2>&1); rc=$?
eq "cos_make_secrets succeeds" "$rc" "0"
for want in "access_key_id=$SECRET_AK" "secret_access_key=$SECRET_SK" "apikey=$SECRET_API" "wrappedKey=$SECRET_WRAP"; do
    if grep -qxF -- "$want" "$WORK/seen"; then pass "a Secret holds ${want%%=*}"; else fail "a Secret holds ${want%%=*}"; fi
done
for v in "$SECRET_AK" "$SECRET_SK" "$SECRET_API" "$SECRET_WRAP"; do
    if grep -qF -- "$v" "$WORK/args" || echo "$out" | grep -qF -- "$v"; then fail "a secret value is on a command line or in the output"; else pass "a secret value is not on a command line or in the output"; fi
done
for n in rt1-cos-credentials rt1-kp-apikey rt1-cos-backup-key; do
    if grep -qF -- "create secret generic $n -n kube-system" "$WORK/args"; then pass "Secret $n is made"; else fail "Secret $n is made"; fi
done
left=0; while read -r f; do [ -e "$f" ] && left=1; done <"$WORK/paths"
eq "the temporary secret files are removed" "$left" "0"

# a manifest with base64 data works too
printf 'apiVersion: v1\nkind: Secret\ndata:\n  wrappedKey: x\n' >"$WORK/conf/backup-key-secret.yaml"
cat >"$WORK/bin/oc" <<'STUB'
#!/usr/bin/env bash
case "$1 $2" in
    "get secret") exit 1 ;;
    "create -f") printf '{"data":{"wrappedKey":"%s"}}' "$(printf 'from-base64' | base64)" ;;
    "create secret") for a in "$@"; do case "$a" in --from-file=wrappedKey=*) cat "${a#--from-file=wrappedKey=}" >"$STUB_SEEN" ;; esac; done ;;
esac
STUB
PATH="$WORK/bin:$PATH" STUB_SEEN="$WORK/seen2" cos_make_secrets kube-system rt2 >/dev/null 2>&1
eq "a base64 wrappedKey is decoded" "$(cat "$WORK/seen2" 2>/dev/null)" "from-base64"

# a missing key file is an error that names the file and no value
rm "$WORK/conf/kp-apikey.json"
: >"$WORK/args"
out=$(PATH="$WORK/bin:$PATH" STUB_SEEN="$WORK/seen3" cos_make_secrets kube-system rt3 2>&1); rc=$?
eq "a missing API key file fails" "$rc" "1"
if echo "$out" | grep -qF "kp-apikey.json"; then pass "the error names the file"; else fail "the error names the file"; fi

# --- cos-list wait: one export of a volume is enough (it holds the earlier moments as undo files) ---
mkdir -p "$WORK/list"
cat >"$WORK/bin/kubectl-backup" <<'STUB'
#!/usr/bin/env bash
vol=""; while [ $# -gt 0 ]; do [ "$1" = "--volume" ] && vol="$2"; shift; done
[ -f "$STUB_LIST/${vol:-all}.out" ] && cat "$STUB_LIST/${vol:-all}.out"
rc=0; [ -f "$STUB_LIST/${vol:-all}.rc" ] && rc=$(cat "$STUB_LIST/${vol:-all}.rc")
exit "$rc"
STUB
chmod +x "$WORK/bin/kubectl-backup"
BACKUP_BIN="$WORK/bin/kubectl-backup" STUB_LIST="$WORK/list" COS_SOURCE_FLAGS=(--policy p) COS_POLL_SECONDS=0
export STUB_LIST
COS_PV_ROOT=pvc-root COS_PV_DATA=pvc-data COS_PV_FILES=pvc-files
printf 'VOLUME  MOMENT\npvc-root  2026-10-22T09:00:00Z\npvc-root  2026-10-22T09:05:00Z\n' >"$WORK/list/pvc-root.out"
printf 'VOLUME  MOMENT\npvc-data  2026-10-22T09:00:00Z\n' >"$WORK/list/pvc-data.out"
printf 'VOLUME  MOMENT\npvc-files  2026-10-22T09:18:54Z\n' >"$WORK/list/pvc-files.out"
ok "one export of the pod volume and two of a disk pass" cos_exports_listed
printf 'No backups of policy p found in bucket b\n' >"$WORK/list/pvc-files.out"
no "a volume with no export fails" cos_exports_listed
printf 'VOLUME  MOMENT\npvc-files  -\n' >"$WORK/list/pvc-files.out"
no "a volume line with no moment fails" cos_exports_listed
printf 'VOLUME  MOMENT\npvc-files  2026-10-22T09:18:54Z\n' >"$WORK/list/pvc-files.out"
echo 1 >"$WORK/list/pvc-data.rc"
no "a cos-list that exits non-zero fails" cos_exports_listed
for want in "cos-list --volume pvc-data (exit 1)" "cos-list --volume pvc-root (exit 0)" "pvc-files  2026-10-22T09:18:54Z"; do
    if echo "$COS_LIST_REPORT" | grep -qF -- "$want"; then pass "the report has: $want"; else fail "the report has: $want"; fi
done
rm "$WORK/list/pvc-data.rc"
ok "the wait returns at once when all are listed" cos_wait_listed 5
echo 2 >"$WORK/list/pvc-root.rc"
SECONDS=0
no "the wait fails when a volume cannot be listed" cos_wait_listed 0
if echo "$COS_LIST_REPORT" | grep -qF "cos-list --volume pvc-root (exit 2)"; then pass "the report after a failed wait names the exit status"; else fail "the report after a failed wait names the exit status"; fi
printf 'VOLUME  MOMENT\npvc-root  2026-10-22T09:00:00Z\n' >"$WORK/list/all.out"
out=$(cos_list_diagnose)
for want in "kubectl backup cos-list (exit 0)" "pvc-root  2026-10-22T09:00:00Z" "cos-list --volume pvc-root (exit 2)"; do
    if echo "$out" | grep -qF -- "$want"; then pass "the diagnostics have: $want"; else fail "the diagnostics have: $want"; fi
done

# --- the locked-objects note ---
COS_BUCKET=bk
n=$(cos_locked_note stage18/x-1)
if echo "$n" | grep -qF "s3://bk/stage18/x-1/" && echo "$n" | grep -qF "cannot be deleted"; then pass "the note names the prefix and says objects stay"; else fail "the note names the prefix and says objects stay"; fi

exit "$FAILED"
