-- 拆除疑似封禁机制。
-- 观察期的结论是它没有信号：三个样本全部是误报（凭据被吊销而非账号被封），
-- 六次真实封禁则全部由 account_disabled 直接判定，一次都没经过这个计数器。
-- 保留一套只会产生假阳性的判据，比没有更糟：它会把正常账号挡在分配池外。
-- 判定封禁只留一条路径 —— 上游明说 account_disabled / account_deactivated。
DROP INDEX IF EXISTS accounts_suspected_idx;
ALTER TABLE accounts DROP COLUMN suspected_banned_at;
ALTER TABLE accounts DROP COLUMN denial_streak_started_at;
ALTER TABLE accounts DROP COLUMN denial_streak;

DELETE FROM settings WHERE key = 'denial_suspect_threshold';
