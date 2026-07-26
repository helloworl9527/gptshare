# 首个真实失效事件复核（STEP-06）

本流程只适用于自然出现的 `credential_revoked` / `account_disabled` 候选。不得为取证主动封禁账号、吊销仍在使用的凭证、冲击限流或绕过 CAPTCHA。候选出现时系统保持原业务状态，暂停该账号自动轮询，且不生成最终封号告警。

## 前置门禁

1. 在维护窗口停止后端服务，并确认同一 `DB_PATH` 不再被服务进程持锁。
2. 以数据库所属的专用服务用户登录本机；数据库必须为 `0600`，父目录为 `0700` 或更严格。若使用 `ENVIRONMENT_FILE`，文件必须由当前服务用户所有、owner 可读且 group/other 不可访问（如 `0400`/`0600`）。
3. 通过独立渠道核对账号 ground truth。不得把原始上游响应、Cookie、token、TOTP secret 或设备码写入理由、终端历史、工单或截图。
4. 从数据库/受控管理视图取得待复核的 `ev1:<sha256>` 精确签名；它绑定 endpoint、stable code 与 parser/contract version。签名改变必须重新待复核。

## 执行

TOTP 只能经 stdin 输入；不要放入 argv 或环境变量：

```sh
read -r -s TOTP_CODE
printf '%s\n' "$TOTP_CODE" | ./evidence-review \
  --signature 'ev1:<sha256>' \
  --decision confirm \
  --reason 'ground truth confirmed terminal account state'
unset TOTP_CODE
```

- `confirm`：将该精确签名提升为 `live_verified`，以首次检测时间重放 `dead_banned` 判定，写字段级审计并产生唯一 outbox 事件。
- `reject`：保持业务状态，记为 `contract_changed`，继续暂停账号，命令输出 `action_required=reopen_codex_review`。此时必须暂停相关签名并返回方案审查，不得静默改规则。

命令拒绝以下情况：服务仍在运行、数据库/目录/环境文件权限不正确、非数据库所属用户、TOTP 错误、签名/决定/理由缺失。审计只记录操作者、时间、签名、决定和理由，不记录凭证或原始响应。

## 残余风险

一次真实确认会对同一窄签名泛化（AA-5）。若后续真实事件与已确认语义矛盾，立即暂停该签名和相关账号，保留历史，重开 Codex/用户裁决。真实负样本一旦自然可得，必须净化后回补 FINDINGS、fixture 与风险登记。
