-- migrate:foreign-keys-off
CREATE TABLE card_groups (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at TEXT NOT NULL
);

CREATE TABLE cards_next (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    code_hash BLOB NOT NULL UNIQUE,
    code_suffix TEXT NOT NULL,
    duration_days INTEGER NOT NULL CHECK (duration_days >= 1),
    status TEXT NOT NULL DEFAULT 'unused'
        CHECK (status IN ('unused', 'redeemed', 'expired', 'revoked', 'consumed')),
    redeemed_at TEXT,
    expires_at TEXT,
    revoked_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    encrypted_code_key_id TEXT,
    encrypted_code BLOB,
    group_id INTEGER REFERENCES card_groups(id) ON DELETE RESTRICT
);
INSERT INTO cards_next (id,code_hash,code_suffix,duration_days,status,redeemed_at,expires_at,revoked_at,created_at,updated_at,encrypted_code_key_id,encrypted_code)
SELECT id,code_hash,code_suffix,duration_days,status,redeemed_at,expires_at,revoked_at,created_at,updated_at,encrypted_code_key_id,encrypted_code FROM cards;
DROP TABLE cards;
ALTER TABLE cards_next RENAME TO cards;
CREATE INDEX cards_status_expires_idx ON cards(status, expires_at);
CREATE INDEX cards_group_idx ON cards(group_id);

CREATE TABLE card_operations (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    action TEXT NOT NULL CHECK (action IN ('renew', 'different')),
    target_card_id INTEGER NOT NULL REFERENCES cards(id) ON DELETE RESTRICT,
    source_card_id INTEGER NOT NULL UNIQUE REFERENCES cards(id) ON DELETE RESTRICT,
    added_days INTEGER NOT NULL CHECK (added_days >= 1),
    before_expires_at TEXT,
    after_expires_at TEXT,
    result_json TEXT NOT NULL,
    created_at TEXT NOT NULL,
    CHECK (target_card_id != source_card_id)
);
CREATE INDEX card_operations_target_idx ON card_operations(target_card_id,id);
