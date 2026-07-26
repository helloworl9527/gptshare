#!/usr/bin/env bash
set -euo pipefail

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

go build -o "$tmpdir/allocation-service" ./cmd/allocation-service

key64_a="YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWE="
key64_b="YmJiYmJiYmJiYmJiYmJiYmJiYmJiYmJiYmJiYmJiYmI="
key64_c="Y2NjY2NjY2NjY2NjY2NjY2NjY2NjY2NjY2NjY2NjY2M="
keyurl_m="bW1tbW1tbW1tbW1tbW1tbW1tbW1tbW1tbW1tbW1tbW0"
keyurl_n="bm5ubm5ubm5ubm5ubm5ubm5ubm5ubm5ubm5ubm5ubm4"
totp_secret="OR2HI5DVOJ2HI5DVOJ2HI5DVOJ2HI5DV"
hash='$2b$12$01234567890123456789012345678901234567890123456789012'

base_env=(
  "ALLOCATION_ENV=development"
  "ALLOCATION_PORT=127.0.0.1:0"
  "ALLOCATION_DB_PATH=$tmpdir/allocation.db"
  "ALLOCATION_MONITOR_BASE_URL=http://127.0.0.1:8080"
  "ALLOCATION_MONITOR_API_KEY=kkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkk"
  "ALLOCATION_ADMIN_USER=admin"
  "ALLOCATION_ADMIN_PASSWORD_HASH=$hash"
  "ALLOCATION_ADMIN_TOTP_SECRET=$totp_secret"
  "ALLOCATION_SESSION_SIGNING_KEY=$key64_a"
  "ALLOCATION_CSRF_SIGNING_KEY=$key64_b"
  "ALLOCATION_CREDENTIAL_MASTER_KEYS=alloc-primary:$keyurl_m,alloc-previous:$keyurl_n"
  "ALLOCATION_CREDENTIAL_ACTIVE_KEY_ID=alloc-primary"
)

run_bad() {
  local name="$1"
  shift
  local output
  set +e
  output="$(env -i PATH="$PATH" "${base_env[@]}" "$@" "$tmpdir/allocation-service" 2>&1)"
  local status=$?
  set -e
  if [[ "$status" -eq 0 ]]; then
    printf 'FAIL %s: bad configuration started successfully\n%s\n' "$name" "$output" >&2
    return 1
  fi
  printf 'PASS %s: exit %s\n' "$name" "$status"
}

run_bad "empty-secret" "ALLOCATION_MONITOR_API_KEY="
run_bad "example-secret" "ALLOCATION_MONITOR_API_KEY=change-me"
run_bad "weak-credential-key" "ALLOCATION_CREDENTIAL_MASTER_KEYS=alloc-primary:d2Vhaw"
run_bad "active-key-missing" "ALLOCATION_CREDENTIAL_ACTIVE_KEY_ID=missing"
run_bad "phase-one-name-reuse" "CREDENTIAL_MASTER_KEYS=phase1:$key64_c"
run_bad "phase-one-material-reuse" "CREDENTIAL_MASTER_KEYS=phase1:$key64_c" "ALLOCATION_CREDENTIAL_MASTER_KEYS=alloc-primary:Y2NjY2NjY2NjY2NjY2NjY2NjY2NjY2NjY2NjY2NjY2M"
