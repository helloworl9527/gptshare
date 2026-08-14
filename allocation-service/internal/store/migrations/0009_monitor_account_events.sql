ALTER TABLE chatgpt_accounts ADD COLUMN monitor_sync_version INTEGER NOT NULL DEFAULT 0 CHECK (monitor_sync_version >= 0);
ALTER TABLE chatgpt_accounts ADD COLUMN monitor_plan TEXT NOT NULL DEFAULT 'unknown';

CREATE TABLE monitor_account_events (
    event_id TEXT PRIMARY KEY,
    monitor_account_id TEXT NOT NULL,
    account_version INTEGER NOT NULL CHECK (account_version > 0),
    event_type TEXT NOT NULL,
    disposition TEXT NOT NULL CHECK (disposition IN ('applied','stale')),
    processed_at TEXT NOT NULL
);

CREATE INDEX monitor_account_events_account_idx
    ON monitor_account_events(monitor_account_id, account_version DESC);
