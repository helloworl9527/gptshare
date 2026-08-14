ALTER TABLE accounts ADD COLUMN sync_version INTEGER NOT NULL DEFAULT 0 CHECK (sync_version >= 0);

CREATE TABLE allocation_account_outbox (
    event_id TEXT PRIMARY KEY,
    account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE RESTRICT,
    account_version INTEGER NOT NULL CHECK (account_version > 0),
    event_type TEXT NOT NULL CHECK (event_type IN ('account.created','account.updated','account.dead_banned','account.reauthorized')),
    payload_json TEXT NOT NULL CHECK (json_valid(payload_json)),
    delivery_status TEXT NOT NULL DEFAULT 'pending'
        CHECK (delivery_status IN ('pending','processing','retrying','delivered','failed')),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TEXT,
    last_error_code TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    delivered_at TEXT,
    UNIQUE(account_id, account_version)
);

CREATE INDEX allocation_account_outbox_due_idx
    ON allocation_account_outbox(delivery_status, next_attempt_at, created_at);
CREATE INDEX allocation_account_outbox_account_idx
    ON allocation_account_outbox(account_id, account_version DESC);
