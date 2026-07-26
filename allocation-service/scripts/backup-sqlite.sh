#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: $0 <db-path>" >&2
  exit 2
fi

db_path="$1"
if [[ ! -f "$db_path" ]]; then
  echo "database not found: $db_path" >&2
  exit 1
fi
if ! command -v sqlite3 >/dev/null 2>&1; then
  echo "sqlite3 is required for WAL-safe backup" >&2
  exit 1
fi

backup_path="${db_path}.manual-$(date -u +%Y%m%dT%H%M%SZ).bak"
sqlite3 "$db_path" "PRAGMA wal_checkpoint(FULL);" ".backup '$backup_path'"
if [[ "$(sqlite3 "$backup_path" 'PRAGMA integrity_check;')" != "ok" ]]; then
  echo "backup integrity_check failed: $backup_path" >&2
  rm -f "$backup_path"
  exit 1
fi
chmod 600 "$backup_path"
printf 'backup_path=%s\n' "$backup_path"
