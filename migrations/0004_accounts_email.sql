-- Migration 0004: Add nullable email field to accounts table
-- This is an additive-only migration for revision 8 / STEP-12 enhancement batch 1

ALTER TABLE accounts ADD COLUMN email TEXT NULL;

-- No index on email per STEP-12 specification
-- NULL means email not available or not yet backfilled
-- Empty string is normalized to NULL
