#!/usr/bin/env bash
set -euo pipefail

root="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"

normal="$(mktemp)"
fallback="$(mktemp)"
cleanup() {
  rm -f "$normal" "$fallback"
}
trap cleanup EXIT

"$root/scripts/security-gate.sh" "$root" >"$normal"
PATH="/bin:/usr/bin:/usr/sbin:/sbin" "$root/scripts/security-gate.sh" "$root" >"$fallback"

if ! diff -u "$normal" "$fallback"; then
  printf 'security gate parity failed\n' >&2
  exit 1
fi

printf 'security gate parity passed\n'
