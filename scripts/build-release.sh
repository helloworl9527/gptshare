#!/bin/sh
set -eu
umask 027
root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
output=${1:-"$root/artifacts"}
release="$output/release"
rm -rf "$release"
mkdir -p "$release/bin" "$release/web" "$release/migrations" "$release/deploy" "$release/scripts"
(cd "$root" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o "$release/bin/chatgpt-monitor" ./cmd/server)
(cd "$root" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o "$release/bin/evidence-review" ./cmd/evidence-review)
(cd "$root/web" && npm run build >/dev/null)
cp -R "$root/web/dist/." "$release/web/"
cp "$root"/migrations/*.sql "$release/migrations/"
cp "$root/deploy/nginx.conf" "$root/deploy/proxy_params.conf" "$root/deploy/security_headers.conf" "$root/deploy/chatgpt-monitor.service" "$root/deploy/server.env.example" "$release/deploy/"
cp "$root/scripts/backup-sqlite.sh" "$root/scripts/restore-sqlite.sh" "$root/scripts/pre-migrate-backup.sh" "$root/scripts/atomic-switch.sh" "$root/scripts/rollback-release.sh" "$root/scripts/validate-deploy-static.sh" "$release/scripts/"
printf '4\n' > "$release/schema-version"
chmod 0750 "$release/bin/chatgpt-monitor" "$release/bin/evidence-review" "$release/scripts"/*.sh
find "$release" -type f ! -path '*/bin/*' ! -path '*/scripts/*' -exec chmod 0640 {} \;
(cd "$release" && find . -type f -print0 | sort -z | xargs -0 openssl dgst -sha256 > SHA256SUMS)
archive="$output/chatgpt-monitor-linux-amd64.tar.gz"
tar -C "$output" -czf "$archive" release
openssl dgst -sha256 "$archive" > "$archive.sha256"
printf '%s\n' "$archive"
