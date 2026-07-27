#!/bin/sh
set -eu
umask 027
root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd); output=${1:-"$root/artifacts/vitals"}; release="$output/release"
rm -rf "$release"; mkdir -p "$release/bin" "$release/migrations" "$release/deploy" "$release/scripts"
(cd "$root/web" && npm run build >/dev/null)
(cd "$root" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o "$release/bin/vitals" ./cmd/vitals && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o "$release/bin/vitals-migrate" ./cmd/vitals-migrate)
cp "$root"/migrations/*.sql "$release/migrations/"; cp -R "$root/deploy/vitals/." "$release/deploy/"
for name in vitals-preflight.sh vitals-backup.sh vitals-restore.sh vitals-rollback.sh; do cp "$root/scripts/$name" "$release/scripts/"; done
printf '{"monitor":5,"allocation":5,"databases":"separate","attach":false}\n' > "$release/schema-manifest.json"
chmod 0750 "$release/bin/"* "$release/scripts/"*.sh; (cd "$release" && find . -type f ! -name SHA256SUMS -print0 | sort -z | xargs -0 openssl dgst -sha256 > SHA256SUMS)
archive="$output/vitals-linux-amd64.tar.gz"; tar -C "$output" -czf "$archive" release; openssl dgst -sha256 "$archive" > "$archive.sha256"; printf '%s\n' "$archive"
