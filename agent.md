# agent.md — 供 AI Agent 理解与部署本仓库

本文件让任何 agent 读完后即可**理解仓库结构**并**把服务部署到 Linux 或 Windows**。面向自动化，命令可直接执行。

---

## 1. 这是什么

`Vitals` = **模块化单体**：一个 Go 二进制同时提供
- **监控域（monitor）**：ChatGPT 账号存活 / 封禁监控。
- **分配域（allocation）**：卡密共享 + 账号自动分配。

关键不变量（改代码时务必守住）：
- **两个独立 SQLite 库**：`monitor.db`、`allocation.db`。禁止 SQL `ATTACH`、跨库 join、跨库事务。
- **两套独立数据加密密钥命名空间** + **一套独立统一登录密钥**，三者材料不可复用（启动 fail-closed 校验）。
- 分配域经**进程内 facade** 只读调用监控域，不走 HTTP。
- 后台任务用 `GoManaged` 监督器包裹，panic 不得击穿公开兑换页。
- 前端严格 CSP（`'self'`，无 `unsafe-inline`），单 origin，字体 / 脚本全同源。

## 2. 代码地图

| 路径 | 作用 |
|---|---|
| `cmd/vitals/` | 单体主入口（组装 auth + 两域 + 前端 embed + 后台监督器） |
| `cmd/vitals-migrate/` | 启动前迁移 runner；先校验全部密钥材料，再依次迁移两库 |
| `internal/vitalsconfig/` | 统一配置加载 + 启动门禁（密钥缺失 / 弱 / 示例值 / 复用 → 拒绝） |
| `internal/vitalsapp/` | HTTP 路由聚合、CSP、健康检查（`background:degraded` 非致命） |
| `internal/auth/` | 唯一管理员 auth（密码 + TOTP + session + CSRF + 限流） |
| `internal/monitorfacade/` | 监控域进程内 facade（供分配域只读调用） |
| `internal/unifiedui/` | 前端 embed（`/admin`、`/assets`、`/`、`/static`） |
| `internal/httpapi/`, `internal/...` | 监控域业务 |
| `allocation-service/` | 分配域独立 Go module（分配算法、卡密、替换、迁移、公共装配包 `module/`） |
| `web/` | 统一前端源码（Vue3/Vite），`npm run build` → `internal/unifiedui/static/` |
| `deploy/vitals/` | `vitals.service`、`nginx.conf`、`server.env.example`、两份回滚 env、`README.md` |
| `scripts/` | `security-gate*.sh`、`vitals-{preflight,backup,restore,rollback,deploy-contract,resource-budget}.sh`、`build-release.sh` |

## 3. 前置依赖

- **Go 1.25+**（SQLite 用 `modernc.org` 纯 Go 驱动，**无需 CGO**，因此可跨平台交叉编译）。
- **Node.js 22.18+ 或 24.11+**（构建前端；仅构建期需要，运行期不需要）。
- 生产建议前置 **Nginx**（或 Caddy）终结 TLS，因为 `APP_ORIGIN` 要求 HTTPS。

## 4. 构建

```bash
# 前端（产物会被 Go embed，必须先构建）
cd web && npm ci && npm run build && cd ..

# 后端二进制
go build -o vitals        ./cmd/vitals
go build -o vitals-migrate ./cmd/vitals-migrate
```

交叉编译（无 CGO，直接可用）：
```bash
# Linux amd64
GOOS=linux   GOARCH=amd64 go build -o dist/linux/vitals        ./cmd/vitals
GOOS=linux   GOARCH=amd64 go build -o dist/linux/vitals-migrate ./cmd/vitals-migrate
# Windows amd64
GOOS=windows GOARCH=amd64 go build -o dist/windows/vitals.exe         ./cmd/vitals
GOOS=windows GOARCH=amd64 go build -o dist/windows/vitals-migrate.exe ./cmd/vitals-migrate
```

## 5. 配置（环境变量）

以 `deploy/vitals/server.env.example` 为准，占位符全部替换。核心项：

| 变量 | 说明 |
|---|---|
| `VITALS_PORT` | 监听地址，如 `127.0.0.1:8080` |
| `APP_ORIGIN` | 对外 HTTPS origin，如 `https://vitals.example.com`（用于 CSRF / cookie / CSP） |
| `TRUST_LOOPBACK_PROXY` | 前置反代时 `true` |
| `MONITOR_DB_PATH` / `ALLOCATION_DB_PATH` | 两库路径（**必须不同文件**） |
| `MONITOR_MIGRATIONS_DIR` | 监控迁移目录（release 内 `migrations/`） |
| `CREDENTIAL_MASTER_KEYS` / `CREDENTIAL_ACTIVE_KEY_ID` | 监控凭证加密（标准 base64 32B） |
| `ALLOCATION_CREDENTIAL_MASTER_KEYS` / `..._ACTIVE_KEY_ID` | 分配凭证 / 卡密加密（raw-url base64 32B） |
| `ADMIN_USER` / `ADMIN_PASSWORD_HASH` / `ADMIN_TOTP_SECRET` | 统一登录（bcrypt cost≥12；TOTP 无填充 base32） |
| `JWT_SIGNING_KEY` / `RATE_LIMIT_KEY` | 会话签名 / 限流（各自独立 base64 32B，**不得与数据密钥复用**） |
| `VITALS_MONITOR_COMPAT_HTTP_ENABLED` | 默认 `false`；保持关闭 |

