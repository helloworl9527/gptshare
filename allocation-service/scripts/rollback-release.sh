#!/usr/bin/env bash
set -euo pipefail

if [[ "$#" -ne 2 ]]; then
  printf 'usage: %s <previous-release-dir> <current-symlink>\n' "$0" >&2
  exit 64
fi

previous="$1"
current_link="$2"
db_path="${ALLOCATION_DB_PATH:-/var/lib/allocation-service/allocation.db}"

if [[ ! -d "$previous" || ! -f "$previous/bin/allocation-service" ]]; then
  printf 'previous release is invalid: %s\n' "$previous" >&2
  exit 65
fi
if [[ ! -L "$current_link" && -e "$current_link" ]]; then
  printf 'current path is not a symlink: %s\n' "$current_link" >&2
  exit 66
fi

previous_schema="$(cat "$previous/schema-version" 2>/dev/null || printf 'unknown')"
db_schema="unknown"
if [[ -f "$db_path" ]] && command -v sqlite3 >/dev/null 2>&1; then
  db_schema="$(sqlite3 "$db_path" 'SELECT coalesce(max(version),0) FROM schema_migrations;' 2>/dev/null || printf 'unknown')"
fi

if [[ "$previous_schema" != "unknown" && "$db_schema" != "unknown" && "$db_schema" -gt "$previous_schema" ]]; then
  printf 'db_restore_required: database schema %s is newer than previous release schema %s\n' "$db_schema" "$previous_schema" >&2
  exit 42
fi

ln -sfn "$previous" "$current_link"
printf 'rollback_symlink_updated=%s previous=%s db_schema=%s previous_schema=%s\n' "$current_link" "$previous" "$db_schema" "$previous_schema"
