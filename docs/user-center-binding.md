# 统一用户中心绑定（绑定票据 + 兑换事件 + 本机内部接口 + `#code=` 进入）

本服务（GPT 共享账号兑换，gpt.baochai.cc）为宝钗统一用户中心提供兑换归属数据。归属**不信任浏览器回报**：
用户中心签发绑定票据，本服务只在**真正扣码的同一个数据库事务里**记录「这次兑换是哪个登录用户做的」，
用户中心再从本机内部接口拉取事件。

> 本分支只包含代码、测试和文档；未合并、未部署、未连接任何服务器。

## 1. 行为摘要

| 场景 | 结果 |
| --- | --- |
| `/api/redeem` 不带 `X-Bind-Ticket` | 匿名兑换，行为与之前完全相同；不写事件、不写 nonce |
| 带有效票据，且这次真的扣码（新开分配） | 200；同一事务内插入 `used_bind_nonces` 与 `redemption_events(kind='new')` |
| 带有效票据，但卡密已兑换（只是返回原结果） | 200 返回原结果；**不写事件、不消耗 nonce**（他人不能借此「抢绑」） |
| 票据格式 / 签名 / `v` / `svc` / 过期不通过，或 nonce 已用过 | `400 {"ok":false,"error":{"code":"ticket_invalid","message":"登录状态已过期，请刷新页面后重试"}}`，卡密不被消耗，**不降级为匿名兑换** |
| 未配置票据功能却收到 `X-Bind-Ticket` | 同上 400（无法校验就不能静默丢掉归属） |
| 同一请求带多个 `X-Bind-Ticket` 头 | 400 `ticket_invalid` |
| 事务中任一步失败（分配、扣码、写事件） | 整体回滚：卡仍为 unused，无分配、无 nonce、无事件 |

票据：`<payload_b64url>.<sig_b64url>`（均为**无填充** base64url）；
`sig = HMAC-SHA256(UC_BIND_TICKET_KEY, payload_b64url 的 ASCII 字节)`，常量时间比较；
载荷 `{"v":1,"svc":"sharegpt","uid":"…","nonce":"<22 位 base64url 字符>","exp":<unix 秒>}`，要求 `exp > now-30s`。
`uid` 允许 `[A-Za-z0-9_.:-]`，1–128 字节。票据有效期上限由用户中心决定，本服务不额外限制。

事件中的 `code_hash = sha256(规范化卡密)` 十六进制（规范化 = 大写的 `XXXX-XXXX-XXXX`），
`entitlement_ref = "card:<卡 ID>"`（客户自己的卡，不是共享账号 ID；多个客户分到同一共享账号时各自独立），
`detail` 只有 `{"plan":"30天","expire_at":"YYYY-MM-DD"}`，`expire_at` 及 status 接口的日期按北京时间（UTC+8）取日。

### 关于 `/api/redeem-different`、`/api/cards/renew`、`action-preview`

本仓库**不存在**这三个接口：公开侧唯一会扣码 / 新开分配的接口是 `/api/redeem`；续期（`extend`）只在管理端
（`/api/admin/cards/:id/extend`，管理员会话 + CSRF），没有用户票据。因此本次只在 `/api/redeem` 接入票据，
目前只会产生 `kind='new'` 事件。表结构和仓储层已支持 `renew`，以后若增加用户侧续期接口，
必须同样调用 `RedeemWithBinding` 风格的同事务写入，而不是事后补写。

## 2. 本机内部接口

只在**本机直连**时可用：请求必须来自回环地址，且不能带 `X-Real-IP`、`X-Forwarded-For`、`Forwarded`、
`X-Forwarded-Host`、`X-Forwarded-Proto`、`CF-Connecting-IP`、`True-Client-IP` 中任何一个头；
`X-UC-Token` 先取 SHA-256 再常量时间比较。任何一项不满足 → **404 空响应体**。未配置时路由根本不注册（同样 404）。

| 接口 | 说明 |
| --- | --- |
| `GET /internal/uc/events?after=<id>&limit=200` | 按 id 升序返回 `id > after` 的事件；`limit` 默认 200，超过 500 按 500；`after`/`limit` 非法 → 400。返回 `{"events":[…],"next_after":<最后一个 id，空页时等于 after>}` |
| `POST /internal/uc/status {"refs":["card:12"]}` | 最多 500 个 ref。**只返回出现在兑换事件里的卡**（匿名兑换的卡、不存在的卡、格式不对的 ref 都不出现）。`state`：`active` / `expiring`（3 天内到期）/ `expired`（到期、过期或作废）/ `unknown`；`display` 只有脱敏的 `{"当前账号":"share-07＠***.com"}`，过期后为空 |
| `POST /internal/uc/detail-link {"ref":"card:12"}` | `{"url":"<UC_DETAIL_ORIGIN>/#code=XXXX-XXXX-XXXX"}`：服务按权益解密卡密放进 URL 片段；只对已绑定的卡有效，其它 → 404 |

