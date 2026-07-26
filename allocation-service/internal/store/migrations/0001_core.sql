CREATE TABLE chatgpt_accounts (
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
        CHECK (status IN ('available', 'unknown_monitor', 'full', 'expired', 'banned', 'disabled')),
    last_allocated_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    CHECK (current_allocations <= max_concurrent_users),
    CHECK (datetime(account_expiry) <= datetime(created_at, '+30 days'))
);

CREATE INDEX chatgpt_accounts_status_expiry_idx ON chatgpt_accounts(status, account_expiry);
CREATE INDEX chatgpt_accounts_monitor_account_idx ON chatgpt_accounts(monitor_account_id);

CREATE TABLE cards (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    code_hash BLOB NOT NULL UNIQUE,
    code_suffix TEXT NOT NULL,
    duration_days INTEGER NOT NULL CHECK (duration_days IN (7, 14, 30, 90)),
    status TEXT NOT NULL DEFAULT 'unused'
        CHECK (status IN ('unused', 'redeemed', 'expired', 'revoked')),
    redeemed_at TEXT,
    expires_at TEXT,
    revoked_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE INDEX cards_status_expires_idx ON cards(status, expires_at);

CREATE TABLE allocations (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    card_id INTEGER NOT NULL REFERENCES cards(id) ON DELETE RESTRICT,
    account_id INTEGER NOT NULL REFERENCES chatgpt_accounts(id) ON DELETE RESTRICT,
    allocated_at TEXT NOT NULL,
    valid_until TEXT NOT NULL,
    grace_until TEXT,
    replaced_at TEXT,
    replacement_reason TEXT,
    allocation_state TEXT NOT NULL
        CHECK (allocation_state IN ('primary', 'grace', 'replaced', 'expired', 'revoked')),
    active INTEGER NOT NULL CHECK (active IN (0, 1)),
    superseded_by_allocation_id INTEGER REFERENCES allocations(id) ON DELETE RESTRICT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    CHECK (
        (allocation_state = 'primary' AND active = 1 AND grace_until IS NULL AND superseded_by_allocation_id IS NULL)
        OR
        (allocation_state = 'grace' AND active = 1 AND grace_until IS NOT NULL AND superseded_by_allocation_id IS NOT NULL)
        OR
        (allocation_state IN ('replaced', 'expired', 'revoked') AND active = 0)
    ),
    CHECK (allocation_state != 'grace' OR datetime(grace_until) > datetime(allocated_at))
);

CREATE UNIQUE INDEX allocations_active_primary_card_uq
    ON allocations(card_id) WHERE active = 1 AND allocation_state = 'primary';
CREATE UNIQUE INDEX allocations_active_grace_card_uq
    ON allocations(card_id) WHERE active = 1 AND allocation_state = 'grace';
CREATE INDEX allocations_card_active_idx ON allocations(card_id, active);
CREATE INDEX allocations_account_active_idx ON allocations(account_id, active);
CREATE INDEX allocations_superseded_idx ON allocations(superseded_by_allocation_id);

CREATE TRIGGER allocations_terminal_state_no_capacity_leak_insert
BEFORE INSERT ON allocations
WHEN NEW.allocation_state IN ('replaced', 'expired', 'revoked') AND NEW.active != 0
BEGIN
    SELECT RAISE(ABORT, 'terminal allocations release account capacity');
END;

CREATE TRIGGER allocations_terminal_state_no_capacity_leak_update
BEFORE UPDATE ON allocations
WHEN NEW.allocation_state IN ('replaced', 'expired', 'revoked') AND NEW.active != 0
BEGIN
    SELECT RAISE(ABORT, 'terminal allocations release account capacity');
END;

CREATE TABLE replacement_history (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    card_id INTEGER NOT NULL REFERENCES cards(id) ON DELETE RESTRICT,
    old_account_id INTEGER NOT NULL REFERENCES chatgpt_accounts(id) ON DELETE RESTRICT,
    new_account_id INTEGER NOT NULL REFERENCES chatgpt_accounts(id) ON DELETE RESTRICT,
    reason TEXT NOT NULL,
    detected_at TEXT NOT NULL,
    replaced_at TEXT NOT NULL,
    grace_until TEXT,
    operator TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE TABLE captcha_challenges (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    card_id INTEGER REFERENCES cards(id) ON DELETE CASCADE,
    challenge_hash BLOB NOT NULL,
    answer_hash BLOB NOT NULL,
    expires_at TEXT NOT NULL,
    verified_at TEXT,
    created_at TEXT NOT NULL
);

CREATE TABLE rate_limit_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    scope TEXT NOT NULL,
    subject_hash BLOB NOT NULL,
    event_type TEXT NOT NULL,
    occurred_at TEXT NOT NULL,
    expires_at TEXT NOT NULL
);

CREATE INDEX rate_limit_events_subject_idx ON rate_limit_events(scope, subject_hash, occurred_at DESC);
CREATE INDEX rate_limit_events_expiry_idx ON rate_limit_events(expires_at);

CREATE TABLE audit_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    actor_type TEXT NOT NULL CHECK (actor_type IN ('admin', 'card', 'system')),
    actor_hash BLOB,
    action TEXT NOT NULL,
    target_type TEXT NOT NULL,
    target_id INTEGER,
    metadata_json TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL
);

CREATE INDEX audit_events_created_idx ON audit_events(created_at DESC);

CREATE TABLE monitor_sync_runs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    state TEXT NOT NULL CHECK (state IN ('running', 'completed', 'failed')),
    started_at TEXT NOT NULL,
    finished_at TEXT,
    accounts_total INTEGER NOT NULL DEFAULT 0 CHECK (accounts_total >= 0),
    accounts_ok INTEGER NOT NULL DEFAULT 0 CHECK (accounts_ok >= 0),
    accounts_failed INTEGER NOT NULL DEFAULT 0 CHECK (accounts_failed >= 0),
    error_code TEXT
);

CREATE INDEX monitor_sync_runs_started_idx ON monitor_sync_runs(started_at DESC);
