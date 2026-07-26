#!/bin/sh
set -eu
root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd); tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT HUP INT TERM
unit="$root/deploy/vitals/vitals.service"; env="$root/deploy/vitals/server.env.example"
rollback_monitor="$root/deploy/vitals/rollback-monitor.env.example"
rollback_allocation="$root/deploy/vitals/rollback-allocation.env.example"
nginx_template="$root/deploy/vitals/nginx.conf"
grep -Fq 'ExecStartPre=/opt/vitals/current/scripts/vitals-preflight.sh' "$unit"
grep -Fq 'ExecStartPre=/opt/vitals/current/scripts/vitals-backup.sh' "$unit"
grep -Fq 'ExecStartPre=/opt/vitals/current/bin/vitals-migrate' "$unit"
grep -Fq 'ExecStart=/opt/vitals/current/bin/vitals' "$unit"
grep -Fq 'MemoryMax=2560M' "$unit"; grep -Fq 'CPUQuota=200%' "$unit"
grep -Fq 'VITALS_PORT=127.0.0.1:8080' "$env"
grep -Fq 'VITALS_MONITOR_COMPAT_HTTP_ENABLED=false' "$env"; ! grep -q '^ALLOCATION_SERVICE_API_KEY=' "$env"
grep -Eq '^ALLOCATION_SERVICE_API_KEY=__REPLACE_WITH_ORIGINAL_STRONG_PHASE_ONE_API_KEY__$' "$rollback_monitor"
grep -Eq '^ALLOCATION_MONITOR_API_KEY=__REPLACE_WITH_SAME_PHASE_ONE_API_KEY__$' "$rollback_allocation"
grep -Eq '^ALLOCATION_MONITOR_BASE_URL=https://' "$rollback_allocation"
test "$(grep -Fc 'server 127.0.0.1:8080;' "$nginx_template")" -eq 1
grep -Fq 'ssl_protocols TLSv1.2 TLSv1.3;' "$nginx_template"
grep -Fq 'Strict-Transport-Security "max-age=31536000; includeSubDomains" always;' "$nginx_template"
grep -Fq "Content-Security-Policy \"default-src 'self'; script-src 'self'; style-src 'self';" "$nginx_template"
! grep -Fq 'unsafe-inline' "$nginx_template"
grep -Fq 'location / {' "$nginx_template"; grep -Fq 'proxy_pass http://vitals_unified;' "$nginx_template"
openssl req -x509 -newkey rsa:2048 -nodes -subj '/CN=vitals.localhost' -keyout "$tmp/key.pem" -out "$tmp/cert.pem" -days 1 >/dev/null 2>&1
sed -e "s|__PID_PATH__|$tmp/nginx.pid|" -e 's|__ERROR_LOG__|stderr|' -e "s|__TLS_CERT__|$tmp/cert.pem|" -e "s|__TLS_KEY__|$tmp/key.pem|" "$nginx_template" > "$tmp/nginx.conf"
nginx -t -c "$tmp/nginx.conf" -p "$tmp" >/dev/null
printf '%s\n' 'deploy_contract=pass nginx_config=ok single_upstream=127.0.0.1:8080 systemd_runtime=not_executed_on_macos'
