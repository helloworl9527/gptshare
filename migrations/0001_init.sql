CREATE TABLE accounts (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    provider_account_id TEXT NOT NULL,
    label TEXT NOT NULL DEFAULT '',
    token_type TEXT NOT NULL CHECK (token_type IN ('access', 'refresh', 'session', 'device')),
    enc_credentials BLOB NOT NULL,
    credential_key_id TEXT NOT NULL,
    plan TEXT NOT NULL DEFAULT 'unknown' CHECK (plan IN ('free', 'plus', 'team', 'unknown')),
    raw_plan TEXT NOT NULL DEFAULT '',
    current_expiry TEXT,
    auth_expiry TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'alive' CHECK (status IN ('alive', 'dead_normal', 'dead_banned')),
    last_alive_at TEXT,
    dead_at TEXT,
    death_type TEXT CHECK (death_type IS NULL OR death_type IN ('normal_expiry', 'abnormal_ban')),
    banned_survival_days REAL,
    import_time TEXT NOT NULL,
    last_check_state TEXT NOT NULL DEFAULT 'pending',
    last_check_error_code TEXT,
    next_retry_at TEXT,
    deleted_at TEXT,
    updated_at TEXT NOT NULL
);

CREATE UNIQUE INDEX accounts_provider_active_uq
    ON accounts(provider_account_id) WHERE deleted_at IS NULL;
CREATE INDEX accounts_poll_due_idx
    ON accounts(status, next_retry_at) WHERE deleted_at IS NULL;

CREATE TABLE authorization_epochs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE RESTRICT,
    started_at TEXT NOT NULL,
    auth_expiry TEXT NOT NULL,
    ended_at TEXT,
    terminal_status TEXT CHECK (terminal_status IS NULL OR terminal_status IN ('dead_normal', 'dead_banned')),
    dead_at TEXT,
    banned_survival_days REAL
);
CREATE INDEX authorization_epochs_account_idx
    ON authorization_epochs(account_id, started_at DESC);
CREATE UNIQUE INDEX authorization_epochs_active_uq
    ON authorization_epochs(account_id) WHERE ended_at IS NULL;

CREATE TABLE poll_runs (
    id TEXT PRIMARY KEY,
    started_at TEXT NOT NULL,
    finished_at TEXT,
    state TEXT NOT NULL CHECK (state IN ('running', 'completed', 'failed', 'cancelled')),
    accounts_total INTEGER NOT NULL DEFAULT 0 CHECK (accounts_total >= 0),
    accounts_ok INTEGER NOT NULL DEFAULT 0 CHECK (accounts_ok >= 0),
    accounts_failed INTEGER NOT NULL DEFAULT 0 CHECK (accounts_failed >= 0),
    error_code TEXT
);
CREATE INDEX poll_runs_started_idx ON poll_runs(started_at DESC);

CREATE TABLE status_change_log (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE RESTRICT,
    epoch_id INTEGER REFERENCES authorization_epochs(id) ON DELETE RESTRICT,
    at TEXT NOT NULL,
    field TEXT NOT NULL CHECK (field IN ('status', 'plan', 'current_expiry', 'last_check_state')),
    from_value TEXT,
    to_value TEXT,
    evidence_code TEXT NOT NULL,
    evidence_level TEXT NOT NULL CHECK (evidence_level IN ('live_verified', 'contract_verified_live_pending', 'unverified')),
    evidence_signature TEXT NOT NULL,
    review_decision TEXT CHECK (review_decision IS NULL OR review_decision IN ('pending', 'confirmed', 'rejected')),
    reviewed_at TEXT,
    run_id TEXT REFERENCES poll_runs(id) ON DELETE SET NULL,
    CHECK (((review_decision IS NULL OR review_decision = 'pending') AND reviewed_at IS NULL) OR
           (review_decision IN ('confirmed', 'rejected') AND reviewed_at IS NOT NULL))
);
CREATE INDEX status_change_account_at_idx ON status_change_log(account_id, at DESC);
CREATE INDEX status_change_review_idx
    ON status_change_log(evidence_level, review_decision, at)
    WHERE evidence_level != 'live_verified';

CREATE TABLE alert_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE RESTRICT,
    epoch_id INTEGER REFERENCES authorization_epochs(id) ON DELETE RESTRICT,
    event_key TEXT NOT NULL UNIQUE,
    event_type TEXT NOT NULL,
    delivery_status TEXT NOT NULL DEFAULT 'pending'
        CHECK (delivery_status IN ('pending', 'processing', 'delivered', 'failed', 'disabled')),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX alert_events_delivery_idx ON alert_events(delivery_status, next_attempt_at);

CREATE TABLE device_auth_sessions (
    id TEXT PRIMARY KEY,
    account_id INTEGER REFERENCES accounts(id) ON DELETE CASCADE,
    enc_device_code BLOB NOT NULL,
    credential_key_id TEXT NOT NULL,
    interval_seconds INTEGER NOT NULL CHECK (interval_seconds > 0),
    expires_at TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('pending', 'authorized', 'expired', 'failed')),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX device_auth_expiry_idx ON device_auth_sessions(state, expires_at);

CREATE TABLE settings (
    key TEXT PRIMARY KEY,
    value BLOB NOT NULL,
    is_secret INTEGER NOT NULL DEFAULT 0 CHECK (is_secret IN (0, 1)),
    key_id TEXT,
    updated_at TEXT NOT NULL,
    CHECK ((is_secret = 0 AND key_id IS NULL) OR (is_secret = 1 AND key_id IS NOT NULL))
);

CREATE TABLE admin_login_attempts (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    username_hmac BLOB NOT NULL,
    client_ip_hmac BLOB NOT NULL,
    result TEXT NOT NULL CHECK (result IN ('success', 'failure', 'locked')),
    attempted_at TEXT NOT NULL,
    expires_at TEXT NOT NULL
);
CREATE INDEX admin_login_attempts_rate_idx
    ON admin_login_attempts(client_ip_hmac, attempted_at DESC);
CREATE INDEX admin_login_attempts_expiry_idx ON admin_login_attempts(expires_at);

CREATE TABLE admin_sessions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    jti_hash BLOB NOT NULL UNIQUE,
    issued_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    revoked_at TEXT
);
CREATE INDEX admin_sessions_expiry_idx ON admin_sessions(expires_at, revoked_at);
