# Allocation Service Deployment

一期自动同步要求配置 `ALLOCATION_ACCOUNT_EVENT_API_KEY`，并在一期配置相同密钥及
`ALLOCATION_ACCOUNT_EVENT_URL=https://<allocation-host>/api/internal/v1/monitor-account-events`。
除回环测试地址外 webhook 必须使用 HTTPS。密钥至少 32 字节，必须独立生成，不得
复用管理员 session/CSRF、一期 monitor API 或凭据加密密钥。

升级后 allocation 数据库 schema 版本为 9。服务启动前先备份 SQLite 文件并运行
迁移；新账号将以 `pending_credentials` 创建，管理员补齐密码和 2FA 后才可分配。

This release is a local artifact only. Do not upload, publish, or switch production
traffic without separate user authorization.

## Topology

- Phase 1 monitor process listens on `127.0.0.1:8080`.
- Phase 2 allocation-service listens on `127.0.0.1:9090`.
- Nginx terminates local TLS on separate loopback ports:
  - `https://phase1.localhost:9443` -> `127.0.0.1:8080`
  - `https://allocation.localhost:9444` -> `127.0.0.1:9090`
- The allocation-service binary serves the user page, `/admin`, `/admin/*`, and
  same-origin `/api/admin/*`, `/api/redeem`, `/api/cards/*`.

## Required Environment Review

- DEPLOY-NOTE-01: phase 1 now requires `ALLOCATION_SERVICE_API_KEY` before startup.
  Upgrade existing phase 1 deployments by adding it to the phase 1 env template
  before enabling phase 2 monitor synchronization.
- DEPLOY-NOTE-02: runtime checks prove phase 2 rejects known phase 1 key names and
  known phase 1 materials, but cannot identify an unknown custom variable whose
  material was copied from phase 1. A human must review the random source and
  custody of `ALLOCATION_CREDENTIAL_MASTER_KEYS`.
- DEPLOY-NOTE-03: `scripts/security-gate.sh` uses `rg` when available and falls
  back to `grep`; CI/deployment hosts should install ripgrep for stable output.
  Node must satisfy the `@babel8` engine requirements used by the front-end toolchain.

## Install Outline

1. Create an `allocation` system user and private state directory:
   `install -d -o allocation -g allocation -m 0700 /var/lib/allocation-service`.
2. Copy the unpacked release to `/opt/allocation-service/releases/<version>`.
3. Copy `deploy/server.env.example` to `/etc/allocation-service/server.env`,
   replace every placeholder, and set permissions to `0600`.
4. Install `deploy/allocation-service.service` under `/etc/systemd/system/`.
5. Install the Nginx TLS routing sample after replacing certificate paths and
   server names for the local environment.
6. Run `systemctl daemon-reload`, `systemctl enable --now allocation-service`,
   `systemctl status allocation-service`, then `nginx -t && systemctl reload nginx`.

## Backup And Restore

- Backup: `scripts/backup-sqlite.sh /var/lib/allocation-service/allocation.db`.
- Integrity: `scripts/integrity-check.sh <backup-or-live-db>`.
- Restore: stop allocation-service, run
  `scripts/restore-sqlite.sh <backup-path> /var/lib/allocation-service/allocation.db`,
  then start the service and check `/health`.

## Rollback

Use `scripts/rollback-release.sh <previous-release-dir> <current-symlink>`.
The script compares embedded schema versions. If the installed database schema is
newer than the previous release schema it exits `42` with `db_restore_required`;
restore a compatible SQLite backup before switching the symlink.
