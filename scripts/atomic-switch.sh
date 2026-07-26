#!/bin/sh
set -eu
if [ "$#" -ne 2 ]; then echo "usage: atomic-switch.sh RELEASE_DIR CURRENT_LINK" >&2; exit 64; fi
release_dir=$1
current_link=$2
test -d "$release_dir" || { echo "release directory missing" >&2; exit 66; }
parent=$(dirname "$current_link")
mkdir -p "$parent"
temp_link="$current_link.next.$$"
trap 'rm -f "$temp_link"' EXIT HUP INT TERM
ln -s "$release_dir" "$temp_link"
if ! mv -Tf "$temp_link" "$current_link" 2>/dev/null; then
  mv -fh "$temp_link" "$current_link"
fi
readlink "$current_link"
