#!/usr/bin/env bash
set -euo pipefail

root="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
cd "$root"

failures=0

grep_excludes_for_glob() {
  case "$1" in
    '!**/*_test.go') printf '%s\n' "--exclude=*_test.go" ;;
    '!web/src/**/*.test.js') printf '%s\n' "--exclude=*.test.js" ;;
    '!web/src/test/**') printf '%s\n' "--exclude-dir=test" ;;
    '!web/tests/**') printf '%s\n' "--exclude-dir=tests" ;;
    '!scripts/security-gate.sh') printf '%s\n' "--exclude=security-gate.sh" ;;
    '!scripts/config-failure-matrix.sh') printf '%s\n' "--exclude=config-failure-matrix.sh" ;;
    '!scripts/dev-run.sh') printf '%s\n' "--exclude=dev-run.sh" ;;
    '!deploy/**') printf '%s\n' "--exclude-dir=deploy" ;;
    '!artifacts/**') printf '%s\n' "--exclude-dir=artifacts" ;;
    '!go.sum') printf '%s\n' "--exclude=go.sum" ;;
    '!web/package-lock.json') printf '%s\n' "--exclude=package-lock.json" ;;
    '!web/node_modules/**') printf '%s\n' "--exclude-dir=node_modules" ;;
    '!web/test-results/**') printf '%s\n' "--exclude-dir=test-results" ;;
    '!web/playwright-report/**') printf '%s\n' "--exclude-dir=playwright-report" ;;
    '!internal/config/config.go') printf '%s\n' "--exclude=config.go" ;;
    '!internal/config/config_test.go') printf '%s\n' "--exclude=config_test.go" ;;
    *)
      printf 'unsupported grep fallback glob: %s\n' "$1" >&2
      return 2
      ;;
  esac
}

search_no_match() {
  local pattern="$1"
  shift
  if command -v rg >/dev/null 2>&1; then
    rg -n "$pattern" "$@"
    return $?
  fi

  local grep_args=()
  local paths=()
  local glob
  while [[ "$#" -gt 0 ]]; do
    case "$1" in
      --glob)
        glob="$2"
        shift 2
        while IFS= read -r translated; do
          grep_args+=("$translated")
        done < <(grep_excludes_for_glob "$glob")
        continue
        ;;
      --glob=*)
        glob="${1#--glob=}"
        shift
        while IFS= read -r translated; do
          grep_args+=("$translated")
        done < <(grep_excludes_for_glob "$glob")
        continue
        ;;
      *)
        paths+=("$1")
        shift
        ;;
    esac
  done
  grep -RInE "${grep_args[@]}" "$pattern" "${paths[@]}"
}

report_failure() {
  local title="$1"
  local output="$2"
  failures=$((failures + 1))
  printf 'FAIL %s\n%s\n' "$title" "$output" >&2
}

report_pass() {
  printf 'PASS %s\n' "$1"
}

scan_product_forbidden_values() {
  local pattern='LEAK_PASSWORD_SENTINEL|LEAK_TOTP_SENTINEL|monitor-token-sentinel|offline-monitor-token|secret-password|secret-totp|query-password-sentinel|query-totp-sentinel|2345-6789-ABCD|EFGH-JKMN-PQRS|JKMN-PQRS-TUVW|controlled-test-password|test-csrf-token-with-sufficient-entropy|monitor-api-key-sentinel'
  local output
  set +e
  output="$(search_no_match "$pattern" . \
    --glob '!**/*_test.go' \
    --glob '!web/src/**/*.test.js' \
    --glob '!web/src/test/**' \
    --glob '!web/tests/**' \
    --glob '!scripts/security-gate.sh' \
    --glob '!scripts/config-failure-matrix.sh' \
    --glob '!artifacts/**' \
    --glob '!go.sum' \
    --glob '!web/package-lock.json' \
    --glob '!web/node_modules/**' \
    --glob '!web/test-results/**' \
    --glob '!web/playwright-report/**' 2>&1)"
  local status=$?
  set -e
  if [[ "$status" -eq 0 ]]; then
    report_failure "product-secret-sentinel-scan" "$output"
  elif [[ "$status" -eq 1 ]]; then
    report_pass "product-secret-sentinel-scan"
  else
    report_failure "product-secret-sentinel-scan-error" "$output"
  fi
}

scan_browser_forbidden_sinks() {
  local output
  set +e
  output="$(search_no_match 'localStorage|sessionStorage|console\.(log|debug|info|warn|error)' web/src internal/httpapi/static/user.html \
    --glob '!web/src/**/*.test.js' \
    --glob '!web/src/test/**' 2>&1)"
  local status=$?
  set -e
  if [[ "$status" -eq 0 ]]; then
    report_failure "browser-secret-sink-scan" "$output"
  elif [[ "$status" -eq 1 ]]; then
    report_pass "browser-secret-sink-scan"
  else
    report_failure "browser-secret-sink-scan-error" "$output"
  fi
}

