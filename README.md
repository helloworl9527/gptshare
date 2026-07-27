# Vitals · 统一运营后台（ChatGPT 账号监控 + 账号分配共享）

Vitals 是一个**本机优先**的运营后台，把两块能力合并为**一个 Go 单二进制、单进程、单端口、单登录**的模块化单体：

1. **账号状态监控（Monitor，一期）** — 管理员密码 + TOTP 登录、Codex CLI OAuth 手动回调、单个/批量令牌与设备码导入、加密凭证存储、多 worker 状态轮询、首次真实封禁事件保态复核、告警 outbox，以及 Vitals 运营总览 / 账号体征 / 详情 / 导入 / 配置界面。
2. **账号自动分配与卡密共享（Allocation，二期）** — 卡密批量生成 / 兑换 / 查询 / 延期 / 作废、以"账号时间浪费最少"为首要目标的最优分配算法、到期 24h 宽限替换与封号立即替换、三级库存预警、面向终端用户的实时 TOTP（倒计时 / 手动刷新）与账号密码一键复制的公开兑换页。

二期通过**进程内 facade**只读调用一期的监控能力（账号存活 / 封禁判定），据此做分配与替换决策。

## 模块化单体：合了什么、没合什么

| 合并（统一） | 保持隔离（红线） |
|---|---|
| 单二进制 `vitals` / 单进程 / 单端口 | **两个独立数据库**：`monitor.db` 与 `allocation.db` |
| 单套管理员登录（密码 + TOTP）护两域 | **两套独立数据加密密钥命名空间**（监控凭证 vs 分配凭证 / 卡密） |
| 单前端（Vue3，方向A 统一导航）| 禁止 SQL ATTACH / 跨库 join / 跨库事务 |
| 单 origin、严格 CSP | 后台任务 panic 隔离（GoManaged 监督器），崩溃不击穿公开页 |

> 统一登录密钥与两套**数据加密**密钥彼此独立、材料不复用；启动时全材料交叉校验，复用即 fail-closed。

## 技术栈

- **后端**：Go 1.25，Gin，SQLite（`modernc.org` 纯 Go，无 CGO，可跨平台交叉编译），AES-GCM（AEAD + AAD 绑定）。两个 Go module：根 `chatgpt-monitor` 与 `allocation-service`（本地 `replace` 聚合）。
- **前端**：Vue 3 + Vite + Element Plus；构建产物 embed 进 `internal/unifiedui/static/`，由 Go 直接服务。
- **入口**：`/admin` 统一后台；`/` 公开卡密兑换页；单 loopback 端口，前置 Nginx 做 TLS。

## 目录速览

```
cmd/vitals/          合并单体主入口
cmd/vitals-migrate/  启动前迁移 runner（两库各自 schema_migrations）
internal/            监控域 + 统一 auth/config/app/facade/前端 embed
allocation-service/  分配域（独立 module：分配算法、卡密、替换、迁移）
web/                 统一前端源码（Vue3/Vite），构建后 embed
deploy/vitals/       systemd / Nginx / env 模板 / 运维 runbook
scripts/             构建、迁移、备份、恢复、回滚、安全门禁脚本
docs/                运维、密钥轮换、风险登记、OpenAPI
test/e2e/            真实二进制端到端回归
```

## 快速开始（本地）

```bash
# 1) 构建前端（产物 embed 进后端）
cd web && npm ci && npm run build && cd ..

# 2) 构建后端
go build ./cmd/vitals ./cmd/vitals-migrate

# 3) 准备环境变量（见 deploy/vitals/server.env.example，占位符需替换）
#    需 HTTPS loopback 反代满足 APP_ORIGIN

# 4) 先迁移，再启动
./vitals-migrate
./vitals
```

访问：后台 `https://<APP_ORIGIN>/admin/`，公开兑换页 `https://<APP_ORIGIN>/`。

## Docker 一键部署

新服务器需预装 Docker（含 Compose 插件）和 OpenSSL。克隆仓库后执行：

```bash
sudo ./deploy.sh
```

脚本会自动构建多阶段镜像、生成独立运行密钥和 TOTP、创建本机自签名
TLS 证书、启动容器并等待健康检查。默认仅监听服务器回环地址：

- 应用：`127.0.0.1:18081`
- HTTPS：`https://127.0.0.1:19443/`
- 初始登录信息：`deploy-credentials.txt`（权限 `0600`）
- 持久化数据库：`data/`

重复执行脚本会保留已有 `.env`、证书和数据库。可在首次执行前通过
`APP_HTTP_PORT`、`APP_HTTPS_PORT` 和 `ADMIN_USER` 覆盖默认值。

公网域名部署时，应用仍应只监听回环地址，并由宿主机 Nginx 终止 TLS。
将 `APP_ORIGIN` 设置为完整 HTTPS Origin，同时显式设置
`VITALS_ALLOW_PUBLIC_APP_ORIGIN=true`；默认值为 `false`，防止意外公开部署。

## 测试与门禁

```bash
go test ./...                     # 根 module
(cd allocation-service && go test ./...)
(cd web && npm run lint && npm test && npm run build && npm run test:e2e)
scripts/security-gate.sh          # 源码密钥/明文 sink 扫描（0 sink 才可打包）
```

## 安全与边界

- 生产上传 / 部署 / 流量切换 / 密钥轮换均需**单独授权**，不随本仓库自动发生。
- 兼容 `/api/v1/monitor/*` 默认关闭并返回 404；启用需具名消费者 + 到期 + 强 key + 限流 + 审计。
- 卡密与账号凭证在库内加密；公开页展示的账号密码 / TOTP 为按需解密下发（已知取舍见 `docs/risk-register.md`）。

部署到 Linux / Windows 的完整步骤见 **[agent.md](./agent.md)**。
