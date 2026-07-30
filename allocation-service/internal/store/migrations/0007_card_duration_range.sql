-- migrate:foreign-keys-off
CREATE TABLE cards_next (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    code_hash BLOB NOT NULL UNIQUE,
    code_suffix TEXT NOT NULL,
    duration_days INTEGER NOT NULL
        CHECK ((duration_days BETWEEN 1 AND 30) OR duration_days = 90),
    status TEXT NOT NULL DEFAULT 'unused'
        CHECK (status IN ('unused', 'redeemed', 'expired', 'revoked')),
    redeemed_at TEXT,
    expires_at TEXT,
    revoked_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    encrypted_code_key_id TEXT,
    encrypted_code BLOB
);

INSERT INTO cards_next (
    id, code_hash, code_suffix, duration_days, status,
    redeemed_at, expires_at, revoked_at, created_at, updated_at,
    encrypted_code_key_id, encrypted_code
)
SELECT
    id, code_hash, code_suffix, duration_days, status,
    redeemed_at, expires_at, revoked_at, created_at, updated_at,
    encrypted_code_key_id, encrypted_code
FROM cards;

DROP TABLE cards;
ALTER TABLE cards_next RENAME TO cards;

CREATE INDEX cards_status_expires_idx ON cards(status, expires_at);
