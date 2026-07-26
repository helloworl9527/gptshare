# ChatGPT 错误语义与首事件复核运行手册

## 自动分类边界

| 输入 | EvidenceLevel | 自动业务状态 | 动作 |
|---|---|---|---|
| 已验证成功响应 | `live_verified` | 更新 active/plan/expiry | 记录字段变化 |
| auth expiry 到达 | `live_verified` | `dead_normal` | 正常退役，不计算封前存活天数 |
| 稳定 revoked/disabled 候选 | `contract_verified_live_pending` | 保持原业务状态 | 暂停单账号、等待人工复核、不发最终封号告警 |
| 普通 401/403、429、5xx、DNS/连接/超时 | `contract_verified_live_pending` | 保持原业务状态 | 按类型退避/重试，单账号失败不阻塞整轮 |
| HTML/WAF、字段缺失、签名变化 | `contract_verified_live_pending` 或 `unverified` | 保持原业务状态 | `contract_changed`、暂停并回方案验证 |
| 未知响应/未知签名 | `unverified` | 保持原业务状态 | fail closed，不得判 banned |

## 首个 revoked/disabled 自然真实事件

1. 确认服务只记录允许字段、`endpoint + stable code + parser version` 的 evidence signature 和响应哈希；禁止复制原始响应、令牌、Cookie、邮箱或账号原始标识到聊天、工单和仓库。
2. 核对账号处于“保态 + polling_paused + review pending”，且没有最终 `dead_banned` 状态或封号告警事件。如不满足，立即停止轮询消费者并按上一 release/备份回滚。
3. 停止在线服务或取得离线互斥锁，以独立 `evidence-review` 二进制、最小权限账号和 TOTP 执行确认或否决；运行命令和输出不得含凭证。
4. 只有第二事实来源确认同一精确 signature 后才能 `confirm`：原子升级 registry、重放受影响账号、关闭授权周期、计算封前存活天数并产生一次去重告警。重复确认必须拒绝。
5. `reject` 或无法确定时标记 `contract_changed`，保持原业务状态并继续暂停；签名任一维度变化都视为新候选，旧确认不得继承。
6. 在同一工作日更新 `internal/chatgpt/FINDINGS.md`、净化 fixture、契约测试和 `docs/risk-register.md`。净化后再次执行精确真实秘密逐值扫描及通用高熵模式扫描；任一命中都阻断发布。

## 容错与恢复

- DNS/连接失败、15 秒超时、429 `Retry-After`、5xx：最多 2 次重试，单次超时 15 秒，3 worker；单账号失败记入 poll run 并继续其余账号。
- SQLite busy：有界重试；事务中止不得产生半条状态变化或重复 outbox。启动时把遗留 running poll 标为 `startup_interrupted`，下一轮按持久状态继续。
- 调度轮次互斥；重叠触发记录 skipped，不并行跑第二整轮。
- 恢复后核对账号、活动授权周期、状态变更日志、evidence registry、outbox 和限流状态；事件键必须保持去重。

## 真实样本回补

真实负样本只能被动、合法、自然取得。禁止为取证主动吊销在用凭证、封禁账号、冲限流、启用浏览器自动化绕过 CAPTCHA。净化 fixture 只保留结构和稳定码；记录响应哈希与取得时间，不保存原始响应。未真实验证始终标为 `contract_verified_live_pending`，不得在文档、UI 或发布说明中改写成“已验证”。
