#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

db="$tmpdir/private/allocation.db"
mkdir -p "$tmpdir/private"
chmod 0700 "$tmpdir" "$tmpdir/private"

if ! command -v sqlite3 >/dev/null 2>&1; then
  echo "sqlite3 is required for backup restore drill" >&2
  exit 1
fi

sqlite3 "$db" "CREATE TABLE drill(id INTEGER PRIMARY KEY, value TEXT NOT NULL); INSERT INTO drill(value) VALUES ('kept');"

backup="$("$root/scripts/backup-sqlite.sh" "$db" | awk -F= '/backup_path=/{print $2}')"
"$root/scripts/integrity-check.sh" "$backup"
rm -f "$db" "$db-wal" "$db-shm"
"$root/scripts/restore-sqlite.sh" "$backup" "$db"
"$root/scripts/integrity-check.sh" "$db"
if [[ "$(sqlite3 "$db" "SELECT value FROM drill WHERE id=1;")" != "kept" ]]; then
  echo "restored database did not preserve drill row" >&2
  exit 1
fi
printf 'backup_restore_drill=pass db=%s backup=%s\n' "$db" "$backup"
