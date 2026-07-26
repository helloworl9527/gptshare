#!/bin/sh
set -eu
root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd); tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT HUP INT TERM
mkdir -p "$tmp/db" "$tmp/backups" "$tmp/restore"; chmod 700 "$tmp/db" "$tmp/backups" "$tmp/restore"; printf 'drill-passphrase-not-runtime-key\n' > "$tmp/pass"; chmod 600 "$tmp/pass"
sqlite3 "$tmp/db/monitor.db" "CREATE TABLE schema_migrations(version INTEGER); INSERT INTO schema_migrations VALUES(4); CREATE TABLE marker(value TEXT); INSERT INTO marker VALUES('monitor');"
sqlite3 "$tmp/db/allocation.db" "CREATE TABLE schema_migrations(version INTEGER); INSERT INTO schema_migrations VALUES(5); CREATE TABLE marker(value TEXT); INSERT INTO marker VALUES('allocation');"
allocation_before=$(openssl dgst -sha256 "$tmp/db/allocation.db")
manifest=$("$root/scripts/vitals-backup.sh" "$tmp/db/monitor.db" "$tmp/db/allocation.db" "$tmp/backups" "$tmp/pass")
monitor_backup="$tmp/backups/$(sed -n 's/^monitor_file=//p' "$manifest")"; allocation_backup="$tmp/backups/$(sed -n 's/^allocation_file=//p' "$manifest")"
"$root/scripts/vitals-restore.sh" "$monitor_backup" "$tmp/restore/monitor.db" "$tmp/pass" monitor >/dev/null
test "$(sqlite3 "$tmp/restore/monitor.db" 'SELECT value FROM marker;')" = monitor
test "$allocation_before" = "$(openssl dgst -sha256 "$tmp/db/allocation.db")"
if "$root/scripts/vitals-restore.sh" "$monitor_backup" "$tmp/restore/wrong.db" "$tmp/pass" allocation >/dev/null 2>&1; then echo "cross-namespace restore unexpectedly succeeded" >&2; exit 1; fi
test "$(sqlite3 "$tmp/db/monitor.db" 'PRAGMA integrity_check;')" = ok; test "$(sqlite3 "$tmp/db/allocation.db" 'PRAGMA integrity_check;')" = ok
grep -Fq 'key_material_included=false' "$manifest"; test -f "$allocation_backup"
mkdir -p "$tmp/previous"; printf '{"monitor":4,"allocation":4}\n' > "$tmp/previous/schema-manifest.json"
set +e
"$root/scripts/vitals-rollback.sh" "$manifest" "$root/deploy/vitals/rollback-monitor.env.example" "$root/deploy/vitals/rollback-allocation.env.example" "$tmp/previous" >/dev/null 2>"$tmp/rollback.err"
rollback_code=$?
set -e
test "$rollback_code" = 42; grep -Fq 'restore_required' "$tmp/rollback.err"
printf '{"monitor":4,"allocation":5}\n' > "$tmp/previous/schema-manifest.json"
"$root/scripts/vitals-rollback.sh" "$manifest" "$root/deploy/vitals/rollback-monitor.env.example" "$root/deploy/vitals/rollback-allocation.env.example" "$tmp/previous" >/dev/null
printf '%s\n' 'vitals_ops_drill=pass monitor_restore=ok allocation_unchanged=yes integrity=ok'
