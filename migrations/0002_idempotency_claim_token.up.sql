ALTER TABLE idempotency_keys
  ADD COLUMN claim_token CHAR(32) NULL;
