#!/bin/sh
set -eu
umask 077
if [ "$#" -ne 4 ]; then echo "usage: vitals-backup.sh MONITOR_DB ALLOCATION_DB BACKUP_DIR PASSPHRASE_FILE" >&2; exit 64; fi
monitor=$1; allocation=$2; out=$3; pass_file=$4
test "$monitor" != "$allocation" || { echo "database paths must be distinct" >&2; exit 65; }
test -f "$pass_file" || { echo "backup passphrase file not found" >&2; exit 66; }
test "$(stat -c %a "$pass_file" 2>/dev/null || stat -f %Lp "$pass_file")" = 600 || { echo "backup passphrase file must be 0600" >&2; exit 77; }
mkdir -p "$out"; chmod 700 "$out"; stamp=$(date -u +%Y%m%dT%H%M%SZ)
manifest="$out/vitals-$stamp.manifest"
: > "$manifest"; chmod 600 "$manifest"
for pair in "monitor:$monitor" "allocation:$allocation"; do
  name=${pair%%:*}; db=${pair#*:}; test -f "$db" || continue
  plain="$out/.$name-$stamp.sqlite"; encrypted="$out/$name-$stamp.sqlite.enc"
  trap 'rm -f "$plain"' EXIT HUP INT TERM
  sqlite3 "$db" "PRAGMA busy_timeout=5000; PRAGMA wal_checkpoint(FULL);" >/dev/null
  sqlite3 "$db" ".timeout 5000" ".backup '$plain'"
  test "$(sqlite3 "$plain" 'PRAGMA integrity_check;')" = ok
  openssl enc -aes-256-cbc -pbkdf2 -salt -in "$plain" -out "$encrypted" -pass "file:$pass_file"
  chmod 600 "$encrypted"; openssl dgst -sha256 "$encrypted" > "$encrypted.sha256"; chmod 600 "$encrypted.sha256"
  version=$(sqlite3 "$plain" 'SELECT coalesce(max(version),0) FROM schema_migrations;' 2>/dev/null || printf unknown)
  printf '%s_file=%s\n%s_schema=%s\n' "$name" "$(basename "$encrypted")" "$name" "$version" >> "$manifest"
  rm -f "$plain"; trap - EXIT HUP INT TERM
done
printf '%s\n' 'key_material_included=false' 'key_namespaces=CREDENTIAL_MASTER_KEYS,ALLOCATION_CREDENTIAL_MASTER_KEYS' >> "$manifest"
printf '%s\n' "$manifest"
