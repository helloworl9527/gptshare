-- 终态账号（已封禁 / 订阅已终止）不再监控。
-- 轮询早就选不到它们了（loadDueAccounts 只接未关闭的 authorization_epoch），
-- 但历史数据里有一批账号的 polling_paused 还停在 0：当时的代码只关闭了 epoch，
-- 没有落这个标记，管理端因此把它们显示成"轮询状态：运行中"，与事实矛盾。
-- 这里把标记补齐，pause_reason 沿用 death_type（normal_expiry / abnormal_ban）。
UPDATE accounts
SET polling_paused = 1,
    pause_reason = COALESCE(pause_reason, death_type),
    next_retry_at = NULL
WHERE deleted_at IS NULL
  AND status IN ('dead_normal', 'dead_banned')
  AND polling_paused = 0;
