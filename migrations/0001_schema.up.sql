CREATE TABLE accounts (
  id            BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  owner_id      BIGINT UNSIGNED NOT NULL,
  currency      CHAR(3) NOT NULL,
  status        ENUM('active','frozen') NOT NULL DEFAULT 'active',
  balance_cents BIGINT NOT NULL DEFAULT 0,
  version       BIGINT NOT NULL DEFAULT 0,
  created_at    TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  updated_at    TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  CONSTRAINT uq_owner_currency UNIQUE (owner_id, currency),
  CONSTRAINT ck_balance_nonneg CHECK (balance_cents >= 0),
  INDEX idx_accounts_id_currency (id, currency)
) ENGINE=InnoDB;

CREATE TABLE ledger_entries (
  id             BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  transfer_id    BIGINT UNSIGNED NOT NULL,
  account_id     BIGINT UNSIGNED NOT NULL,
  direction      ENUM('debit','credit') NOT NULL,
  amount_cents   BIGINT NOT NULL,
  balance_after  BIGINT NOT NULL,
  created_at     TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  CONSTRAINT uq_transfer_account_dir UNIQUE (transfer_id, account_id, direction),
  INDEX idx_ledger_account (account_id, id)
) ENGINE=InnoDB;

CREATE TABLE transfers (
  id          BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  amount_cents BIGINT NOT NULL,
  currency    CHAR(3) NOT NULL,
  created_at  TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
) ENGINE=InnoDB;

CREATE TABLE idempotency_keys (
  id            BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  idem_key      VARCHAR(255) NOT NULL,
  scope         VARCHAR(64) NOT NULL,
  request_hash  CHAR(64) NOT NULL,
  transfer_id   BIGINT UNSIGNED NULL,
  state         ENUM('processing','completed','failed') NOT NULL,
  result_code   VARCHAR(32) NULL,
  created_at    TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  expires_at    TIMESTAMP(6) NOT NULL,
  CONSTRAINT uq_idem UNIQUE (scope, idem_key)
) ENGINE=InnoDB;
