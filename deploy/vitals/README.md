# Vitals Unified Deployment And Rollback

Local artifacts only. Production upload, remote changes, traffic switching, key rotation, and deployment require separate user authorization.

## Startup Order

The single `vitals.service` runs one binary, process, and loopback port. Its enforced order is: render and validate the 0600 environment file; create separate encrypted backups for `monitor.db` and `allocation.db`; run `vitals-migrate`; start `vitals`. Configuration validation rejects missing, weak, example, or reused monitor/allocation/auth key material before either migration opens a database.

Each database keeps its own `schema_migrations` ledger. Monitor migrations use only `MONITOR_DB_PATH`; allocation migrations use only `ALLOCATION_DB_PATH`. SQL ATTACH, cross-database joins, and cross-database transactions are prohibited. A migration failure stops startup; restore the matching pre-migration backup. If monitor committed before allocation failed, restore monitor too when an all-or-nothing release rollback is required; this is operational rollback, not a fake cross-database transaction.

## Backup And Restore

`vitals-backup.sh MONITOR_DB ALLOCATION_DB BACKUP_DIR PASSPHRASE_FILE` checkpoints and backs up each database independently, runs `integrity_check`, encrypts each artifact, and writes a manifest with independent schema versions. The release and backup contain key namespace names only, never key material. Preserve the separately managed 0600 runtime env and backup passphrase through the approved secret manager.

Restore one module into an empty path with `vitals-restore.sh BACKUP TARGET PASS_FILE monitor|allocation`. Namespace checks reject a monitor backup for an allocation target and vice versa. The script never overwrites an existing database, so restoring one damaged database cannot overwrite the other. After restore, run `sqlite3 DB 'PRAGMA integrity_check;'` and verify the manifest schema before changing the configured path.

## Monolith To Double-Service Rollback

1. Stop `vitals.service` and keep Nginx traffic stopped.
2. Compare both current schema versions with the previous release using `vitals-rollback.sh`. Exit 42 `restore_required` means restore compatible backups before starting old code.
3. Restore the previous monitor and allocation releases, their two systemd units, and the two-site Nginx configuration.
4. Render both rollback env templates. The phase-one template must contain `ALLOCATION_SERVICE_API_KEY`; phase two must contain the matching `ALLOCATION_MONITOR_API_KEY` and monitor URL. Restore the original independent admin/session values and both original data keyrings.
5. Start monitor first, then allocation. Verify both health endpoints, authenticated admin paths, public card query, and the phase-two-to-phase-one compatibility call.

Default unified operation keeps `VITALS_MONITOR_COMPAT_HTTP_ENABLED=false`, does not require `ALLOCATION_SERVICE_API_KEY`, and returns 404 for `/api/v1/monitor/*`. Temporary compatibility enablement requires a named consumer, future expiry, shutdown task, strong API key, rate limit, and audit.

## Nginx And Limits

The sample terminates TLS 1.2/1.3 on one loopback site, proxies every route to one loopback port, sets HSTS and strict CSP, and contains no `unsafe-inline`. The service limit is 2 CPUs and 2.5 GiB. Production hosts must run `systemd-analyze verify`, `nginx -t`, TLS/header probes, and a workload resource drill; macOS cannot provide those Linux runtime proofs.

`scripts/security-gate.sh` and `scripts/security-gate-parity.sh` are build-time source-tree gates and must pass before packaging. They intentionally are not shipped as runtime release tools because their scan scope includes both complete Go modules and frontend sources, which are not part of the binary release archive.
