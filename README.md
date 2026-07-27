# ChatGPT 账号监控 + 共享账号自动分配

本项目将 **ChatGPT 账号监控** 与 **共享账号自动分配**整合为一个运营系统。服务采用 Go 模块化单体架构，以一个进程、一个端口和一套管理员登录同时提供监控后台、账号池管理、卡密兑换及用户查询页面。

## 核心功能

### ChatGPT 账号监控

- 支持 Codex CLI OAuth 手动回调、设备码、单个令牌和批量令牌导入。
- 加密保存账号凭据，定时并发检查账号的可用、异常及封禁状态。
- 记录授权周期、首次异常证据和封禁存活时间，降低瞬时上游错误造成的误判。
- 提供账号总览、状态详情、重新授权、监控配置及告警 outbox。
- 管理后台使用密码 + TOTP 登录，Session、CSRF 和登录限流统一防护。

监控结果通过进程内只读 facade 提供给分配模块，不需要额外部署或调用内部 HTTP 服务。

### 共享账号自动分配

- 管理员可批量生成、复制、导出、延期和作废卡密。
- 用户通过公开页面兑换卡密后，系统自动选择共享账号并展示账号、密码及本地生成的 TOTP 验证码。
- 已兑换卡密可保存在用户当前浏览器，之后可直接重新查询。
- 每个共享账号可设置独立并发容量，满载账号不会继续参与分配。
- 自动计算总容量、已用容量、近期兑换速率及安全、注意、紧急、耗尽四级库存状态。

#### 自动分配逻辑

用户首次兑换卡密时，系统在同一个数据库事务内完成卡密校验、候选账号选择、容量占用、分配记录创建和卡密状态更新：

1. 过滤已过期、已满载、不可用或不满足业务状态的账号。
2. 监控服务可用时排除已封禁账号，并优先选择 `alive` 状态账号。
3. 比较账号到期时间与卡密到期时间，优先选择时间浪费最少的账号。
4. 时间条件相同时，依次选择当前分配更少、最近使用更早的账号。
5. 通过事务条件更新和唯一约束避免并发兑换导致容量超卖或重复分配。

分配后，后台任务会持续结合监控状态检查现有账号：

- 账号被封禁：立即分配新账号并释放旧账号容量。
- 账号即将到期：提前分配新账号，旧账号保留 24 小时宽限期。
- 没有备用容量：保留当前状态、记录失败审计，并在后续任务中重试。
- 同一张卡密重复查询：返回已有有效分配，不重复占用账号容量。

## 系统边界

| 统一能力 | 必须保持隔离 |
|---|---|
| 单 Go 二进制、单进程、单端口 | `monitor.db` 与 `allocation.db` 两个独立 SQLite 数据库 |
| 单套管理员登录和统一前端 | 监控数据、分配数据、管理员认证使用独立密钥材料 |
| 监控状态供分配逻辑只读使用 | 禁止 SQL `ATTACH`、跨库 join 和跨库事务 |
| 后台任务统一监督 | 后台任务异常不得影响公开兑换页面 |

## 技术栈

- **后端**：Go 1.25、Gin、JWT、TOTP、AES-GCM。
- **数据库**：SQLite（`modernc.org` 纯 Go 驱动，无需 CGO），监控域与分配域各自独立。
- **前端**：Vue 3、Vite、Element Plus；生产产物嵌入 Go 二进制。
- **部署**：Docker 多阶段构建、Docker Compose、Nginx TLS 反向代理。
- **安全**：凭据加密、严格 CSP、同源与 CSRF 校验、管理员登录限流、安全审计。

服务入口：

- `/admin/`：统一运营后台。
- `/`：用户卡密兑换与账号查询页面。
- `/health`：应用及数据库健康检查。

## 目录速览

```text
cmd/vitals/             应用主入口，组装认证、监控、分配、前端及后台任务
cmd/vitals-migrate/     启动前数据库迁移
internal/               监控域、统一认证、配置、HTTP API、facade 和前端嵌入
allocation-service/     账号池、卡密、自动分配、替换任务和库存预警
web/                    Vue 3 管理后台及公开用户页面源码
internal/unifiedui/     前端生产构建产物，由 Go embed 提供
deploy/vitals/          环境变量模板、Nginx、systemd 和运维说明
scripts/                测试、安全扫描、备份、恢复和回滚脚本
docs/                   OpenAPI、安全边界、风险登记及功能说明
test/e2e/               真实二进制端到端测试
agent.md                Agent 理解、修改和部署仓库的完整操作手册
```

## Docker 部署

> 如果由 AI Agent 操作，必须先完整阅读 **[agent.md](./agent.md)**，确认数据库、密钥、备份和生产授权边界后，再执行部署。不要仅根据 README 猜测或跳过部署前检查。

服务器需要预先安装 Docker、Docker Compose 插件和 OpenSSL：

```bash
git clone https://github.com/helloworl9527/gptshare.git
cd gptshare

# Agent：先完整阅读部署手册
cat agent.md

# 构建镜像、生成首次运行配置并启动服务
sudo ./deploy.sh
```

`deploy.sh` 会执行以下操作：

1. 通过多阶段 Dockerfile 构建前端和 Go 二进制。
2. 首次部署时生成独立的数据库加密、Session、限流和 TOTP 密钥。
3. 创建本机 TLS 证书并启动 `vitals` 与 `proxy` 容器。
4. 等待应用和反向代理健康检查通过。

默认地址与文件：

- HTTPS：`https://127.0.0.1:19443/`
- 后台：`https://127.0.0.1:19443/admin/`
- 初始管理员信息：`deploy-credentials.txt`，权限为 `0600`
- 运行配置：`.env`，权限为 `0600`
- 持久化数据库：`data/`

重复执行 `sudo ./deploy.sh` 会保留现有 `.env`、证书和数据库。公网部署时应让应用继续监听回环地址，由宿主机 Nginx 或其他受控反向代理终止 TLS，并正确设置：

```env
APP_ORIGIN=https://your-domain.example
VITALS_ALLOW_PUBLIC_APP_ORIGIN=true
TRUST_LOOPBACK_PROXY=true
```

部署完成后检查：

```bash
docker compose ps
docker compose logs --tail=100 vitals
curl -k https://127.0.0.1:19443/health
```

更完整的配置、备份、恢复、回滚、Linux/Windows 部署和安全门禁说明，以 **[agent.md](./agent.md)** 为准。

## 开发验证

```bash
go test ./...
(cd allocation-service && go test ./...)
(cd web && npm run lint && npm test && npm run build && npm run test:e2e)
scripts/security-gate.sh
```

修改分配逻辑、凭据处理、数据库边界或部署配置前，请先阅读 `agent.md` 中的关键不变量与禁止事项。
