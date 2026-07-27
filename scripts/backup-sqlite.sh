#!/bin/sh
set -eu
umask 077

if [ "$#" -ne 3 ]; then
  echo "usage: backup-sqlite.sh DB_PATH BACKUP_DIR PASSPHRASE_FILE" >&2
  exit 64
fi
db_path=$1
backup_dir=$2
pass_file=$3
test -f "$db_path" || { echo "database not found" >&2; exit 66; }
test -f "$pass_file" || { echo "backup passphrase file not found" >&2; exit 66; }
test "$(stat -c %a "$pass_file" 2>/dev/null || stat -f %Lp "$pass_file")" = 600 || { echo "backup passphrase file must be 0600" >&2; exit 77; }
mkdir -p "$backup_dir"
chmod 700 "$backup_dir"
stamp=$(date -u +%Y%m%dT%H%M%SZ)
plain="$backup_dir/.backup-$stamp.sqlite"
encrypted="$backup_dir/chatgpt-monitor-$stamp.sqlite.enc"
trap 'rm -f "$plain"' EXIT HUP INT TERM
sqlite3 "$db_path" "PRAGMA busy_timeout=5000; PRAGMA wal_checkpoint(FULL);" >/dev/null
sqlite3 "$db_path" ".timeout 5000" ".backup '$plain'"
test "$(sqlite3 "$plain" 'PRAGMA quick_check;')" = ok
chmod 600 "$plain"
openssl enc -aes-256-cbc -pbkdf2 -salt -in "$plain" -out "$encrypted" -pass "file:$pass_file"
chmod 600 "$encrypted"
openssl dgst -sha256 "$encrypted" > "$encrypted.sha256"
chmod 600 "$encrypted.sha256"
printf '%s\n' "$encrypted"
