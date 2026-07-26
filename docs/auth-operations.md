# 管理员鉴权运维操作

以下操作只允许在计划维护窗口内，由本机服务用户执行。每次操作前停止服务、确认 SQLite 无活动写入、备份数据库与环境文件，并记录操作者、时间、原因、备份 checksum 和操作结果；操作完成后仅通过回环 HTTPS 做登录 smoke test。

## TOTP secret 轮换

1. 停止服务并备份数据库与仅服务用户可读的环境文件。
2. 在离线受控终端生成新的高熵 base32 secret，通过 0600 EnvironmentFile 更新 `ADMIN_TOTP_SECRET`；secret 不得进入 shell history、工单、日志或截图。
3. 在备份完成且服务保持停止时，从 SQLite 删除内部键 `internal.auth.totp_last_step`，避免旧 secret 的成功时间步影响新 secret；不得修改其他 settings。
4. 启动服务，用新 TOTP 完成一次回环 HTTPS 两段登录；确认旧 TOTP 不可用并记录脱敏结果。失败则停服，恢复数据库和 EnvironmentFile 备份。

## 登录锁定恢复

1. 先核查是否存在攻击流量；未排除攻击时不得解除锁定。
2. 停止服务并备份数据库。
3. 仅删除 `admin_login_attempts` 中已确认应解除的 HMAC 记录；表内没有原始用户名或 IP，不得为排障新增明文字段或日志。
4. 重启后验证正确两因子可登录，错误尝试仍在第五次返回 429。记录删除行数，不导出 HMAC 值。

## JWT signing key 轮换

1. 安排停服窗口，停止服务并备份数据库与环境文件。
2. 在单个 SQLite 事务内把所有未撤销 `admin_sessions.revoked_at` 写为当前 UTC 时间；不得只替换 key 而保留旧 session。
3. 离线生成独立的新 `JWT_SIGNING_KEY`，更新 0600 EnvironmentFile；不得与 credential master key、rate-limit key 或 TOTP secret 复用。
4. 重启服务并完成回环 HTTPS 登录；确认轮换前 Cookie 返回 401、新 Cookie 可访问 `/api/me`。失败则停服并按备份回滚，且保持旧 session 已撤销直至安全裁决。
