#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
nginx_conf="$root/deploy/nginx.local.conf"
service_file="$root/deploy/allocation-service.service"
failures=0

expect() {
  local label="$1"
  local pattern="$2"
  local file="$3"
  if grep -Eq "$pattern" "$file"; then
    printf 'PASS %s\n' "$label"
  else
    printf 'FAIL %s missing pattern %s in %s\n' "$label" "$pattern" "$file" >&2
    failures=$((failures + 1))
  fi
}

expect phase1-proxy-port 'server 127\.0\.0\.1:8080;' "$nginx_conf"
expect phase2-proxy-port 'server 127\.0\.0\.1:9090;' "$nginx_conf"
expect phase1-tls-port 'listen 127\.0\.0\.1:9443 ssl;' "$nginx_conf"
expect phase2-tls-port 'listen 127\.0\.0\.1:9444 ssl;' "$nginx_conf"
expect nginx-http2 'http2 on;' "$nginx_conf"
expect systemd-resource-memory 'MemoryMax=2560M' "$service_file"
expect systemd-resource-cpu 'CPUQuota=200%' "$service_file"

if command -v nginx >/dev/null 2>&1; then
  tmp_dir="$(mktemp -d)"
  tmp_conf="$(mktemp)"
  nginx_include="$tmp_dir/nginx.local.conf"
  if command -v openssl >/dev/null 2>&1; then
    openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
      -subj '/CN=allocation.localhost' \
      -keyout "$tmp_dir/local.key" \
      -out "$tmp_dir/local.crt" >/dev/null 2>&1
    sed \
      -e "s#/etc/ssl/local/phase-one-monitor.crt#$tmp_dir/local.crt#g" \
      -e "s#/etc/ssl/local/phase-one-monitor.key#$tmp_dir/local.key#g" \
      -e "s#/etc/ssl/local/allocation-service.crt#$tmp_dir/local.crt#g" \
      -e "s#/etc/ssl/local/allocation-service.key#$tmp_dir/local.key#g" \
      "$nginx_conf" > "$nginx_include"
  else
    cp "$nginx_conf" "$nginx_include"
  fi
  cat > "$tmp_conf" <<EOF
events {}
http {
  include $nginx_include;
}
EOF
  nginx -t -c "$tmp_conf" -p "$root" || failures=$((failures + 1))
  rm -rf "$tmp_dir" "$tmp_conf"
else
  printf 'SKIP nginx-runtime-parse nginx_not_installed\n'
fi
if command -v systemd-analyze >/dev/null 2>&1; then
  systemd-analyze verify "$service_file" || failures=$((failures + 1))
else
  printf 'SKIP systemd-runtime-verify systemd_analyze_not_installed\n'
fi

if [[ "$failures" -ne 0 ]]; then
  printf 'port_isolation_validation=fail failures=%d\n' "$failures" >&2
  exit 1
fi
printf 'port_isolation_validation=pass failures=0\n'
