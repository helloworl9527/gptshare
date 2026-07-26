#!/bin/sh
set -eu
if [ "$#" -ne 4 ]; then echo "usage: vitals-rollback.sh MANIFEST ROLLBACK_MONITOR_ENV ROLLBACK_ALLOCATION_ENV PREVIOUS_RELEASE" >&2; exit 64; fi
manifest=$1; monitor_env=$2; allocation_env=$3; previous=$4
for file in "$manifest" "$monitor_env" "$allocation_env" "$previous/schema-manifest.json"; do test -f "$file" || { echo "rollback artifact missing" >&2; exit 66; }; done
grep -Fq 'ALLOCATION_SERVICE_API_KEY=' "$monitor_env" || { echo "rollback monitor API key missing" >&2; exit 78; }
grep -Fq 'ALLOCATION_MONITOR_API_KEY=' "$allocation_env" || { echo "rollback allocation client key missing" >&2; exit 78; }
monitor_current=$(sed -n 's/^monitor_schema=//p' "$manifest"); allocation_current=$(sed -n 's/^allocation_schema=//p' "$manifest")
monitor_previous=$(sed -n 's/.*"monitor"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p' "$previous/schema-manifest.json")
allocation_previous=$(sed -n 's/.*"allocation"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p' "$previous/schema-manifest.json")
if [ "$monitor_current" != "$monitor_previous" ] || [ "$allocation_current" != "$allocation_previous" ]; then echo "restore_required: schema mismatch; restore both compatible backups before double-service start" >&2; exit 42; fi
printf '%s\n' 'rollback_ready: stop vitals; restore legacy systemd/nginx; load both rollback env files; start monitor then allocation'