三个 nginx 模板（`docker/default.conf.template`、`deploy/gpt.baochai.cc.nginx.conf`、`deploy/vitals/nginx.conf`）
都显式加了 `location ^~ /internal/ { return 404; }`；即使有人漏配，反代总会加 `X-Forwarded-For`，服务端也会 404。

## 3. 前端 `#code=` 进入

公开兑换页读取 `#code=XXXX`：规范化后填入输入框，**立即** `history.replaceState` 去掉片段，然后**只查询不兑换**
（避免被构造的链接悄悄消耗一张未用的卡；用户仍可点按钮兑换）。格式不对时提示「链接中的卡密无效，请手动输入。」且不发请求。
卡密任何时候都不写进路径或查询参数；页面也监听 `hashchange`。

## 4. 改动的文件

- 新增
  - `allocation-service/internal/usercenter/ticket.go`：票据校验
  - `allocation-service/internal/usercenter/service.go`：内部接口业务（事件分页、状态、详情链接、令牌比较、脱敏）
  - `allocation-service/internal/repository/user_center.go`：同事务 nonce/事件写入、查询、nonce 清理
  - `allocation-service/internal/httpapi/user_center.go`：票据接入、本机直连 + 令牌中间件、内部路由
  - `allocation-service/internal/store/migrations/0018_user_center_binding.sql`：`used_bind_nonces`、`redemption_events`
  - 测试：`allocation-service/internal/usercenter/ticket_test.go`、`allocation-service/internal/repository/user_center_test.go`、
    `allocation-service/internal/httpapi/user_center_routes_test.go`、`allocation-service/module/user_center_test.go`、
    `internal/vitalsconfig/user_center_test.go`、`test/e2e/user_center_binary_test.go`
- 修改
  - `allocation-service/internal/repository/repository.go`：`redeemCard` 在扣码事务内调用 nonce/事件写入；新增 `RedeemCodeWithBinding`
  - `allocation-service/internal/allocator/service.go`：`RedeemWithBinding`，nonce 重放映射为 `ErrBindingReplayed`
  - `allocation-service/internal/httpapi/router.go`：`/api/redeem` 校验 `X-Bind-Ticket`
  - `allocation-service/module/module.go`：`EnableUserCenter`、内部路由注册、`RunBindNonceCleanup`
  - `internal/vitalsconfig/config.go`：新配置项与启动门禁
  - `cmd/vitals/main.go`：装配；新增监督器任务 `bind-nonce-cleanup`（每 10 分钟清理 exp 早于 now-5min 的 nonce）
  - `web/public/static/user.js`、`internal/unifiedui/static/static/user.js`（两者保持字节一致）、两份 `user.html`（缓存版本号）
  - `web/tests/vitals.spec.js`：`#code=` 的 Playwright 用例
  - `test/e2e/vitals_binary_test.go`：崩溃矩阵加入 `bind-nonce-cleanup`；未配置时 `/internal/uc/events` 必须 404
  - nginx 模板（见上）、`deploy/vitals/server.env.example`、`agent.md`

两个 SQLite 库仍然独立：新表都在分配库，事务只涉及分配库，没有 ATTACH / 跨库事务；CSP 未改动。

## 5. 新配置项

| 变量 | 格式 | 说明 |
| --- | --- | --- |
| `UC_BIND_TICKET_KEY` | 64 位十六进制（32 字节） | 票据 HMAC 密钥，与用户中心共享 |
| `UC_INTERNAL_TOKEN` | 64 位十六进制（32 字节） | 内部接口 `X-UC-Token` 共享密钥（原样比较这串十六进制） |
| `UC_DETAIL_ORIGIN` | `https://host`（无路径/查询/片段） | detail-link 使用的公开兑换页 origin，生产应为 `https://gpt.baochai.cc`。单独设置而不复用 `APP_ORIGIN`，因为 Docker 部署里 `APP_ORIGIN` 是 `https://127.0.0.1:<port>` |

启动门禁（`vitals` 与 `vitals-migrate --validate-only` 都会执行，fail-closed）：

- 三项都留空或不设置 → 功能关闭，匿名兑换不受影响；
- 只配了两个密钥中的一个、或配了 `UC_DETAIL_ORIGIN` 却没配密钥 → 拒绝启动；
- 非 64 位十六进制、示例 / 占位值、弱值（32 字节里不同字节值少于 16 个，例如全 0、`0123…ef` 循环）→ 拒绝启动；
- 两个密钥相同（不区分大小写），或与监控 / 分配数据密钥、`JWT_SIGNING_KEY`、`RATE_LIMIT_KEY`、`ADMIN_TOTP_SECRET`、
  `ALLOCATION_SERVICE_API_KEY` 的原始字节或其 hex / base64 表示相同 → 拒绝启动；
