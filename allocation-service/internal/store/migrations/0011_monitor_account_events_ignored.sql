-- migrate:foreign-keys-off
CREATE TABLE monitor_account_events_next (
    event_id TEXT PRIMARY KEY,
    monitor_account_id TEXT NOT NULL,
    account_version INTEGER NOT NULL CHECK (account_version > 0),
    event_type TEXT NOT NULL,
    disposition TEXT NOT NULL CHECK (disposition IN ('applied', 'stale', 'ignored_archived')),
    processed_at TEXT NOT NULL
);

INSERT INTO monitor_account_events_next (
    event_id, monitor_account_id, account_version, event_type, disposition, processed_at
)
SELECT
    event_id, monitor_account_id, account_version, event_type, disposition, processed_at
FROM monitor_account_events;

DROP TABLE monitor_account_events;
ALTER TABLE monitor_account_events_next RENAME TO monitor_account_events;

CREATE INDEX monitor_account_events_account_idx
    ON monitor_account_events(monitor_account_id, account_version DESC);
