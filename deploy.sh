#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")" && pwd)
cd "$root"
umask 077

APP_HTTP_PORT=${APP_HTTP_PORT:-18081}
APP_HTTPS_PORT=${APP_HTTPS_PORT:-19443}
ADMIN_USER=${ADMIN_USER:-admin}

for command in docker openssl; do
    command -v "$command" >/dev/null 2>&1 || {
        echo "missing required command: $command" >&2
        exit 1
    }
done
docker compose version >/dev/null 2>&1 || {
    echo "Docker Compose plugin is required" >&2
    exit 1
}

echo "[1/5] Building application image"
docker build -t gptshare-vitals:local .

if [ ! -f .env ]; then
    echo "[2/5] Generating deployment credentials"
    bootstrap=$(docker run --rm --entrypoint vitals-password-hash \
        gptshare-vitals:local --bootstrap)
    value() {
        printf '%s\n' "$bootstrap" | sed -n "s/^$1=//p"
    }
    admin_password=$(value ADMIN_PASSWORD)
    admin_password_hash=$(value ADMIN_PASSWORD_HASH)
    monitor_key=$(value MONITOR_KEY)
    allocation_key=$(value ALLOCATION_KEY)
    jwt_key=$(value JWT_KEY)
    rate_limit_key=$(value RATE_LIMIT_KEY)
    totp_secret=$(value TOTP_SECRET)

    cat > .env <<EOF
APP_HTTP_PORT=$APP_HTTP_PORT
APP_HTTPS_PORT=$APP_HTTPS_PORT
VITALS_PORT=127.0.0.1:$APP_HTTP_PORT
APP_ORIGIN=https://127.0.0.1:$APP_HTTPS_PORT
VITALS_ALLOW_PUBLIC_APP_ORIGIN=false
TRUST_LOOPBACK_PROXY=true
MONITOR_DB_PATH=/data/monitor.db
MONITOR_MIGRATIONS_DIR=/app/migrations
ALLOCATION_DB_PATH=/data/allocation.db
CREDENTIAL_MASTER_KEYS=monitor-v1:$monitor_key
CREDENTIAL_ACTIVE_KEY_ID=monitor-v1
ALLOCATION_CREDENTIAL_MASTER_KEYS=allocation-v1:$allocation_key
ALLOCATION_CREDENTIAL_ACTIVE_KEY_ID=allocation-v1
ADMIN_USER=$ADMIN_USER
ADMIN_PASSWORD_HASH='$admin_password_hash'
ADMIN_TOTP_SECRET=$totp_secret
JWT_SIGNING_KEY=$jwt_key
RATE_LIMIT_KEY=$rate_limit_key
VITALS_MONITOR_COMPAT_HTTP_ENABLED=false
EOF
    chmod 0600 .env

    cat > deploy-credentials.txt <<EOF
Admin URL: https://127.0.0.1:$APP_HTTPS_PORT/admin/
Username: $ADMIN_USER
Password: $admin_password
TOTP secret: $totp_secret
EOF
    chmod 0600 deploy-credentials.txt
else
    echo "[2/5] Keeping existing .env credentials"
fi

set -a
. ./.env
set +a

echo "[3/5] Preparing persistent data and local TLS"
mkdir -p data certs
chmod 0700 data certs
if [ ! -s certs/server.key ] || [ ! -s certs/server.crt ]; then
    openssl req -x509 -newkey rsa:2048 -sha256 -nodes -days 365 \
        -subj /CN=127.0.0.1 \
        -addext subjectAltName=IP:127.0.0.1 \
        -keyout certs/server.key \
        -out certs/server.crt >/dev/null 2>&1
    chmod 0600 certs/server.key
    chmod 0644 certs/server.crt
fi

echo "[4/5] Starting containers"
docker compose config -q
docker compose up -d

echo "[5/5] Waiting for health checks"
attempt=0
while [ "$attempt" -lt 30 ]; do
    app_id=$(docker compose ps -q vitals)
    proxy_id=$(docker compose ps -q proxy)
    app_health=$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}missing{{end}}' "$app_id" 2>/dev/null || true)
    proxy_health=$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}missing{{end}}' "$proxy_id" 2>/dev/null || true)
    if [ "$app_health" = healthy ] && [ "$proxy_health" = healthy ]; then
        docker compose ps
        echo
        echo "Deployment complete: https://127.0.0.1:$APP_HTTPS_PORT/"
        if [ -f deploy-credentials.txt ]; then
            echo "Initial credentials: $root/deploy-credentials.txt"
        fi
        exit 0
    fi
    attempt=$((attempt + 1))
    sleep 2
done

docker compose ps >&2
docker compose logs --no-color --tail=100 >&2
echo "deployment health check timed out" >&2
exit 1
