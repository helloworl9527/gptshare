#!/bin/sh
set -eu
if [ "$#" -ne 1 ]; then echo "usage: vitals-preflight.sh ENV_FILE" >&2; exit 64; fi
env_file=$1
test -f "$env_file" || { echo "environment file not found" >&2; exit 66; }
test "$(stat -c %a "$env_file" 2>/dev/null || stat -f %Lp "$env_file")" = 600 || { echo "environment file must be 0600" >&2; exit 77; }
if grep -Eq '__REPLACE_WITH|change-me|example-secret' "$env_file"; then echo "environment contains placeholder material" >&2; exit 78; fi
set -a
. "$env_file"
set +a
exec "$(dirname "$0")/../bin/vitals-migrate" --validate-only
