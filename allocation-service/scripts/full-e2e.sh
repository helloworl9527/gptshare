#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

go test -count=1 ./internal/httpapi -run TestDeploymentEndToEndFlow -v
printf 'full_e2e=pass covers=admin_add_account,generate_card,redeem,query,totp,phase1_offline_degrade,replacement,export,warning\n'
