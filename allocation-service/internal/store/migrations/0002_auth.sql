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
