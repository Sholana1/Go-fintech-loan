-- +goose Up
CREATE SCHEMA identity;

CREATE TABLE identity.customers (
  customer_id    uuid PRIMARY KEY,
  phone          text NOT NULL UNIQUE,           -- E.164
  full_name      text NOT NULL,
  date_of_birth  date NOT NULL,
  -- The BVN is never stored in clear. bvn_hash (keyed HMAC) enforces one
  -- customer per BVN; bvn_ciphertext (AES-256-GCM) is decrypted only to pass
  -- the identifier to a credit bureau with a consent reference.
  bvn_hash       bytea NOT NULL UNIQUE,
  bvn_ciphertext bytea NOT NULL,
  kyc_status     text NOT NULL CHECK (kyc_status IN ('PENDING','VERIFIED','REJECTED')),
  kyc_tier       smallint NOT NULL CHECK (kyc_tier BETWEEN 0 AND 3),
  status         text NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE','BLOCKED')),
  -- NULL until the ledger account has been opened; a background task
  -- completes it if the ledger was unavailable at registration.
  deposit_account_code text,
  created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX customers_pending_account ON identity.customers (created_at) WHERE deposit_account_code IS NULL;

CREATE TABLE identity.credentials (
  customer_id     uuid PRIMARY KEY REFERENCES identity.customers,
  pin_hash        text NOT NULL,                 -- argon2id, self-describing encoding
  failed_attempts int NOT NULL DEFAULT 0,
  locked_until    timestamptz,
  updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE identity.kyc_checks (
  check_id     uuid PRIMARY KEY,
  customer_id  uuid NOT NULL REFERENCES identity.customers,
  provider     text NOT NULL,
  provider_ref text NOT NULL,
  outcome      text NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE identity.staff (
  staff_id      uuid PRIMARY KEY,
  email         text NOT NULL UNIQUE,
  password_hash text NOT NULL,
  roles         text[] NOT NULL,
  status        text NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE','DISABLED')),
  failed_attempts int NOT NULL DEFAULT 0,
  locked_until  timestamptz,
  created_at    timestamptz NOT NULL DEFAULT now()
);

-- Append-only record of security-relevant actions. The application role can
-- insert and read, never update or delete.
CREATE TABLE identity.audit_log (
  audit_id   bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  at         timestamptz NOT NULL DEFAULT now(),
  actor      text NOT NULL,                      -- workload or principal id
  action     text NOT NULL,
  subject_id uuid,
  detail     jsonb NOT NULL DEFAULT '{}'
);

GRANT USAGE ON SCHEMA identity TO identity_app;
GRANT SELECT, INSERT ON ALL TABLES IN SCHEMA identity TO identity_app;
GRANT UPDATE (deposit_account_code, kyc_status, kyc_tier, status) ON identity.customers TO identity_app;
GRANT UPDATE (pin_hash, failed_attempts, locked_until, updated_at) ON identity.credentials TO identity_app;
GRANT UPDATE (failed_attempts, locked_until) ON identity.staff TO identity_app;

-- +goose Down
DROP SCHEMA identity CASCADE;
