CREATE TABLE maintenance_jobs (
    name TEXT PRIMARY KEY,
    next_run_at TEXT NOT NULL,
    last_run_at TEXT,
    lease_token TEXT,
    lease_until TEXT,
    last_error_code TEXT,
    last_removed_count INTEGER NOT NULL DEFAULT 0,
    last_skipped_count INTEGER NOT NULL DEFAULT 0,
    CHECK ((lease_token IS NULL) = (lease_until IS NULL))
);

-- Keep an audit receipt; never delete the account or its monitoring history.
CREATE TABLE account_cleanup_log (
    account_id INTEGER PRIMARY KEY REFERENCES accounts(id) ON DELETE RESTRICT,
    removed_at TEXT NOT NULL,
    reason TEXT NOT NULL CHECK (reason = 'expired_weekly_cleanup')
);
