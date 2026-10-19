# 每 7 天清理过期监控账号

此功能运行在服务器 `vitals` 后端，不改动 Cloudflare Pages/Workers 兑换页。

- 首次启用建立持久化计划，7 天后首次执行；之后每 7 天执行一次。后台每小时检查是否到期，正常运行下触发误差不超过一小时。
- 重启不重置计划；停机错过的任务在启动后补跑。数据库故障不推进计划，下一小时重试。任务有持久化防重租约，崩溃后租约在 30 分钟后过期，下次检查可重试。
- 判断会员 `current_expiry`，不是凭据 `auth_expiry`。无会员到期时间但已经 `dead_normal` 的账号也属于候选。已有未来会员到期时间的账号即使状态未同步也不会清理。
- 每个账号删除前持有监控数据库写锁，重新检查会员到期、未投递的分配同步事件，并由分配域确认没有任何 active 分配（包括宽限期）。不能确认安全就跳过。
- 分配域先归档对应账号，防止新分配及晚到同步事件重新启用；仍承载客户的过期账号交给已有换号任务迁移，周清理绝不强删客户分配。
- 分配域仍记录未来到期时间的未归档账号会跳过；仅存在于监控侧的账号会保留分配域归档标记，阻止旧快照重新创建可分配账号。
- 监控侧仅软删除：移出监控列表，擦除加密凭据、清理临时授权会话并结束开放授权周期。卡密、续费记录、分配记录、换号记录、账号行及监控历史均保留。
- 每次完成记录清理/跳过数量及下次时间；账号级审计记录写入 `account_cleanup_log`。日志不输出邮箱、密码、RT 或其他凭据。
- 这是本系统记录清理，不会自动登出 ChatGPT 已登录设备或修改账号密码。

## 发布

监控数据库增加迁移 `0012_expired_account_cleanup.sql`；分配数据库仍是 schema 18。须同时更新 `vitals`、`vitals-migrate` 和 `/app/migrations/`，不要仅替换二进制。

使用 `deploy/Dockerfile.card-actions` 构建服务器镜像，可保留服务器原兑换页；不要为此功能重新构建 Cloudflare 前端。生产切换前备份两个数据库，在备份副本验证迁移，确认后台鉴权与兑换功能正常后再发布。

## 检查计划（仅输出统计，不查询凭据）

```sql
SELECT name, next_run_at, last_run_at, last_error_code,
       last_removed_count, last_skipped_count
FROM maintenance_jobs WHERE name = 'expired-account-cleanup';

SELECT count(*) FROM account_cleanup_log;
```

不开放公网清理 API，不使用手工修改 `next_run_at` 来强行触发生产清理。
