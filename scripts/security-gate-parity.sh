#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
normal=$(mktemp)
fallback=$(mktemp)
cleanup() { rm -f "$normal" "$normal.normalized" "$fallback"; }
trap cleanup EXIT HUP INT TERM

"$root/allocation-service/scripts/security-gate-parity.sh"
SECURITY_GATE_SCANNER=grep "$root/scripts/security-gate.sh" >"$fallback"

if command -v rg >/dev/null 2>&1; then
	SECURITY_GATE_SCANNER=rg "$root/scripts/security-gate.sh" >"$normal"
	sed 's/scanner=rg/scanner=grep/' "$normal" >"$normal.normalized"
	if ! diff -u "$normal.normalized" "$fallback"; then
		echo "unified security gate rg/grep parity failed" >&2
		exit 1
	fi
	printf '%s\n' 'unified_security_gate_parity=pass scanners=rg,grep'
else
	SECURITY_GATE_SCANNER=auto "$root/scripts/security-gate.sh" >"$normal"
	if ! diff -u "$normal" "$fallback"; then
		echo "unified security gate grep fallback parity failed" >&2
		exit 1
	fi
	printf '%s\n' 'unified_security_gate_parity=pass scanner=grep-fallback rg=unavailable'
fi
