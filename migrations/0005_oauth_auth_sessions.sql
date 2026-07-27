CREATE TABLE oauth_auth_sessions (
    id TEXT PRIMARY KEY,
    account_id INTEGER REFERENCES accounts(id) ON DELETE CASCADE,
    enc_session BLOB NOT NULL,
    credential_key_id TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('pending', 'exchanging', 'authorized', 'expired', 'failed')),
    expires_at TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX oauth_auth_expiry_idx ON oauth_auth_sessions(state, expires_at);
