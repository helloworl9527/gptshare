#!/bin/sh
set -eu
root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
unit="$root/deploy/chatgpt-monitor.service"
nginx="$root/deploy/nginx.conf"

require_line() {
  file=$1
  line=$2
  grep -Fqx "$line" "$file" || { echo "missing required line: $line" >&2; exit 1; }
}

require_line "$unit" 'User=chatgpt-monitor'
require_line "$unit" 'Group=chatgpt-monitor'
require_line "$unit" 'UMask=0077'
require_line "$unit" 'NoNewPrivileges=true'
require_line "$unit" 'PrivateTmp=true'
require_line "$unit" 'ProtectSystem=strict'
require_line "$unit" 'ProtectHome=true'
require_line "$unit" 'StateDirectoryMode=0700'
require_line "$unit" 'CapabilityBoundingSet='
require_line "$unit" 'EnvironmentFile=/etc/chatgpt-monitor/server.env'
grep -Fq 'listen 127.0.0.1:__HTTPS_PORT__ ssl;' "$nginx"
grep -Fq 'proxy_pass http://127.0.0.1:8080;' "$nginx"
grep -Fq "script-src 'self'" "$root/deploy/security_headers.conf"
if grep -Eq 'unsafe-inline|unsafe-eval' "$root/deploy/security_headers.conf"; then
  echo 'unsafe CSP directive found' >&2
  exit 1
fi
printf '%s\n' 'static_deploy_contract=pass' 'systemd_runtime_validation=not_executed_on_macos'
