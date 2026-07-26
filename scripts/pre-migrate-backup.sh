#!/bin/sh
set -eu
db_path=$1
backup_dir=$2
pass_file=$3
if [ ! -e "$db_path" ]; then
  exit 0
fi
exec "$(dirname "$0")/backup-sqlite.sh" "$db_path" "$backup_dir/pre-migrate" "$pass_file"
