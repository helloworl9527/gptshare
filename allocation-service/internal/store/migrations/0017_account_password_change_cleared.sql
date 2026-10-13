-- Track when the "password change required" flag was last cleared, so the
-- dashboard can hide accounts the operator already handled. When NULL, the
-- account still shows the "requires password change" flag whenever the
-- computed condition holds (an expired passenger subscription still attached
-- to a GPT account with >3 days of subscription remaining).
ALTER TABLE chatgpt_accounts ADD COLUMN password_change_cleared_at TEXT;
