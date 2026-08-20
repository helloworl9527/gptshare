-- 连续"账号级拒绝"计数，用于疑似封禁判定。
-- 只统计 401/403/token_revoked/credential_revoked/account_disabled 这类账号级拒绝；
-- 需要重新授权（凭据过期/失效）不计入，任何一次成功轮询清零。
ALTER TABLE accounts ADD COLUMN denial_streak INTEGER NOT NULL DEFAULT 0 CHECK (denial_streak >= 0);
ALTER TABLE accounts ADD COLUMN denial_streak_started_at TEXT;
ALTER TABLE accounts ADD COLUMN suspected_banned_at TEXT;

CREATE INDEX accounts_suspected_idx ON accounts(suspected_banned_at);
