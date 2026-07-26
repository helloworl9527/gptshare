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
result="$(sqlite3 "$db_path" 'PRAGMA integrity_check;')"
printf '%s\n' "$result"
if [[ "$result" != "ok" ]]; then
  exit 1
fi
