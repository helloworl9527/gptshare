#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export LC_ALL=C
export LANG=C
version="${1:-$(date -u +%Y%m%dT%H%M%SZ)}"
release_name="allocation-service-${version}"
artifacts_dir="$root/artifacts"
release_dir="$artifacts_dir/$release_name"
archive="$artifacts_dir/${release_name}.tar.gz"

rm -rf "$release_dir" "$archive" "$archive.sha256"
mkdir -p "$release_dir/bin" "$release_dir/deploy" "$release_dir/scripts"

(
  cd "$root/web"
  npm ci
  npm run build
)
rm -rf "$root/web/node_modules"

(
  cd "$root"
  go test -count=1 ./...
  go vet ./...
  go build -trimpath -ldflags='-s -w' -o "$release_dir/bin/allocation-service" ./cmd/allocation-service
)

cp "$root/.env.example" "$release_dir/.env.example"
cp "$root/deploy/"*.conf "$release_dir/deploy/"
cp "$root/deploy/"*.service "$release_dir/deploy/"
cp "$root/deploy/"*.example "$release_dir/deploy/"
cp "$root/deploy/README-deploy.md" "$release_dir/deploy/"
cp "$root/scripts/"*.sh "$release_dir/scripts/"
chmod 0755 "$release_dir/bin/allocation-service" "$release_dir/scripts/"*.sh

schema_version="$(cd "$root" && go test -count=1 ./internal/store -run TestPrintLatestSchemaVersionForRelease -v 2>/dev/null | awk -F= '/schema_version=/{print $2}' | tail -1)"
if [[ -z "$schema_version" ]]; then
  schema_version="unknown"
fi
printf '%s\n' "$schema_version" > "$release_dir/schema-version"

(
  cd "$release_dir"
  find . -type f -not -name SHA256SUMS -print0 | sort -z | xargs -0 shasum -a 256 > SHA256SUMS
)
(
  cd "$artifacts_dir"
  tar -czf "$archive" "$release_name"
  shasum -a 256 "$(basename "$archive")" > "$(basename "$archive").sha256"
)

printf 'release_archive=%s\n' "$archive"
printf 'release_checksum_file=%s\n' "$archive.sha256"
cat "$archive.sha256"
