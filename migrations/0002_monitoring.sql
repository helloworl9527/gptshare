ALTER TABLE accounts ADD COLUMN polling_paused INTEGER NOT NULL DEFAULT 0 CHECK (polling_paused IN (0, 1));
ALTER TABLE accounts ADD COLUMN pause_reason TEXT;
ALTER TABLE accounts ADD COLUMN pending_evidence_signature TEXT;
ALTER TABLE accounts ADD COLUMN pending_detected_at TEXT;

ALTER TABLE poll_runs ADD COLUMN trigger_type TEXT NOT NULL DEFAULT 'scheduled'
    CHECK (trigger_type IN ('scheduled', 'manual'));
ALTER TABLE poll_runs ADD COLUMN account_id INTEGER REFERENCES accounts(id) ON DELETE SET NULL;
ALTER TABLE poll_runs ADD COLUMN accounts_skipped INTEGER NOT NULL DEFAULT 0 CHECK (accounts_skipped >= 0);
ALTER TABLE poll_runs ADD COLUMN error_counts_json TEXT NOT NULL DEFAULT '{}';

ALTER TABLE status_change_log ADD COLUMN review_operator TEXT;
ALTER TABLE status_change_log ADD COLUMN review_reason TEXT;

CREATE INDEX accounts_poll_ready_idx
    ON accounts(polling_paused, next_retry_at, id)
    WHERE deleted_at IS NULL;
CREATE INDEX poll_runs_account_started_idx ON poll_runs(account_id, started_at DESC);
CREATE INDEX accounts_pending_evidence_idx
    ON accounts(pending_evidence_signature)
    WHERE polling_paused = 1;

INSERT INTO settings(key,value,is_secret,key_id,updated_at)
VALUES ('internal.poll_interval_seconds','3600',0,NULL,strftime('%Y-%m-%dT%H:%M:%fZ','now'))
ON CONFLICT(key) DO NOTHING;
