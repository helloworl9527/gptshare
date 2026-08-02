#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
release="$root/.workflow/artifacts/mstep05/release"
archive="$root/.workflow/artifacts/mstep05/vitals-linux-amd64.tar.gz"
pattern='(console\.(log|debug)|sessionStorage|unsafe-inline|unsafe-eval|ATTACH[[:space:]]+DATABASE)'

requested_scanner=${SECURITY_GATE_SCANNER:-auto}
case "$requested_scanner" in
	auto)
		if command -v rg >/dev/null 2>&1; then
			scanner=rg
		elif command -v grep >/dev/null 2>&1; then
			scanner=grep
		else
			echo "security gate requires rg or grep" >&2
			exit 1
		fi
		;;
	rg|grep)
		if ! command -v "$requested_scanner" >/dev/null 2>&1; then
			echo "$requested_scanner required by SECURITY_GATE_SCANNER" >&2
			exit 1
		fi
		scanner=$requested_scanner
		;;
	*)
		echo "unsupported SECURITY_GATE_SCANNER: $requested_scanner" >&2
		exit 1
		;;
esac

scan_source() {
	if [ "$scanner" = rg ]; then
		rg -n -i --glob '!**/*test*' --glob '!**/dev-run.sh' --glob '!**/security-gate*.sh' --glob '!**/validate-deploy-static.sh' --glob '!**/vitals-deploy-contract.sh' --glob '!**/*.md' --glob '!**/node_modules/**' --glob '!**/artifacts/**' "$pattern" "$root/cmd" "$root/internal" "$root/scripts" "$root/deploy/vitals" "$root/web/src" "$root/web/public"
	else
		grep -RInE --exclude='*test*' --exclude='dev-run.sh' --exclude='security-gate*.sh' --exclude='validate-deploy-static.sh' --exclude='vitals-deploy-contract.sh' --exclude='*.md' --exclude-dir='test' --exclude-dir='tests' --exclude-dir='node_modules' --exclude-dir='artifacts' "$pattern" "$root/cmd" "$root/internal" "$root/scripts" "$root/deploy/vitals" "$root/web/src" "$root/web/public"
	fi
}

scan_release() {
	if [ "$scanner" = rg ]; then
		rg -n -i --glob '!**/*.md' "$pattern" "$release/deploy" "$release/scripts" "$release/migrations" "$release/schema-manifest.json"
	else
		grep -RInE --exclude='*.md' "$pattern" "$release/deploy" "$release/scripts" "$release/migrations" "$release/schema-manifest.json"
	fi
}

scan_file() {
	if [ "$scanner" = rg ]; then
		rg -n -i "$pattern" "$1"
	else
		grep -nEi "$pattern" "$1"
	fi
}

assert_clean() {
	label=$1
	shift
	if "$@"; then
		echo "$label contains an unauthorized sink" >&2
		exit 1
	else
		status=$?
	fi
	if [ "$status" -ne 1 ]; then
		echo "$label scan failed with status $status using $scanner" >&2
		exit 1
	fi
}

assert_authorized_browser_storage() {
	source="$root/web/public/static/user.js"
	embedded="$root/internal/unifiedui/static/static/user.js"
	for file in "$source" "$embedded"; do
		count=$(grep -Ec 'localStorage\.(getItem|removeItem|setItem)\(savedCardsStorageKey' "$file" || true)
		if [ "$count" -ne 3 ]; then
			echo "authorized saved-card storage contract changed: $file" >&2
			exit 1
		fi
		if grep -En '(localStorage|sessionStorage)' "$file" | grep -Ev 'localStorage\.(getItem|removeItem|setItem)\(savedCardsStorageKey'; then
			echo "authorized saved-card script contains an unexpected storage sink: $file" >&2
			exit 1
		fi
	done
	if ! cmp -s "$source" "$embedded"; then
		echo "embedded saved-card script differs from reviewed source" >&2
		exit 1
	fi
	if [ "$scanner" = rg ]; then
		if output=$(rg -n -i --glob '!**/*test*' --glob '!**/security-gate*.sh' --glob '!**/node_modules/**' '(localStorage|sessionStorage)' \
			"$root/cmd" "$root/internal" "$root/scripts" "$root/deploy/vitals" "$root/web/src" "$root/web/public"); then
			status=0
		else
			status=$?
		fi
	else
		if output=$(grep -RInE --exclude='*test*' --exclude='security-gate*.sh' --exclude-dir='test' --exclude-dir='tests' --exclude-dir='node_modules' '(localStorage|sessionStorage)' \
			"$root/cmd" "$root/internal" "$root/scripts" "$root/deploy/vitals" "$root/web/src" "$root/web/public"); then
			status=0
		else
			status=$?
		fi
	fi
	if [ "$status" -ne 0 ] && [ "$status" -ne 1 ]; then
		echo "browser storage scan failed with status $status using $scanner" >&2
		exit 1
	fi
	filtered=$(printf '%s\n' "$output" | grep -Fv "$source:" | grep -Fv "$embedded:" || true)
	if [ -n "$filtered" ]; then
		printf '%s\n' "$filtered" >&2
		echo "source contains an unauthorized browser storage sink" >&2
		exit 1
	fi
}

"$root/allocation-service/scripts/security-gate.sh"
assert_authorized_browser_storage
assert_clean "unified source" scan_source

grep -Fq 'VITALS_MONITOR_COMPAT_HTTP_ENABLED=false' "$root/deploy/vitals/server.env.example"
if grep -q '^ALLOCATION_SERVICE_API_KEY=' "$root/deploy/vitals/server.env.example"; then
	echo "default unified env must not require compatibility API key" >&2
	exit 1
fi
grep -Fq 'ALLOCATION_SERVICE_API_KEY=' "$root/deploy/vitals/rollback-monitor.env.example"
grep -Fq 'ALLOCATION_MONITOR_API_KEY=' "$root/deploy/vitals/rollback-allocation.env.example"

if [ ! -d "$release" ] || [ ! -f "$archive" ] || [ ! -f "$archive.sha256" ]; then
	echo "release security scan requires the built MSTEP-05 artifact" >&2
	exit 1
fi
assert_clean "release artifact" scan_release

binary_strings=$(mktemp)
trap 'rm -f "$binary_strings"' EXIT HUP INT TERM
for binary in "$release/bin/vitals" "$release/bin/vitals-migrate"; do
	if ! strings "$binary" >"$binary_strings"; then
		echo "release binary strings scan failed: $binary" >&2
		exit 1
	fi
	assert_clean "release binary $binary" scan_file "$binary_strings"
done

if [ "$(openssl dgst -sha256 "$archive" | awk '{print $NF}')" != "$(awk '{print $NF}' "$archive.sha256")" ]; then
	echo "release archive checksum mismatch" >&2
	exit 1
fi
printf 'unified_security_gate=pass unauthorized_sink=0 scanner=%s\n' "$scanner"
