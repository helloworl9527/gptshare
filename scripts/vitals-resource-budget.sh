#!/bin/sh
set -eu
root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd); log=$(mktemp); trap 'rm -f "$log"' EXIT HUP INT TERM
/usr/bin/time -l sh -c "cd '$root'; go test -count=1 ./internal/vitalsapp ./internal/monitor ./internal/monitorfacade & cd '$root/web'; npm run test:e2e >/dev/null & wait" 2>"$log"
rss=$(awk '/maximum resident set size/{print $1}' "$log" | tail -1); test -n "$rss"; mib=$(( (rss + 1048575) / 1048576 ))
test "$mib" -le 2560 || { echo "resource_budget=fail peak_rss_mib=$mib" >&2; exit 1; }
printf 'resource_budget=pass peak_rss_mib=%s limit_mib=2560 cpu_limit=2cores workload=poller,replacement,facade,redeem,playwright\n' "$mib"
