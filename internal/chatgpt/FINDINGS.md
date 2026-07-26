# STEP-01 技术验证结论

采集日期：2026-07-22（Asia/Shanghai）  
结论：**四条有效凭证路径均为 `live_verified` 且连续两轮 PASS；完整合成负向矩阵通过，真实负样本暂不可得的单元为 `contract_verified_live_pending`。依据 revision 6，STEP-01 已达到提交用户验收的自动门禁，但尚未获得 `验收通过：STEP-01`，不得进入 STEP-02。**

## 固定参考实现

- 仓库：`router-for-me/CLIProxyAPI`
- 固定提交：`36b45d57a3e804b9dfcee307e5d7b3e8cea5acfc`
- 许可证：MIT
- 研读/借鉴文件：
  - `sdk/auth/codex_device.go`：设备码 start/poll、client id、redirect URI。
  - `internal/auth/codex/openai_auth.go`：OAuth token/refresh 参数与 scope。
  - `internal/auth/codex/jwt_parser.go`：ChatGPT account/plan/subscription claims。
- 复用方式：仅按固定提交核对协议并独立实现最小标准库客户端；未复制上游包或新增其依赖。

## 已锁定契约

| 用途 | 方法与端点 | 必需输入 | 字段位置 |
|---|---|---|---|
| Session → access | `GET https://chatgpt.com/api/auth/session` | Cookie `__Secure-next-auth.session-token` | `/accessToken` |
| Device start | `POST https://auth.openai.com/api/accounts/deviceauth/usercode` | JSON `/client_id` | `/device_auth_id`、`/user_code`、`/interval` |
| Device poll | `POST https://auth.openai.com/api/accounts/deviceauth/token` | JSON `/device_auth_id`、`/user_code` | `/authorization_code`、`/code_verifier` |
| Code → token | `POST https://auth.openai.com/oauth/token` | form `authorization_code`、client id、`https://auth.openai.com/deviceauth/callback`、PKCE verifier | `/access_token`、`/refresh_token`、`/id_token` |
| Refresh → token | `POST https://auth.openai.com/oauth/token` | form `refresh_token`、client id、scope=`openid profile email` | `/access_token`、`/refresh_token`、`/id_token` |
| 账号标识 | access JWT claim（仅内存解析） | access token | `/https:~1~1api.openai.com~1auth/chatgpt_account_id` |
| 套餐/订阅到期/可用性 | `GET https://chatgpt.com/backend-api/accounts/check/v4-2023-04-27` | `Authorization`、`X-Authorization`、`Chatgpt-Account-Id` | `/accounts/{id}/entitlement/subscription_plan`、`/accounts/{id}/entitlement/expires_at`；2xx + 目标账号结构完整为 active |

上述 ChatGPT 端点为未公开契约，存在变化和使用条件风险；生产代码必须继续把 HTML/WAF、字段缺失和未知响应归为 `contract_changed`，不得改判封号。

## 凭证路径 × 核心字段证据矩阵

匿名样本：`acct-A`；输出账号标识为 `sha256:966401d71ac31022`，不保存原始 ID。  
脱敏上游响应 SHA-256：`482ef1c1b06e059a91a07dfd7c55b5522a035a4787bee603a098e7f9499a30c2`。

| 路径 | provider_account_id | raw plan → plan | subscription expiry | account state | EvidenceLevel | 第 1 轮 | 第 2 轮 |
|---|---|---|---|---|---|---|---|
| access | PASS（JWT claim，输出哈希） | `chatgptplusplan` → `plus` | `2026-08-19T18:28:13Z` | `active` | `live_verified` | PASS / exit 0 | PASS / exit 0 |
| Session | PASS（Session→access 后同上） | `chatgptplusplan` → `plus` | `2026-08-19T18:28:13Z` | `active` | `live_verified` | PASS / exit 0 | PASS / exit 0 |
| device | PASS（device→access/refresh 后同上） | `chatgptplusplan` → `plus` | `2026-08-19T18:28:13Z` | `active` | `live_verified` | PASS / exit 0 | PASS / exit 0 |
| refresh | PASS（每轮接续轮换后的 refresh） | `chatgptplusplan` → `plus` | `2026-08-19T18:28:13Z` | `active` | `live_verified` | PASS / exit 0 | PASS / exit 0 |

订阅到期与用户真值“2026-08-19”一致；JWT 的通用 `exp` 与 Session `expires` 没有被误用为订阅到期。

## 响应 → 判定矩阵

