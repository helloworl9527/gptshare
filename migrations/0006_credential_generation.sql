ALTER TABLE accounts ADD COLUMN credential_generation INTEGER NOT NULL DEFAULT 1
    CHECK (credential_generation >= 1);

UPDATE accounts SET credential_generation=1 WHERE credential_generation<1;
