-- +goose Up
-- Repayments made from outside the bank (card, transfer from another bank)
-- through a payment provider.
--
-- The row is created before the customer pays, with a reference we choose.
-- Nothing is credited on the customer's or a webhook's say-so: the provider
-- is asked for the payment by reference, and only its SUCCESS answer moves
-- money. The row is its own work queue (next_attempt_at) so a crash at any
-- point is repaired by driving it again.
--
--   INITIATED  waiting for the provider to confirm                -> VERIFIED | FAILED | EXPIRED | REVIEW
--   VERIFIED   provider confirmed; ledger credit not yet recorded -> CREDITED
--   CREDITED   customer's account credited; repayment pending     -> APPLIED | UNAPPLIED
--   APPLIED    repayment allocated to the loan                    (final)
--   UNAPPLIED  money stays in the customer's account              (final)
--   FAILED     provider says the payment failed                   -> VERIFIED (provider later proves success)
--   EXPIRED    never paid within the validity window              -> VERIFIED (paid late)
--   REVIEW     confirmed in an unexpected currency; a person decides
CREATE TABLE lending.external_payments (
  payment_id      uuid PRIMARY KEY,
  loan_id         uuid NOT NULL REFERENCES lending.loans,
  customer_id     uuid NOT NULL,
  -- Our reference, fixed at creation: the provider, the webhook and the
  -- status query are all matched on it.
  reference       text NOT NULL UNIQUE,
  provider        text NOT NULL,
  provider_ref    text NOT NULL DEFAULT '',
  expected_minor  bigint NOT NULL CHECK (expected_minor > 0),
  -- What the provider confirmed. This, not expected_minor, is what is
  -- credited: it is the money that actually arrived.
  verified_minor  bigint CHECK (verified_minor > 0),
  currency        text NOT NULL,
  state           text NOT NULL CHECK (state IN ('INITIATED','VERIFIED','CREDITED','APPLIED','UNAPPLIED','FAILED','EXPIRED','REVIEW')),
  last_code       text NOT NULL DEFAULT '',
  journal_id      bigint,
  repayment_id    uuid REFERENCES lending.repayments,
  attempts        int NOT NULL DEFAULT 0,
  next_attempt_at timestamptz,
  expires_at      timestamptz NOT NULL,
  state_changed_at timestamptz NOT NULL DEFAULT now(),
  created_at      timestamptz NOT NULL DEFAULT now(),
  version         bigint NOT NULL DEFAULT 0,
  CHECK (state NOT IN ('VERIFIED','CREDITED','APPLIED','UNAPPLIED') OR verified_minor IS NOT NULL),
  CHECK (state NOT IN ('CREDITED','APPLIED','UNAPPLIED') OR journal_id IS NOT NULL)
);
CREATE INDEX external_payments_work ON lending.external_payments (next_attempt_at) WHERE next_attempt_at IS NOT NULL;
CREATE INDEX external_payments_loan ON lending.external_payments (loan_id, created_at);

-- Provider webhooks, deduplicated on the provider's event id.
CREATE TABLE lending.external_payment_events (
  provider    text NOT NULL,
  event_id    text NOT NULL,
  reference   text NOT NULL,
  received_at timestamptz NOT NULL DEFAULT now(),
  raw         bytea NOT NULL,
  PRIMARY KEY (provider, event_id)
);

ALTER TABLE lending.repayments DROP CONSTRAINT repayments_source_check;
ALTER TABLE lending.repayments ADD CONSTRAINT repayments_source_check CHECK (source IN ('CUSTOMER','AUTO_DEBIT','EXTERNAL'));

GRANT SELECT, INSERT, UPDATE ON lending.external_payments TO lending_app;
-- Evidence: append-only for the application role.
GRANT SELECT, INSERT ON lending.external_payment_events TO lending_app;

-- +goose Down
ALTER TABLE lending.repayments DROP CONSTRAINT repayments_source_check;
ALTER TABLE lending.repayments ADD CONSTRAINT repayments_source_check CHECK (source IN ('CUSTOMER','AUTO_DEBIT'));
DROP TABLE lending.external_payment_events;
DROP TABLE lending.external_payments;
