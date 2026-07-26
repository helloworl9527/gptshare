#!/bin/sh
set -eu
umask 077

root=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
output=${1:-"$root/.workflow/evidence/step-11-resource.csv"}
duration=${STEP11_RESOURCE_DURATION:-30m}
upstream_delay=${STEP11_RESOURCE_UPSTREAM_DELAY:-9s}
nginx_bin=${NGINX_BIN:-$(command -v nginx || true)}

if [ -z "$nginx_bin" ]; then
  echo "nginx is required for the STEP-11 combined resource measurement" >&2
  exit 69
fi

work=$(mktemp -d)
test_pid=
cleanup() {
  "$nginx_bin" -p "$work/" -c "$work/nginx.conf" -s stop >/dev/null 2>&1 || true
  if [ -n "$test_pid" ]; then
    kill "$test_pid" >/dev/null 2>&1 || true
  fi
  rm -rf "$work"
}
trap cleanup EXIT HUP INT TERM
chmod 700 "$work"
mkdir -p "$(dirname "$output")"

openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=localhost \
  -keyout "$work/key.pem" -out "$work/cert.pem" >/dev/null 2>&1
chmod 600 "$work/key.pem" "$work/cert.pem"

sed \
  -e 's/^worker_processes auto;/worker_processes 1;/' \
  -e "s|__PID_PATH__|$work/nginx.pid|g" \
  -e "s|__ERROR_LOG__|$work/error.log|g" \
  -e "s|__MIME_TYPES__|/opt/homebrew/etc/nginx/mime.types|g" \
  -e "s|__ACCESS_LOG__|$work/access.log|g" \
  -e 's|__HTTP_PORT__|28080|g' \
  -e 's|__HTTPS_PORT__|28443|g' \
  -e 's|__SERVER_NAME__|localhost|g' \
  -e "s|__TLS_CERT__|$work/cert.pem|g" \
  -e "s|__TLS_KEY__|$work/key.pem|g" \
  -e "s|__WEB_ROOT__|$root/artifacts/release/web|g" \
  -e "s|__SECURITY_HEADERS__|$root/deploy/security_headers.conf|g" \
  -e "s|__PROXY_PARAMS__|$root/deploy/proxy_params.conf|g" \
  "$root/deploy/nginx.conf" >"$work/nginx.conf"

"$nginx_bin" -t -p "$work/" -c "$work/nginx.conf"
"$nginx_bin" -p "$work/" -c "$work/nginx.conf"

(cd "$root" && go test -c -o "$work/resource.test" ./test/e2e)
phase_file="$work/phase"
result_file="$work/result.log"
printf 'startup\n' >"$phase_file"
RUN_STEP11_RESOURCE=1 \
STEP11_RESOURCE_DURATION="$duration" \
STEP11_RESOURCE_UPSTREAM_DELAY="$upstream_delay" \
STEP11_RESOURCE_PHASE_FILE="$phase_file" \
STEP11_PROJECT_ROOT="$root" \
GOMAXPROCS=2 GOMEMLIMIT=2500MiB \
  "$work/resource.test" -test.run '^TestResource100Accounts30Minutes$' -test.v >"$result_file" 2>&1 &
test_pid=$!

printf 'timestamp_utc,phase,go_rss_kib,go_cpu_pct,nginx_rss_kib,nginx_cpu_pct,total_rss_mib,total_cpu_pct\n' >"$output"
while kill -0 "$test_pid" >/dev/null 2>&1; do
  phase=$(tr -d '\n' <"$phase_file")
  go_row=$(ps -o rss= -o %cpu= -p "$test_pid" | awk 'NF >= 2 {print $1","$2}')
  go_rss=$(printf '%s' "$go_row" | cut -d, -f1)
  go_cpu=$(printf '%s' "$go_row" | cut -d, -f2)
  go_rss=${go_rss:-0}
  go_cpu=${go_cpu:-0}
  nginx_master=$(cat "$work/nginx.pid")
  nginx_pids="$nginx_master $(pgrep -P "$nginx_master" 2>/dev/null || true)"
  nginx_values=$(ps -o rss= -o %cpu= -p "$(printf '%s' "$nginx_pids" | tr ' ' ',' | sed 's/,$//')" 2>/dev/null |
    awk '{rss+=$1; cpu+=$2} END {printf "%.0f,%.3f",rss,cpu}')
  nginx_rss=$(printf '%s' "$nginx_values" | cut -d, -f1)
  nginx_cpu=$(printf '%s' "$nginx_values" | cut -d, -f2)
  total_rss=$(awk -v a="$go_rss" -v b="$nginx_rss" 'BEGIN {printf "%.3f",(a+b)/1024}')
  total_cpu=$(awk -v a="$go_cpu" -v b="$nginx_cpu" 'BEGIN {printf "%.3f",a+b}')
  printf '%s,%s,%s,%s,%s,%s,%s,%s\n' \
    "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$phase" "$go_rss" "$go_cpu" \
    "$nginx_rss" "$nginx_cpu" "$total_rss" "$total_cpu" >>"$output"
  sleep 5
done

set +e
wait "$test_pid"
test_exit=$?
set -e
test_pid=
cat "$result_file"
if [ "$test_exit" -ne 0 ]; then
  exit "$test_exit"
fi

awk -F, '
  NR > 1 {
    count[$2]++
    rss[$2]+=$7
    cpu[$2]+=$8
    if ($7 > maxrss[$2]) maxrss[$2]=$7
    if ($8 > maxcpu[$2]) maxcpu[$2]=$8
    if ($7 > allrss) allrss=$7
  }
  END {
    printf "RESOURCE_SAMPLES total=%d peak_combined_rss_mib=%.3f\n",NR-1,allrss
    for (phase in count) {
      printf "RESOURCE_PHASE phase=%s samples=%d avg_combined_rss_mib=%.3f peak_combined_rss_mib=%.3f avg_sampled_cpu_pct=%.3f peak_sampled_cpu_pct=%.3f\n",
        phase,count[phase],rss[phase]/count[phase],maxrss[phase],cpu[phase]/count[phase],maxcpu[phase]
    }
  }
' "$output"
