-- migrate:foreign-keys-off
-- 新增 suspected 状态：疑似被封禁的账号不再参与分配，但存量顾客保持不动。
CREATE TABLE chatgpt_accounts_next (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    display_username TEXT NOT NULL,
    display_password_secret BLOB NOT NULL,
    display_password_key_id TEXT NOT NULL,
    display_2fa_secret BLOB NOT NULL,
    display_2fa_key_id TEXT NOT NULL,
    account_expiry TEXT NOT NULL,
    max_concurrent_users INTEGER NOT NULL CHECK (max_concurrent_users > 0),
    current_allocations INTEGER NOT NULL DEFAULT 0 CHECK (current_allocations >= 0),
    monitor_account_id TEXT UNIQUE,
    monitor_status TEXT NOT NULL DEFAULT 'unknown_monitor'
        CHECK (monitor_status IN ('alive', 'unknown', 'dead_normal', 'dead_banned', 'unknown_monitor', 'not_found')),
    status TEXT NOT NULL DEFAULT 'available'
        CHECK (status IN ('available', 'pending_credentials', 'unknown_monitor', 'full', 'expired', 'banned', 'suspected', 'disabled')),
    last_allocated_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    source_url_key_id TEXT,
    source_url_secret BLOB,
    archived_at TEXT,
    monitor_sync_version INTEGER NOT NULL DEFAULT 0 CHECK (monitor_sync_version >= 0),
    monitor_plan TEXT NOT NULL DEFAULT 'unknown'
);

INSERT INTO chatgpt_accounts_next (
    id, display_username, display_password_secret, display_password_key_id,
    display_2fa_secret, display_2fa_key_id, account_expiry, max_concurrent_users,
    current_allocations, monitor_account_id, monitor_status, status,
    last_allocated_at, created_at, updated_at, source_url_key_id, source_url_secret,
    archived_at, monitor_sync_version, monitor_plan
)
SELECT
    id, display_username, display_password_secret, display_password_key_id,
    display_2fa_secret, display_2fa_key_id, account_expiry, max_concurrent_users,
    current_allocations, monitor_account_id, monitor_status, status,
    last_allocated_at, created_at, updated_at, source_url_key_id, source_url_secret,
    archived_at, monitor_sync_version, monitor_plan
FROM chatgpt_accounts;

DROP TABLE chatgpt_accounts;
ALTER TABLE chatgpt_accounts_next RENAME TO chatgpt_accounts;

CREATE INDEX chatgpt_accounts_status_expiry_idx ON chatgpt_accounts(status, account_expiry);
CREATE INDEX chatgpt_accounts_monitor_account_idx ON chatgpt_accounts(monitor_account_id);
CREATE INDEX chatgpt_accounts_archived_idx ON chatgpt_accounts(archived_at);