scan_url_secret_sinks() {
  local output
  set +e
  output="$(search_no_match '(password|totp|2fa|secret|token|credential|card_code|code).{0,80}(URLSearchParams|(^|[^[:alnum:]_])location\.|history\.pushState|router\.push|router\.replace)|((URLSearchParams|(^|[^[:alnum:]_])location\.|history\.pushState|router\.push|router\.replace).{0,80}(password|totp|2fa|secret|token|credential|card_code|code))' web/src internal/httpapi/static/user.html \
    --glob '!web/src/**/*.test.js' \
    --glob '!web/src/test/**' 2>&1)"
  local status=$?
  set -e
  if [[ "$status" -eq 0 ]]; then
    report_failure "url-secret-sink-scan" "$output"
  elif [[ "$status" -eq 1 ]]; then
    report_pass "url-secret-sink-scan"
  else
    report_failure "url-secret-sink-scan-error" "$output"
  fi
}

scan_pii_in_logs_and_errors() {
  local output
  set +e
  output="$(search_no_match '[[:alnum:]._%+-]+@[[:alnum:].-]+\.[[:alpha:]]{2,}' internal cmd \
    --glob '!**/*_test.go' 2>&1)"
  local status=$?
  set -e
  if [[ "$status" -eq 0 ]]; then
    report_failure "pii-email-product-log-error-scan" "$output"
  elif [[ "$status" -eq 1 ]]; then
    report_pass "pii-email-product-log-error-scan"
  else
    report_failure "pii-email-product-log-error-scan-error" "$output"
  fi
}

scan_screenshot_strings() {
  local screenshots_dir="$root/../.workflow/screenshots"
  if [[ ! -d "$screenshots_dir" ]]; then
    report_pass "screenshot-secret-strings-scan-skipped-no-directory"
    return
  fi
  local output
  set +e
  if command -v rg >/dev/null 2>&1; then
    output="$(find "$screenshots_dir" -type f -name '*.png' -print0 | xargs -0 strings 2>/dev/null | rg -n 'LEAK_PASSWORD_SENTINEL|LEAK_TOTP_SENTINEL|secret-password|secret-totp|monitor-token-sentinel|offline-monitor-token|2345-6789-ABCD|EFGH-JKMN-PQRS|JKMN-PQRS-TUVW|controlled-test-password|test-csrf-token-with-sufficient-entropy' 2>&1)"
  else
    output="$(find "$screenshots_dir" -type f -name '*.png' -print0 | xargs -0 strings 2>/dev/null | grep -nE 'LEAK_PASSWORD_SENTINEL|LEAK_TOTP_SENTINEL|secret-password|secret-totp|monitor-token-sentinel|offline-monitor-token|2345-6789-ABCD|EFGH-JKMN-PQRS|JKMN-PQRS-TUVW|controlled-test-password|test-csrf-token-with-sufficient-entropy' 2>&1)"
  fi
  local status=$?
  set -e
  if [[ "$status" -eq 0 ]]; then
    report_failure "screenshot-secret-strings-scan" "$output"
  elif [[ "$status" -eq 1 || "$status" -eq 123 ]]; then
    report_pass "screenshot-secret-strings-scan"
  else
    report_failure "screenshot-secret-strings-scan-error" "$output"
  fi
}

scan_phase_one_secret_reuse() {
  local output
  set +e
  output="$(search_no_match '(^|[^_A-Z])(CREDENTIAL_MASTER_KEYS|CREDENTIAL_ACTIVE_KEY_ID|JWT_SIGNING_KEY|RATE_LIMIT_KEY|ADMIN_TOTP_SECRET|ALLOCATION_SERVICE_API_KEY)' . \
    --glob '!internal/config/config.go' \
    --glob '!internal/config/config_test.go' \
    --glob '!scripts/config-failure-matrix.sh' \
    --glob '!scripts/security-gate.sh' \
    --glob '!scripts/dev-run.sh' \
    --glob '!deploy/**' \
    --glob '!artifacts/**' \
    --glob '!web/node_modules/**' \
    --glob '!web/package-lock.json' \
    --glob '!go.sum' 2>&1)"
  local status=$?
  set -e
  if [[ "$status" -eq 0 ]]; then
    report_failure "phase-one-secret-reference-scan" "$output"
  elif [[ "$status" -eq 1 ]]; then
    report_pass "phase-one-secret-reference-scan"
  else
    report_failure "phase-one-secret-reference-scan-error" "$output"
  fi
}

scan_product_forbidden_values
scan_browser_forbidden_sinks
scan_url_secret_sinks
scan_pii_in_logs_and_errors
scan_screenshot_strings
scan_phase_one_secret_reuse

if [[ "$failures" -ne 0 ]]; then
  printf 'security gate failed: %d unauthorized sink(s)\n' "$failures" >&2
  exit 1
fi

printf 'security gate passed: 0 unauthorized sink(s)\n'