- `UC_DETAIL_ORIGIN` 不是 https origin → 拒绝启动。

## 6. 部署时要做的事（由用户中心那边执行；本分支未做任何部署）

1. **生成密钥**（各自独立生成，只放进 `.env`，`chmod 0600`，不要提交）：
   ```bash
   openssl rand -hex 32   # → UC_BIND_TICKET_KEY（同一值配给用户中心签票据）
   openssl rand -hex 32   # → UC_INTERNAL_TOKEN（同一值配给用户中心作为 X-UC-Token）
   ```
   在 `.env` 增加 `UC_BIND_TICKET_KEY=…`、`UC_INTERNAL_TOKEN=…`、`UC_DETAIL_ORIGIN=https://gpt.baochai.cc`。
2. **备份两个库**（`data/monitor.db`、`data/allocation.db`）。迁移 runner 在升级分配库前也会自动写一份
   `allocation.db.pre-migrate-<时间戳>.bak`。
3. **迁移**：容器入口已是 `vitals-migrate --validate-only` → `vitals-migrate` → `vitals`，重建镜像并重启容器即可；
   分配库 schema 17 → 18（只新增两张表和两个索引，不改已有表）。迁移失败不要启动服务。
4. **更新 nginx**：宿主机 `deploy/gpt.baochai.cc.nginx.conf` 与容器内 `docker/default.conf.template` 已加 `/internal/` → 404，
   部署后 `nginx -t` 并 reload；从公网验证 `curl -i https://gpt.baochai.cc/internal/uc/events` 为 404。
5. **网络要求**：内部接口只接受回环来源。`vitals` 只监听 `127.0.0.1:${APP_HTTP_PORT}`（默认 18081），
   用户中心后端必须以宿主机进程或 `network_mode: host` 容器的方式直连 `http://127.0.0.1:18081/internal/uc/...`，
   **不要**经过 nginx（会被 404），也不能从 bridge 网络容器访问（来源不是回环）。
6. **验证**：`/health` 200；`curl -s -H "X-UC-Token: $UC_INTERNAL_TOKEN" 'http://127.0.0.1:18081/internal/uc/events?after=0'`
   返回 `{"events":[],"next_after":0}`；去掉令牌或加 `-H 'X-Forwarded-For: 1.2.3.4'` 应为 404；匿名兑换照常。

### 浏览器侧如何带上票据（待与用户中心确认）

本分支**没有**让公开兑换页自己去拿票据，也**没有**为 `/api/redeem` 开 CORS：页面 CSP 是 `connect-src 'self'`，
不能跨域向 baochai.cc 取票据。可行的接法：(a) 用户中心后端在本机把兑换请求连同 `X-Bind-Ticket` 转发到
`http://127.0.0.1:18081/api/redeem`（现在即可用）；(b) 如需用户中心页面在浏览器里跨域直接调用，
需要另行为 `/api/redeem` 增加只允许 `https://baochai.cc` 的 CORS（含 `X-Bind-Ticket` 预检）——这会放宽现有边界，单独评审后再做。

## 7. 回滚

由轻到重：

1. **只关功能**（推荐，无数据风险）：从 `.env` 删掉（或置空）三个 `UC_*` 变量并重启。票据请求会得到 400、
   内部接口 404，匿名兑换不受影响；已有事件保留，重新开启后继续可用。
2. **回到旧镜像但保留数据**：旧二进制看到 schema 18 会拒绝启动（"newer than binary schema"）。先停服务、备份当前库，再执行
   ```sql
   DROP TABLE redemption_events;
   DROP TABLE used_bind_nonces;
   DELETE FROM schema_migrations WHERE version = 18;
   ```
   然后启动旧镜像。升级之后发生的兑换（卡、分配）都保留，只丢绑定事件。
3. **整体恢复备份**：用 `allocation.db.pre-migrate-*.bak`（或第 2 步的手动备份）恢复。注意这会丢掉升级之后的全部兑换记录，
   只在库损坏时使用。

nginx 中新增的 `/internal/` → 404 与旧版本兼容，回滚时无需撤销。

## 8. 测试

- `go test ./...`（根模块，含 `test/e2e` 真实二进制：启动门禁 fail-closed、匿名不写事件、带票据写事件、重放/过期/伪造 400、
  已兑换卡不写事件、带转发头 404、status 脱敏、detail-link 走片段；崩溃矩阵含 `bind-nonce-cleanup`）
- `cd allocation-service && go test ./...`（票据矩阵、同事务注入失败回滚 ×3、分页、内部接口鉴权、非回环来源、多客户同账号分开）
- `cd web && npm run lint && npm test`；Playwright `#code=` 两个用例
- `scripts/security-gate.sh`、`scripts/security-gate-parity.sh`
