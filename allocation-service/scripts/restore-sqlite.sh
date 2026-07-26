#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "usage: $0 <backup-path> <db-path>" >&2
  exit 2
fi

backup_path="$1"
db_path="$2"
if [[ ! -f "$backup_path" ]]; then
  echo "backup not found: $backup_path" >&2
  exit 1
fi
if ! command -v sqlite3 >/dev/null 2>&1; then
  echo "sqlite3 is required for restore integrity validation" >&2
  exit 1
fi
if [[ "$(sqlite3 "$backup_path" 'PRAGMA integrity_check;')" != "ok" ]]; then
  echo "backup integrity_check failed: $backup_path" >&2
  exit 1
fi

mkdir -p "$(dirname "$db_path")"
chmod 700 "$(dirname "$db_path")"
cp "$backup_path" "$db_path"
chmod 600 "$db_path"
printf 'restore_path=%s\n' "$db_path"
