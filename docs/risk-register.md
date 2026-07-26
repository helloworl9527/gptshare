# 一期风险登记

## RISK-OQ1-NEGATIVE-SEMANTICS

- 状态：开放，生产发布前必须复核。
- 负责人：服务维护人；真实自然事件出现时由当班管理员执行离线复核。
- 复查时限：每次发布前、上游响应签名变化时，以及任何真实负样本自然可得后的当日。
- 已验证：access、refresh、Session、device 四条正向路径为 `live_verified`；合成负向契约矩阵覆盖刷新成功、吊销/停用候选、403、429、5xx、超时、HTML/字段缺失及未知签名。
- 尚未真实验证：`access_expired_refreshable`、`credential_revoked`、`account_disabled` 及权限/限流/暂态负响应的上游真实语义。它们保持 `contract_verified_live_pending`，不等于 `live_verified`。
- 控制：普通 401/403/429/5xx/超时/HTML/缺字段不得判封号；未知签名一律 `unverified`、fail closed。首个 revoked/disabled 候选保留业务状态、暂停该账号轮询且不创建最终封号告警，须由最小权限离线工具、TOTP 和第二事实来源确认。
- 回补义务：真实样本一旦安全、合法、自然可得，立即暂停相关自动判定，净化账号、令牌、Cookie、设备码、邮箱、原始响应及原始账号 ID，仅保留稳定错误码、允许字段与响应哈希；更新 `internal/chatgpt/FINDINGS.md`、净化 fixture、风险登记及契约测试。不得主动封禁账号、冲击限流、绕 CAPTCHA 或破坏在用凭证取证。
- 停止/回滚：真实样本与登记语义矛盾、未知签名未 fail closed、普通错误产生 banned、签名变化后沿用旧确认或任一泄密扫描失败时，立即停止相关轮询/告警，标记 `contract_changed`，回到方案验证与用户裁决。

## RISK-LINUX-PREUPLOAD

- 状态：开放；当前 Darwin arm64 本机无法完成。
- 未执行：Ubuntu/等价 Linux 上的 `systemd-analyze verify`、systemd 实际启动、`/proc/<pid>/status` 最小权限、EnvironmentFile/DB 权限隔离、非服务用户不可执行 `evidence-review`、从非回环地址确认 8080 不可达。
- 控制：未完成上述清单不得取得上传放行；不得把 macOS 静态校验描述为 Linux 实测。

## RISK-GO-2026-5932

- 状态：接受到 2026-08-06 或下一次上传授权前（以较早者为准）。
- 事实：`golang.org/x/crypto/openpgp` 已停止维护且无修复版本；当前 `govulncheck` 判定代码受影响 0、导入包受影响 0，项目未调用该包。
- 控制：每次发布前重扫；一旦变为可达或出现替代/修复版本，立即停止发布并升级或移除依赖。

## production_deployment

- 当前值：`pending`。
- STEP-11 仅产生本地证据与带 checksum 的 release 包。任何上传、迁移、重启或生产 smoke 都需要用户针对具体目标主机、维护窗口、备份点明确回复 `批准上传`；通用实施批准或 STEP 验收不能替代该授权。
