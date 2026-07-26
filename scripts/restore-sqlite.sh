#!/bin/sh
set -eu
umask 077

if [ "$#" -ne 3 ]; then
  echo "usage: restore-sqlite.sh ENCRYPTED_BACKUP TARGET_DB PASSPHRASE_FILE" >&2
  exit 64
fi
backup=$1
target=$2
pass_file=$3
test -f "$backup" && test -f "$pass_file" || { echo "backup or passphrase file not found" >&2; exit 66; }
test ! -e "$target" || { echo "target already exists; restore into an empty path" >&2; exit 73; }
target_dir=$(dirname "$target")
mkdir -p "$target_dir"
chmod 700 "$target_dir"
tmp="$target.restore.$$"
trap 'rm -f "$tmp"' EXIT HUP INT TERM
openssl enc -d -aes-256-cbc -pbkdf2 -in "$backup" -out "$tmp" -pass "file:$pass_file"
test "$(sqlite3 "$tmp" 'PRAGMA quick_check;')" = ok
chmod 600 "$tmp"
mv "$tmp" "$target"
printf '%s\n' "$target"
