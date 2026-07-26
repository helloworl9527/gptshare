#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
log="$(mktemp)"
trap 'rm -f "$log"' EXIT

if /usr/bin/time -l true >/dev/null 2>&1; then
  (
    cd "$root"
    /usr/bin/time -l go test -count=1 ./internal/httpapi -run TestDeploymentEndToEndFlow -v
  ) 2>"$log"
  cat "$log"
  rss_bytes="$(awk '/maximum resident set size/{print $1}' "$log" | tail -1)"
  rss_mib=$(( (rss_bytes + 1048575) / 1048576 ))
elif /usr/bin/time -v true >/dev/null 2>&1; then
  (
    cd "$root"
    /usr/bin/time -v go test -count=1 ./internal/httpapi -run TestDeploymentEndToEndFlow -v
  ) 2>"$log"
  cat "$log"
  rss_kib="$(awk -F: '/Maximum resident set size/{gsub(/ /,"",$2); print $2}' "$log" | tail -1)"
  rss_mib=$(( (rss_kib + 1023) / 1024 ))
else
  (
    cd "$root"
    go test -count=1 ./internal/httpapi -run TestDeploymentEndToEndFlow -v
  )
  printf 'resource_budget=skip time_command_unavailable\n'
  exit 0
fi

if [[ "$rss_mib" -gt 2560 ]]; then
  printf 'resource_budget=fail peak_rss_mib=%s limit_mib=2560 cpu_budget=2cores\n' "$rss_mib" >&2
  exit 1
fi
printf 'resource_budget=pass peak_rss_mib=%s limit_mib=2560 cpu_budget=2cores\n' "$rss_mib"
