#!/bin/sh
set -eu
if [ "$#" -ne 3 ]; then echo "usage: rollback-release.sh PREVIOUS_RELEASE CURRENT_RELEASE CURRENT_LINK" >&2; exit 64; fi
previous=$1
current=$2
current_link=$3
test -f "$previous/schema-version" && test -f "$current/schema-version" || { echo "schema version metadata missing" >&2; exit 66; }
previous_schema=$(cat "$previous/schema-version")
current_schema=$(cat "$current/schema-version")
if [ "$previous_schema" != "$current_schema" ]; then
  echo "db_restore_required: schema $current_schema cannot automatically roll back to $previous_schema" >&2
  exit 42
fi
exec "$(dirname "$0")/atomic-switch.sh" "$previous" "$current_link"