| 判定 | 稳定条件 | banned candidate | 保留旧状态 | 合成测试 | EvidenceLevel |
|---|---|---:|---:|---|---|
| `active` | accounts-check 2xx、JSON、目标账号与三项字段完整 | 否 | 不适用 | PASS + 四路径真实两轮 | `live_verified` |
| `access_expired_refreshable` | access `exp` 已过期→同一授权 refresh 成功→新 access status active | 否 | 是 | 完整链 PASS | `contract_verified_live_pending` |
| `credential_revoked` | 稳定 `token_revoked` / `refresh_token_reused` 等代码 | 是 | **是，候选不得直接写死** | PASS（净化 fixture） | `contract_verified_live_pending` |
| `account_disabled` | 稳定 `account_disabled` / `account_deactivated` 代码 | 是 | **是，候选不得直接写死** | PASS（净化 fixture） | `contract_verified_live_pending` |
| `permission_or_scope_denied` | 普通 401/403，且没有稳定吊销/停用代码 | 否 | 是 | PASS | `contract_verified_live_pending` |
| `rate_limited` | HTTP 429 | 否 | 是 | PASS | `contract_verified_live_pending` |
| `upstream_transient` | 5xx、连接/读取失败、超时 | 否 | 是 | PASS | `contract_verified_live_pending` |
| `contract_changed` | 已知 HTML/WAF、非 JSON、字段缺失 | 否 | 是 | PASS | `contract_verified_live_pending` |
| 未知 HTTP/错误码/未覆盖签名 | 无已验证合成契约 | 否 | 是 | fail-closed PASS | `unverified` |

401/403 本身绝不判定为封号；429、5xx、超时、HTML/WAF 和字段缺失也绝不产生 banned candidate。只有稳定吊销/停用代码产生 candidate，但其等级在真实首事件复核前仍为 `contract_verified_live_pending`，`PreserveBusinessState=true`；任何未知签名均 `unverified`、`BannedCandidate=false`、fail closed。

## RA-2：Session Token 独立结论

**可用。** 当前 Session Token 能经 0600 文件输入，通过 `__Secure-next-auth.session-token` 调用 session endpoint 交换出 access token；连续两轮取得与 access/device/refresh 相同的账号哈希、Plus、订阅到期和 active 结果。无需浏览器自动化或 CAPTCHA。

## 安全与兼容性

- probe 只接受 stdin 或经 `stat` 验证的 0600 普通文件；凭证值不进入 argv/env。
- token、Cookie、设备码、邮箱、原始账号 ID、原始响应体不进入输出、日志、台账、fixture 或本文件。
- 生产接口成功返回 `StatusResult{ProviderAccountID,RawPlan,Plan,SubscriptionExpiry,AccountState,EvidenceCode,EvidenceLevel}`；失败返回 `TypedError{Kind,EvidenceCode,EvidenceLevel,Retryable,BannedCandidate,PreserveBusinessState}`。原始 JSON 仅在内存中解析/哈希。
- plan 为开放映射：`free/plus/team/unknown(raw)`；`unknown` 不能替代缺失的套餐或到期字段。
- 探针退出码：0=已证实可用，10=暂态/可重试，20=契约或不可用，2=本地输入/权限错误。

## RISK-OQ1-NEGATIVE-SEMANTICS

真实“过期 access + 有效 refresh”、吊销、封禁、权限、限流和暂态负样本目前客观不可安全取得；不得主动封禁账号、吊销在用凭证、冲击限流或绕 CAPTCHA。因此这些负向响应的上游真实语义没有被冒充为实测，维持 `contract_verified_live_pending`。

- 风险方向：可能漏判/延迟确认真实封号，不允许把未实证候选直接写成终态；不是把普通错误误杀为封号。
- STEP-01 补偿：完整合成矩阵、显式 EvidenceLevel、候选封号位、保态位、未知签名 fail closed、净化 fixture 与零泄漏扫描。
- 首事件责任（STEP-06，尚未实施）：首个自然真实 `credential_revoked/account_disabled` candidate 必须保态、暂停单账号轮询、不发最终封号告警，由运维人员按计划的离线复核流程确认或否决。
- 证据签名（STEP-06，尚未实施）：registry 与 `endpoint + stable code + parser/contract version` 签名持久化不属于 STEP-01；本步只提供稳定 `EvidenceCode` 与 `EvidenceLevel`，未预置数据库、registry 或复核命令。
- 强制回补：任何真实负样本一旦安全、合法、自然可得，立即净化并回补 FINDINGS/fixture/风险登记，不得等到发布；只有人工确认的精确签名才可提升为 `live_verified`。
- 停止条件：真实样本与当前映射矛盾、普通 401/403/429/5xx/超时/HTML/缺字段产生 banned、未知签名未 fail closed、fixture/日志泄密时，立即暂停相关判定并重开 Codex/用户裁决。

## revision 6 门禁结论

- 已证实：四种有效凭证路径、三项核心字段与 active 连续两轮稳定，均 `live_verified`；Session 路径可用。
- 合成已证实：完整 access-expired→refresh→active 链、吊销/停用候选、普通 401/403、429、5xx、超时、HTML/WAF、字段缺失与未知错误 fail-closed/保态断言通过。
- 未实证：真实负样本语义仍为 `contract_verified_live_pending`，不是 `live_verified`。
- 处置：revision 6 下不因“安全且自然取得的真实负样本客观不可得”单独硬停止；STEP-01 可提交用户验收。用户未给出原话 `验收通过：STEP-01` 前仍不得进入 STEP-02。
