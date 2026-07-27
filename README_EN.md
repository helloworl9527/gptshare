# ChatGPT Account Monitoring + Automatic Shared Account Allocation

[简体中文](./README.md) | [English](./README_EN.md)

This project combines **ChatGPT account monitoring** and **automatic shared account allocation** in one operations system. It is a modular monolith written in Go: one process, one port, and one administrator login provide the monitoring console, shared account pool, redemption-code workflow, and public account lookup page.

## Core Features

### ChatGPT Account Monitoring

- Import accounts through a manual Codex CLI OAuth callback, device authorization, a single token, or a batch of tokens.
- Store account credentials encrypted and check account availability, errors, and bans concurrently on a schedule.
- Track authorization epochs, initial anomaly evidence, and time-to-ban to reduce false positives caused by temporary upstream failures.
- Provide an account overview, status details, reauthorization, monitoring settings, and an alert outbox.
- Protect the administrator console with password + TOTP authentication, server-side sessions, CSRF validation, and login rate limiting.

Monitoring results are exposed to the allocation module through a read-only in-process facade. No extra internal HTTP service is required.

### Automatic Shared Account Allocation

- Generate, copy, export, extend, and revoke redemption codes in batches.
- When a user redeems a code on the public page, automatically choose a shared account and display its username, password, and locally generated TOTP code.
- Save successfully redeemed codes in the user's current browser for quick future lookup.
- Configure an independent concurrency capacity for each shared account; full accounts are excluded from allocation.
- Calculate total capacity, used capacity, recent redemption rate, and four inventory levels: safe, notice, urgent, and exhausted.

#### Allocation Logic

On the first redemption of a code, one database transaction validates the code, selects a candidate account, reserves capacity, creates the allocation, and updates the code state:

1. Exclude expired, full, unavailable, or otherwise ineligible accounts.
2. When monitoring is available, exclude banned accounts and prioritize accounts in the `alive` state.
3. Compare the account expiration with the redemption-code expiration and minimize unused subscription time.
4. When time-based scores are equal, prefer the account with fewer current allocations, then the one used least recently.
5. Use conditional transactional updates and uniqueness constraints to prevent capacity overselling and duplicate allocations during concurrent redemptions.

After allocation, background jobs continue evaluating active accounts against monitoring results:

- **Banned account:** allocate a replacement immediately and release the old account's capacity.
- **Account nearing expiration:** allocate a replacement in advance and keep the old account available for a 24-hour grace period.
- **No spare capacity:** preserve the current state, write a failed audit event, and retry in a later job.
- **Repeated lookup of the same code:** return the existing valid allocation without consuming capacity again.

## System Boundaries

| Unified capability | Required isolation |
|---|---|
| One Go binary, process, and port | Two independent SQLite databases: `monitor.db` and `allocation.db` |
| One administrator login and frontend | Independent key material for monitoring data, allocation data, and administrator authentication |
| Read-only monitoring state for allocation decisions | No SQL `ATTACH`, cross-database joins, or cross-database transactions |
| Supervised background jobs | A background job failure must not take down the public redemption page |

## Technology Stack

- **Backend:** Go 1.25, Gin, JWT, TOTP, and AES-GCM.
- **Database:** SQLite with the pure-Go `modernc.org` driver (no CGO); separate databases for monitoring and allocation.
- **Frontend:** Vue 3, Vite, and Element Plus; production assets are embedded in the Go binary.
- **Deployment:** Multi-stage Docker build, Docker Compose, and an Nginx TLS reverse proxy.
- **Security:** Encrypted credentials, strict CSP, same-origin and CSRF validation, administrator login rate limiting, and security auditing.

Service endpoints:

- `/admin/` — unified operations console.
- `/` — public redemption and account lookup page.
- `/health` — application and database health check.

## Repository Layout

```text
cmd/vitals/             Main application entry point; assembles auth, monitoring, allocation, UI, and background jobs
cmd/vitals-migrate/     Pre-start database migration runner
internal/               Monitoring domain, unified auth/config/API, facade, and embedded frontend
allocation-service/     Account pool, redemption codes, automatic allocation, replacement jobs, and inventory warnings
web/                    Vue 3 administrator and public frontend source
internal/unifiedui/     Production frontend assets embedded by Go
deploy/vitals/          Environment template, Nginx, systemd, and operations documentation
scripts/                Test, security scan, backup, restore, and rollback scripts
docs/                   OpenAPI, security boundaries, risk register, and feature documentation
test/e2e/               End-to-end tests against the real application binary
agent.md                Complete repository and deployment guide for AI agents
```

## Docker Deployment

> If an AI agent will perform the deployment, it must read **[agent.md](./agent.md)** in full before taking action. The agent must verify database, key, backup, and production-authorization boundaries instead of guessing from this README or skipping pre-deployment checks.

The server must have Docker, the Docker Compose plugin, and OpenSSL installed:

```bash
git clone https://github.com/helloworl9527/gptshare.git
cd gptshare

# Agent: read the complete deployment guide first
cat agent.md

# Build the image, generate first-run configuration, and start the service
sudo ./deploy.sh
```

`deploy.sh` performs the following steps:

1. Builds the frontend and Go binaries through a multi-stage Dockerfile.
2. On the first deployment, generates independent database-encryption, session, rate-limit, and TOTP keys.
3. Creates a local TLS certificate and starts the `vitals` and `proxy` containers.
4. Waits for both application and reverse-proxy health checks to pass.

Default addresses and files:

- HTTPS: `https://127.0.0.1:19443/`
- Administrator console: `https://127.0.0.1:19443/admin/`
- Initial administrator credentials: `deploy-credentials.txt` with mode `0600`
- Runtime configuration: `.env` with mode `0600`
- Persistent databases: `data/`

Running `sudo ./deploy.sh` again preserves the existing `.env`, certificates, and databases. For a public deployment, keep the application bound to loopback, terminate TLS with a controlled host-level Nginx or equivalent reverse proxy, and configure:

```env
APP_ORIGIN=https://your-domain.example
VITALS_ALLOW_PUBLIC_APP_ORIGIN=true
TRUST_LOOPBACK_PROXY=true
```

Verify the deployment:

```bash
docker compose ps
docker compose logs --tail=100 vitals
curl -k https://127.0.0.1:19443/health
```

For complete configuration, backup, restore, rollback, Linux/Windows deployment, and security-gate instructions, follow **[agent.md](./agent.md)**.

## Development Verification

```bash
go test ./...
(cd allocation-service && go test ./...)
(cd web && npm run lint && npm test && npm run build && npm run test:e2e)
scripts/security-gate.sh
```

Before changing allocation logic, credential handling, database boundaries, or deployment configuration, read the critical invariants and prohibited actions in `agent.md`.
