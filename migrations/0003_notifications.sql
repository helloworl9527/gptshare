ALTER TABLE alert_events RENAME TO alert_events_step06;

CREATE TABLE alert_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE RESTRICT,
    epoch_id INTEGER REFERENCES authorization_epochs(id) ON DELETE RESTRICT,
    event_key TEXT NOT NULL UNIQUE,
    event_type TEXT NOT NULL,
    delivery_status TEXT NOT NULL DEFAULT 'pending'
        CHECK (delivery_status IN ('pending', 'processing', 'recorded_no_channel', 'delivered', 'failed', 'disabled')),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TEXT,
    claimed_at TEXT,
    last_error_code TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

INSERT INTO alert_events
    (id,account_id,epoch_id,event_key,event_type,delivery_status,attempts,next_attempt_at,created_at,updated_at)
SELECT id,account_id,epoch_id,event_key,event_type,delivery_status,attempts,next_attempt_at,created_at,updated_at
FROM alert_events_step06;

DROP TABLE alert_events_step06;
CREATE INDEX alert_events_delivery_idx ON alert_events(delivery_status, next_attempt_at, id);

INSERT INTO settings(key,value,is_secret,key_id,updated_at)
SELECT 'poll_interval',value,0,NULL,updated_at FROM settings
WHERE key='internal.poll_interval_seconds'
ON CONFLICT(key) DO NOTHING;
DELETE FROM settings WHERE key='internal.poll_interval_seconds';

INSERT INTO settings(key,value,is_secret,key_id,updated_at)
VALUES ('near_expiry_days','3',0,NULL,strftime('%Y-%m-%dT%H:%M:%fZ','now'))
ON CONFLICT(key) DO NOTHING;

CREATE TABLE settings_audit (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    at TEXT NOT NULL,
    actor TEXT NOT NULL,
    action TEXT NOT NULL CHECK (action IN ('update', 'secret_set', 'secret_clear')),
    setting_key TEXT NOT NULL,
    configured INTEGER NOT NULL CHECK (configured IN (0, 1))
);
CREATE INDEX settings_audit_at_idx ON settings_audit(at DESC, id DESC);
