ALTER TABLE chatgpt_accounts ADD COLUMN archived_at TEXT;

CREATE INDEX chatgpt_accounts_archived_idx ON chatgpt_accounts(archived_at);
