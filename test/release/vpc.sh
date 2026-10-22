#!/usr/bin/env bash
# vpc.sh — Read-only VPC API calls for release test stages.
#
# On the build server the token comes from the instance metadata service and
# the trusted profile with ID RT_VPC_PROFILE_ID (read-only VPC access), so no
# key is stored. remote.sh looks the ID up. The metadata service accepts a
# profile ID or CRN, not a name. Set RT_VPC_TOKEN to use a token you already
# have instead.

RT_VPC_PROFILE_ID="${RT_VPC_PROFILE_ID:-}"
RT_VPC_REGION="${RT_VPC_REGION:-eu-de}"
RT_VPC_API="https://${RT_VPC_REGION}.iaas.cloud.ibm.com/v1"
RT_METADATA="http://api.metadata.cloud.ibm.com"
_vpc_token_file() { echo "$RT_OUT/.vpc-token"; }

# vpc_token — print an IAM token, renewed when it is older than 30 minutes.
vpc_token() {
    if [ -n "${RT_VPC_TOKEN:-}" ]; then echo "$RT_VPC_TOKEN"; return 0; fi
    [ -n "$RT_VPC_PROFILE_ID" ] || return 1
    local f; f=$(_vpc_token_file)
    if [ -s "$f" ] && [ -z "$(find "$f" -mmin +30 2>/dev/null)" ]; then cat "$f"; return 0; fi
    local identity token
    identity=$(curl -sf -m 10 -X PUT "$RT_METADATA/instance_identity/v1/token?version=2022-03-01" \
        -H "Metadata-Flavor: ibm" -H "Content-Type: application/json" -d '{"expires_in": 3600}' |
        jq -r '.access_token // empty') || return 1
    [ -n "$identity" ] || return 1
    token=$(curl -sf -m 10 -X POST "$RT_METADATA/instance_identity/v1/iam_token?version=2022-03-01" \
        -H "Authorization: Bearer $identity" -H "Content-Type: application/json" \
        -d "{\"trusted_profile\": {\"id\": \"$RT_VPC_PROFILE_ID\"}}" |
        jq -r '.access_token // empty') || return 1
    [ -n "$token" ] || return 1
    (umask 077; echo "$token" >"$f")
    echo "$token"
}

# vpc_available — true when the VPC API can be read.
vpc_available() { vpc_token >/dev/null 2>&1; }

# vpc_get PATH — GET a VPC API path, e.g. /shares/ID. Prints the JSON body.
vpc_get() {
    local token sep="?"
    token=$(vpc_token) || return 1
    case "$1" in *\?*) sep="&" ;; esac
    curl -sf -m 30 -H "Authorization: Bearer $token" "$RT_VPC_API$1${sep}version=2026-04-28&generation=2"
}

# vpc_share_names — print the name of every share in the region, one per line.
vpc_share_names() {
    local path="/shares?limit=100" page start
    while :; do
        page=$(vpc_get "$path") || return 1
        echo "$page" | jq -r '.shares[].name'
        start=$(echo "$page" | jq -r '.next.href // empty' | sed -n 's/.*[?&]start=\([^&]*\).*/\1/p')
        [ -n "$start" ] || return 0
        path="/shares?limit=100&start=$start"
    done
}

# vpc_leftovers — record whether any VPC share of this run is left. Called by the sweep.
vpc_leftovers() {
    if ! vpc_available; then
        not_checked "VPC shares left behind" "no VPC access"
        return 0
    fi
    # Pool deletion returns before the VPC has finished deleting the shares.
    local left
    _no_run_shares() { ! vpc_share_names | grep -q "^rt-${RT_ID}-"; }
    wait_until 600 15 _no_run_shares
    left=$(vpc_share_names | grep "^rt-${RT_ID}-")
    if [ -n "$left" ]; then _record FAIL "VPC shares left behind: $(echo "$left" | tr '\n' ' ')"
    else _record PASS "no VPC shares left behind"
    fi
}
