#!/bin/sh
set -eu
umask 077
if [ "$#" -ne 4 ]; then echo "usage: vitals-restore.sh ENCRYPTED_BACKUP TARGET_DB PASSPHRASE_FILE EXPECTED_MODULE" >&2; exit 64; fi
backup=$1; target=$2; pass_file=$3; module=$4
case "$module" in monitor|allocation) ;; *) echo "module must be monitor or allocation" >&2; exit 64;; esac
test -f "$backup" && test -f "$pass_file" || { echo "backup or passphrase missing" >&2; exit 66; }
test ! -e "$target" || { echo "target already exists; restore refuses overwrite" >&2; exit 73; }
case "$(basename "$backup")" in "$module"-*.sqlite.enc) ;; *) echo "backup namespace does not match target module" >&2; exit 65;; esac
mkdir -p "$(dirname "$target")"; chmod 700 "$(dirname "$target")"; tmp="$target.restore.$$"; trap 'rm -f "$tmp"' EXIT HUP INT TERM
openssl enc -d -aes-256-cbc -pbkdf2 -in "$backup" -out "$tmp" -pass "file:$pass_file"
test "$(sqlite3 "$tmp" 'PRAGMA integrity_check;')" = ok; chmod 600 "$tmp"; mv "$tmp" "$target"
printf '%s\n' "$target"
