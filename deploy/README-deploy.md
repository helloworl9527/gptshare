# 本地构建与生产等价部署手册

本目录只准备部署物料。STEP-10 不上传服务器、不发布生产；只有 STEP-11 本地证据全部通过，且用户针对具体主机、维护窗口、备份点明确回复“批准上传”后，才可执行远程步骤。

## 目标布局与权限

- 服务用户：独立非登录用户 `chatgpt-monitor`，不得以 root 运行应用。
- release：`/opt/chatgpt-monitor/releases/<release-id>`，root:chatgpt-monitor，目录 0750、普通文件 0640、两个二进制和脚本 0750。
- current：`/opt/chatgpt-monitor/current` 原子符号链接；Nginx Web 根只指向 `current/web`，不得包含 `bin/evidence-review`。
- 配置：`/etc/chatgpt-monitor/server.env` 与 `backup.pass` 均 root:chatgpt-monitor 0600；密钥必须独立生成，不得把 example 占位符投入运行。
- 数据：`/var/lib/chatgpt-monitor` 与 `/var/backups/chatgpt-monitor` 0700，SQLite 主库/WAL/SHM/加密备份 0600。
- 后端只监听 `127.0.0.1:8080`；防火墙只放行 80/443，不放行 8080。Nginx 覆盖而非追加 `X-Real-IP`/`X-Forwarded-For`，应用仅信任回环代理。

## 本地构建与校验

```sh
make release
openssl dgst -sha256 artifacts/chatgpt-monitor-linux-amd64.tar.gz
cat artifacts/chatgpt-monitor-linux-amd64.tar.gz.sha256
```

构建包含 linux/amd64 后端、`evidence-review`、前端静态文件、migration、部署配置与恢复脚本。上传前必须在本地核对 archive checksum，并在目标主机临时目录再次核对；checksum 不一致立即停止。

STEP-11 最终本地 release：

- 文件：`artifacts/chatgpt-monitor-linux-amd64.tar.gz`
- SHA-256：`381fcdee36c9196560c3786c773e9ab6e7883205b30f25f7e7c12d795581f094`
- 该值 supersede STEP-10 的 `efdf3304062abea61f99da2437267ac53019a3991d7503c11ec0db296565be0d`。变化来源仅为 STEP-11 回归中登记的 SQLite busy 有界退避修复，以及不进入运行二进制的文档/测试材料；旧包不再是最终交付物。
- 每次重新构建都会产生新 archive，必须同步更新 checksum 和实施台账；不得只修改记录而不核对实际文件。

STEP-12 revision 8 本地 release：

- 文件：`artifacts/chatgpt-monitor-linux-amd64.tar.gz`
- SHA-256：`afdae50ca567e1581cc950ecd86388fdc8517e4fdd746336e29e1075cbb386e5`
- 包内 `schema-version=5`，且包含 `migrations/0005_oauth_auth_sessions.sql`。
- 该值 supersede STEP-11 的 `381fcdee36c9196560c3786c773e9ab6e7883205b30f25f7e7c12d795581f094`。变化来源仅为 STEP-12 增强批次 1：账号邮箱 nullable 字段、邮箱默认标签/展示/前端搜索，以及 schema 3→4 升级；生产上传仍需另行授权。

## Nginx 与 TLS

`deploy/nginx.conf` 使用 `__...__` 部署占位符。渲染时必须提供 server name、Web root、证书/私钥、日志、pid、mime.types、proxy params 及监听端口。生产监听 80/443；本地测试可用回环高端口和自签证书。

生产证书建议由 ACME 客户端写入 `/etc/letsencrypt/live/<host>/`，Nginx 只读。续期后先 `nginx -t` 再 reload；每日检查证书剩余有效期，≤21 天或续期任务失败须告警。自签证书只用于本机，不得上传生产。

配置强制 TLS 1.2/1.3、HTTP→HTTPS 308、登录/API 分层限流、64 KiB body、超时、隐藏版本、安全响应头与无 `unsafe-inline`/`unsafe-eval` 的 CSP。不配置 IP 白名单。

## 原子发布

1. 在目标主机创建 `/opt/chatgpt-monitor/releases/<release-id>.incoming`，上传 archive 和 checksum 到该临时目录。
2. 校验 checksum，解包为新 release；核对 owner/mode，确认 `evidence-review` 不在 Web 根且非服务组用户不可执行。
3. 停止服务或进入短暂停写窗口；若数据库已存在，systemd `ExecStartPre` 会执行迁移前加密备份。另手工记录备份文件、checksum 与时间。
4. 用 `scripts/atomic-switch.sh <new-release> /opt/chatgpt-monitor/current` 原子切换。
5. `systemd-analyze verify deploy/chatgpt-monitor.service`、`systemctl daemon-reload`、start/restart，确认服务以 `chatgpt-monitor` 用户运行。
6. 运行 HTTPS health、无会话 `/api/me`=401、登录+TOTP+CSRF smoke；失败立即停止流量并进入回滚分支。

## 备份与恢复

```sh
scripts/backup-sqlite.sh /var/lib/chatgpt-monitor/chatgpt-monitor.db /var/backups/chatgpt-monitor /etc/chatgpt-monitor/backup.pass
scripts/restore-sqlite.sh /var/backups/chatgpt-monitor/chatgpt-monitor-<utc>.sqlite.enc /var/lib/chatgpt-monitor/restore/chatgpt-monitor.db /etc/chatgpt-monitor/backup.pass
```

备份先执行 WAL FULL checkpoint，再用 SQLite 在线 backup API 生成一致副本，`quick_check` 通过后用 AES-256-CBC/PBKDF2 加密；passphrase 只经 0600 文件读取，不进入 argv 值、日志或 release。恢复拒绝覆盖现有目标，必须恢复到空路径并核对 quick_check、账号数、授权周期数、状态日志数和 schema version，再在停服窗口替换。

## 应用与数据库回滚

- 若 previous/current 的 `schema-version` 相同，可用 `rollback-release.sh` 原子切回上一 release，再做 health/smoke。
- 若 schema version 不同，脚本 exit 42 并输出 `db_restore_required`；不得声称只切代码即可恢复。保持流量停止，按上节把已验证的迁移前备份恢复到新路径，核对数据后再切换 DB 与上一 release。
- 每次恢复都记录恢复点、丢失窗口、执行人、checksum 和健康检查结果。

## Linux 必验项

macOS 无 systemd，不能替代以下证据：在 Ubuntu VM/等价 Linux 上运行 `systemd-analyze verify`、启动单元、检查 User/Group/UMask/StateDirectoryMode/EnvironmentFile 权限、确认 `/proc/<pid>/status` 无特权、非授权用户不能执行 `evidence-review`，并从非回环地址确认 8080 不可达。本地未具备该环境时必须明确标记为未执行，不能冒充通过。

## 供应链例外

- `GO-2026-5932`：`golang.org/x/crypto/openpgp` 已停止维护且无修复版本。2026-07-23 的 `govulncheck ./...` 显示本项目代码受影响数为 0，导入包受影响数为 0；告警仅来自间接要求的模块，项目未导入或调用 `openpgp`。
- 处置：保留当前最小依赖面，不新增 `openpgp` 调用；每次发布前重跑 `govulncheck`，一旦变为可达或上游出现替代/修复版本即停止发布并升级或移除依赖。
- 负责人：服务维护人。下次定期复查：2026-08-06；任何生产上传授权前必须提前复查。