独立部署还需配置一期的 `ALLOCATION_ACCOUNT_EVENT_URL` 和双方相同的
`ALLOCATION_ACCOUNT_EVENT_API_KEY`。事件地址必须使用 HTTPS；仅 `localhost` 或
回环 IP 可使用 HTTP。该密钥至少 32 字节，且不得复用管理员会话、监控兼容 API
或任何数据加密密钥。统一进程部署使用本地 sink，不需要这两个变量。

生成 32 字节密钥示例：
```bash
openssl rand -base64 32          # 标准 base64（monitor / JWT / RATE_LIMIT）
openssl rand -base64 32 | tr '+/' '-_' | tr -d '='   # raw-url base64（allocation）
```
> 三类密钥材料必须**各自独立生成**，任何复用都会在启动时被拒。env 文件权限设为 `0600`。

## 6. 启动顺序（强制）

**先迁移，再启动**，两库各自迁移（无跨库事务）：
```bash
./vitals-migrate      # 校验密钥 → 迁移 monitor → 迁移 allocation
./vitals              # 启动服务
```
本次账号事件功能要求 monitor schema 7 和 allocation schema 9。升级时必须先完成
两库备份，再运行迁移 runner；任一迁移失败都不要启动服务。
迁移失败即停机，从对应库的迁移前备份恢复。

## 7. 部署到 Linux（systemd + Nginx）

1. 放置：二进制 → `/opt/vitals/current/`；`migrations/` 同目录；库 → `/var/lib/vitals/`。
2. 渲染 `deploy/vitals/server.env.example` → `/etc/vitals/vitals.env`（`chmod 0600`）。
3. 安装 `deploy/vitals/vitals.service`（内建顺序：校验 env → 备份两库 → `vitals-migrate` → `vitals`；限额 2 CPU / 2.5 GiB）。
4. 安装 `deploy/vitals/nginx.conf`（单 loopback upstream，TLS1.2/1.3，HSTS，严格 CSP，全路径反代）。
5. 上线前在生产主机运行：`systemd-analyze verify`、`nginx -t`、TLS/header 探测、`scripts/vitals-resource-budget.sh`、`scripts/vitals-deploy-contract.sh`。
6. 启停：`systemctl enable --now vitals`；健康检查 `curl -k https://<host>/health`。

回滚到旧双服务：见 `deploy/vitals/README.md`（`scripts/vitals-rollback.sh`，exit 42 = 需先恢复兼容备份；回滚 env 必须含 `ALLOCATION_SERVICE_API_KEY`）。

## 8. 部署到 Windows

Windows 无 systemd；用下述任一方式让 `vitals.exe` 常驻，前面用 Caddy/Nginx/IIS 终结 TLS。

1. 交叉编译得到 `vitals.exe`、`vitals-migrate.exe`（见 §4）。
2. 配置环境变量（§5）——可用 `.env` 由启动脚本注入，或系统环境变量。库路径用 Windows 路径，如 `C:\ProgramData\vitals\monitor.db`、`allocation.db`。
3. 先跑迁移：`vitals-migrate.exe`，再起服务：`vitals.exe`。
4. 常驻为 Windows 服务（推荐 [NSSM](https://nssm.cc/)）：
   ```bat
   nssm install Vitals "C:\vitals\vitals.exe"
   nssm set Vitals AppDirectory "C:\vitals"
   nssm set Vitals AppEnvironmentExtra APP_ORIGIN=https://vitals.example.com VITALS_PORT=127.0.0.1:8080 ...
   nssm start Vitals
   ```
   （或用 `sc.exe create` / 任务计划程序开机启动。）
5. TLS：Windows 上最简用 **Caddy**（自动 HTTPS）反代到 `127.0.0.1:8080`，或用 Nginx-for-Windows 复用 `deploy/vitals/nginx.conf` 的等价配置。
6. 健康检查：`curl.exe -k https://<host>/health`。

> SQLite 为纯 Go 实现，Windows 无需额外原生依赖；换行 / 路径分隔符由 Go 处理。

## 9. 验证清单（部署后）

- `GET /health` → 200，body 含 `status:ok` 且两库 `ok`（`background:degraded` 属非致命）。
- `GET /admin/` → 200，可用统一账号（密码 + TOTP）登录。
- `GET /` → 200，公开卡密兑换页可兑换 / 查询。
- `GET /api/v1/monitor/xxx` → 404（兼容面默认关闭）。
- 后台任务 panic 不影响公开页（监督器隔离）。

## 10. 测试与安全门禁（改动后必跑）

```bash
go test ./... && (cd allocation-service && go test ./...)
(cd web && npm run lint && npm test && npm run build)
go test ./test/e2e -run TestVitalsBinaryUnifiedFlowAndCrashLoopMatrix   # 真实二进制端到端
scripts/security-gate.sh && scripts/security-gate-parity.sh             # 0 unauthorized sink
```

## 11. 红线（禁止事项）

- 不要合并两个数据库或引入跨库事务 / ATTACH。
- 不要复用密钥材料（监控 / 分配 / 统一登录三者互相独立）。
- 不要放宽 CSP 或引入外部 CDN。
- 不要在未获显式授权时执行生产上传 / 部署 / 密钥轮换 / 流量切换。
- 不要用裸 `go func()` 启动长驻后台任务——用 `GoManaged` 监督器。
