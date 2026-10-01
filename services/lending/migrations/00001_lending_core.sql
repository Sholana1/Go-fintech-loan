-- +goose Up
-- Lending service schema. Amounts are bigint minor units (kobo); money is
-- never stored as floating point. Business dates are `date` in Africa/Lagos;
-- instants are timestamptz (UTC).

CREATE SCHEMA lending;

-- ---------------------------------------------------------------- applications
CREATE TABLE lending.applications (
  application_id   uuid PRIMARY KEY,
  customer_id      uuid NOT NULL,
  product_id       text NOT NULL,
  product_version  int  NOT NULL,
  requested_principal_minor   bigint NOT NULL CHECK (requested_principal_minor > 0),
  tenor_months     int  NOT NULL CHECK (tenor_months > 0),
  stated_monthly_income_minor bigint NOT NULL CHECK (stated_monthly_income_minor >= 0),
  state            text NOT NULL CHECK (state IN ('SUBMITTED','ASSESSING','DECLINED','REFERRED','OFFERED',
                                                  'ACCEPTED','DISBURSED','EXPIRED','CANCELLED','DISBURSEMENT_FAILED')),
  state_reason     text[] NOT NULL DEFAULT '{}',
  waiting_on       text NOT NULL DEFAULT '',
  -- Consent to the credit-bureau enquiry and automated assessment. The row
  -- cannot exist without it.
  consent_ref      uuid NOT NULL,
  consent_policy_version text NOT NULL,
  consented_at     timestamptz NOT NULL,
  submitted_at     timestamptz NOT NULL,              -- T0
  decided_at       timestamptz,                       -- T1
  expires_at       timestamptz NOT NULL,
  attempts         int NOT NULL DEFAULT 0,
  next_attempt_at  timestamptz,                       -- when a worker should (re)drive it; NULL = nothing to do
  version          bigint NOT NULL DEFAULT 0,
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now()
);
-- One open application per customer and product. This index, not application
-- code, is what makes concurrent duplicate submissions impossible.
CREATE UNIQUE INDEX one_open_application ON lending.applications (customer_id, product_id)
  WHERE state IN ('SUBMITTED','ASSESSING','REFERRED','OFFERED','ACCEPTED');
CREATE INDEX applications_work ON lending.applications (next_attempt_at)
  WHERE next_attempt_at IS NOT NULL;
CREATE INDEX applications_customer ON lending.applications (customer_id, created_at DESC);
CREATE INDEX applications_referred ON lending.applications (submitted_at) WHERE state = 'REFERRED';

-- Raw credit-bureau responses, kept immutably so a decision can be reproduced.
CREATE TABLE lending.bureau_reports (
  report_id      uuid PRIMARY KEY,
  application_id uuid NOT NULL REFERENCES lending.applications,
  customer_id    uuid NOT NULL,
  provider       text NOT NULL,
  request_ref    text NOT NULL,
  provider_ref   text NOT NULL,
  fetched_at     timestamptz NOT NULL,
  raw            bytea NOT NULL,
  raw_sha256     text NOT NULL,
  UNIQUE (provider, request_ref)
);
CREATE INDEX bureau_reports_customer ON lending.bureau_reports (customer_id, fetched_at DESC);

-- One row per credit decision, written once and never updated.
CREATE TABLE lending.decision_snapshots (
  decision_id      uuid PRIMARY KEY,
  application_id   uuid NOT NULL REFERENCES lending.applications,
  outcome          text NOT NULL CHECK (outcome IN ('APPROVE','REFER','DECLINE')),
  reason_codes     text[] NOT NULL,
  approved_principal_minor bigint NOT NULL DEFAULT 0,
  risk_band        text NOT NULL DEFAULT '',
  monthly_rate_bps int NOT NULL DEFAULT 0,
  policy_version   text NOT NULL,
  product_id       text NOT NULL,
  product_version  int NOT NULL,
  features         jsonb NOT NULL,                    -- exactly the inputs the policy saw
  input_refs       jsonb NOT NULL,                    -- pointers to raw evidence (bureau report id, fraud check)
  decided_by       text NOT NULL,                     -- 'AUTO' or a staff id
  reviewer_note    text,
  created_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX decision_snapshots_application ON lending.decision_snapshots (application_id, created_at);

CREATE TABLE lending.offers (
  offer_id          uuid PRIMARY KEY,
  application_id    uuid NOT NULL REFERENCES lending.applications,
  decision_id       uuid NOT NULL REFERENCES lending.decision_snapshots,
  customer_id       uuid NOT NULL,
  principal_minor   bigint NOT NULL CHECK (principal_minor > 0),
  tenor_months      int NOT NULL,
  monthly_rate_bps  int NOT NULL,
  origination_fee_minor   bigint NOT NULL CHECK (origination_fee_minor >= 0),
  net_disbursement_minor  bigint NOT NULL CHECK (net_disbursement_minor > 0),
  instalment_minor        bigint NOT NULL,
  total_interest_minor    bigint NOT NULL,
  total_repayable_minor   bigint NOT NULL,
  nominal_annual_rate_bps   int NOT NULL,
  effective_annual_cost_bps int NOT NULL,
  disclosure        bytea NOT NULL,                   -- canonical JSON exactly as hashed
  disclosure_hash   text NOT NULL,
  state             text NOT NULL CHECK (state IN ('OFFERED','ACCEPTED','EXPIRED','VOIDED')),
  expires_at        timestamptz NOT NULL,
  accepted_at       timestamptz,                      -- T2
  acceptance        jsonb,                            -- evidence: token id, step-up result, authorisations
  created_at        timestamptz NOT NULL DEFAULT now(),
  CHECK (net_disbursement_minor = principal_minor - origination_fee_minor),
  CHECK ((state = 'ACCEPTED') = (accepted_at IS NOT NULL))
);
-- At most one live offer per application: a new offer requires voiding the old.
CREATE UNIQUE INDEX one_live_offer ON lending.offers (application_id) WHERE state = 'OFFERED';
CREATE INDEX offers_expiry ON lending.offers (expires_at) WHERE state = 'OFFERED';

-- ---------------------------------------------------------------------- loans
CREATE TABLE lending.loans (
  loan_id          uuid PRIMARY KEY,
  application_id   uuid NOT NULL UNIQUE REFERENCES lending.applications,  -- one loan per application
  offer_id         uuid NOT NULL UNIQUE REFERENCES lending.offers,        -- one loan per offer
  customer_id      uuid NOT NULL,
  product_id       text NOT NULL,
  product_version  int NOT NULL,
  principal_minor  bigint NOT NULL CHECK (principal_minor > 0),
  monthly_rate_bps int NOT NULL,
  tenor_months     int NOT NULL,
  origination_fee_minor bigint NOT NULL,
  state            text NOT NULL CHECK (state IN ('PENDING_DISBURSEMENT','ACTIVE','IN_ARREARS','CLOSED','WRITTEN_OFF','DISBURSEMENT_FAILED')),
  schedule_version int NOT NULL DEFAULT 1,
  auto_debit_authorised boolean NOT NULL,
  deposit_account_code  text NOT NULL,
  accepted_on      date NOT NULL,
  disbursed_at     timestamptz,                       -- T3: set only when the ledger has posted
  disbursement_journal_id bigint,
  days_past_due    int NOT NULL DEFAULT 0,
  arrears_bucket   text NOT NULL DEFAULT 'CURRENT',
  restructured     boolean NOT NULL DEFAULT false,
  written_off_minor bigint NOT NULL DEFAULT 0,
  recovered_minor   bigint NOT NULL DEFAULT 0 CHECK (recovered_minor <= written_off_minor),
  closed_at        timestamptz,
  -- True when a loan left servicing with earned interest not yet recognised
  -- in the ledger (for example an early settlement on a day the accrual job
  -- had not reached). The accrual job recognises it and clears the flag.
  accrual_catchup  boolean NOT NULL DEFAULT false,
  -- Scheduled collection bookkeeping.
  next_collection_at       timestamptz,
  collection_attempt_date  date,
  collection_attempts      int NOT NULL DEFAULT 0,
  version          bigint NOT NULL DEFAULT 0,
  created_at       timestamptz NOT NULL DEFAULT now(),
  -- "Disbursed" always means posted: a servicing state requires T3 and a journal.
  CHECK (state IN ('PENDING_DISBURSEMENT','DISBURSEMENT_FAILED') OR (disbursed_at IS NOT NULL AND disbursement_journal_id IS NOT NULL))
);
CREATE INDEX loans_customer ON lending.loans (customer_id, created_at DESC);
CREATE INDEX loans_servicing ON lending.loans (loan_id) WHERE state IN ('ACTIVE','IN_ARREARS');
CREATE INDEX loans_accrual_catchup ON lending.loans (loan_id) WHERE accrual_catchup;
CREATE INDEX loans_collection ON lending.loans (next_collection_at)
  WHERE next_collection_at IS NOT NULL AND state IN ('ACTIVE','IN_ARREARS');

CREATE TABLE lending.instalments (
  loan_id          uuid NOT NULL REFERENCES lending.loans,
  schedule_version int NOT NULL,
  seq              int NOT NULL,
  period_start     date NOT NULL,
  due_date         date NOT NULL,
  principal_due    bigint NOT NULL CHECK (principal_due >= 0),
  interest_due     bigint NOT NULL CHECK (interest_due >= 0),
  fees_due         bigint NOT NULL DEFAULT 0 CHECK (fees_due >= 0),
  principal_paid   bigint NOT NULL DEFAULT 0 CHECK (principal_paid >= 0 AND principal_paid <= principal_due),
  interest_paid    bigint NOT NULL DEFAULT 0 CHECK (interest_paid >= 0 AND interest_paid <= interest_due),
  fees_paid        bigint NOT NULL DEFAULT 0 CHECK (fees_paid >= 0 AND fees_paid <= fees_due),
  interest_accrued bigint NOT NULL DEFAULT 0 CHECK (interest_accrued >= 0 AND interest_accrued <= interest_due),
  accrual_from     date NOT NULL,
  accrual_base     bigint NOT NULL DEFAULT 0 CHECK (accrual_base >= 0 AND accrual_base <= interest_due),
  PRIMARY KEY (loan_id, schedule_version, seq)
);
CREATE INDEX instalments_due ON lending.instalments (due_date)
  WHERE principal_paid < principal_due OR interest_paid < interest_due OR fees_paid < fees_due;

-- ------------------------------------------------------------ posting intents
-- A posting intent is the durable instruction to post one journal to the
-- ledger. It is written in the same transaction as the business change that
-- needs it, posted by a worker (the ledger makes that idempotent), and then
-- applied back to lending in a second transaction. It is how every
-- lending->ledger movement survives a crash at any point.
CREATE TABLE lending.posting_intents (
  intent_id       uuid PRIMARY KEY,                   -- also the ledger posting reference op_id
  kind            text NOT NULL CHECK (kind IN ('DISBURSEMENT','REPAYMENT','LATE_FEE','ACCRUAL','WRITE_OFF','RECOVERY')),
  loan_id         uuid REFERENCES lending.loans,
  journal_type    text NOT NULL,
  lines           jsonb NOT NULL,
  business_date   date,
  payload         jsonb NOT NULL DEFAULT '{}',
  state           text NOT NULL DEFAULT 'PENDING' CHECK (state IN ('PENDING','POSTED','REJECTED')),
  reject_reason   text,
  journal_id      bigint,
  attempts        int NOT NULL DEFAULT 0,
  next_attempt_at timestamptz NOT NULL,
  created_at      timestamptz NOT NULL DEFAULT now(),
  completed_at    timestamptz,
  CHECK ((state = 'POSTED') = (journal_id IS NOT NULL))
);
-- At most one in-flight posting per loan, so loan-level changes are applied
-- strictly one after another and an allocation computed against the schedule
-- cannot be overtaken by another.
CREATE UNIQUE INDEX one_pending_intent_per_loan ON lending.posting_intents (loan_id)
  WHERE state = 'PENDING' AND loan_id IS NOT NULL;
CREATE INDEX posting_intents_work ON lending.posting_intents (next_attempt_at) WHERE state = 'PENDING';

CREATE TABLE lending.repayments (
  repayment_id    uuid PRIMARY KEY,
  loan_id         uuid NOT NULL REFERENCES lending.loans,
  customer_id     uuid NOT NULL,
  source          text NOT NULL CHECK (source IN ('CUSTOMER','AUTO_DEBIT')),
  requested_minor bigint NOT NULL CHECK (requested_minor > 0),
  applied_minor   bigint NOT NULL CHECK (applied_minor > 0),
  unapplied_minor bigint NOT NULL CHECK (unapplied_minor >= 0),
  allocation      jsonb NOT NULL,                     -- lines per instalment, or a recovery marker
  is_recovery     boolean NOT NULL DEFAULT false,
  state           text NOT NULL CHECK (state IN ('PENDING','ALLOCATED','REJECTED')),
  reject_reason   text,
  journal_id      bigint,
  business_date   date NOT NULL,
  intent_id       uuid NOT NULL UNIQUE REFERENCES lending.posting_intents,
  created_at      timestamptz NOT NULL DEFAULT now(),
  completed_at    timestamptz,
  CHECK (applied_minor + unapplied_minor = requested_minor)
);
CREATE INDEX repayments_loan ON lending.repayments (loan_id, created_at);

CREATE TABLE lending.loan_fees (
  fee_id        uuid PRIMARY KEY,
  loan_id       uuid NOT NULL REFERENCES lending.loans,
  kind          text NOT NULL CHECK (kind IN ('LATE_FEE')),
  instalment_due_date date NOT NULL,
  amount_minor  bigint NOT NULL CHECK (amount_minor > 0),
  state         text NOT NULL CHECK (state IN ('PENDING','POSTED','REJECTED')),
  intent_id     uuid NOT NULL UNIQUE REFERENCES lending.posting_intents,
  created_at    timestamptz NOT NULL DEFAULT now(),
  -- A late fee is assessed at most once per instalment.
  UNIQUE (loan_id, kind, instalment_due_date)
);

-- One row per loan per business date on which interest was recognised.
CREATE TABLE lending.interest_accruals (
  loan_id       uuid NOT NULL REFERENCES lending.loans,
  business_date date NOT NULL,
  amount_minor  bigint NOT NULL CHECK (amount_minor > 0),
  PRIMARY KEY (loan_id, business_date)
);
CREATE INDEX interest_accruals_date ON lending.interest_accruals (business_date);

-- One row per business date: the aggregate accrual journal for that date.
CREATE TABLE lending.accrual_runs (
  business_date date PRIMARY KEY,
  state         text NOT NULL CHECK (state IN ('CALCULATED','POSTED','EMPTY')),
  total_minor   bigint NOT NULL,
  loans         int NOT NULL,
  intent_id     uuid UNIQUE REFERENCES lending.posting_intents,
  created_at    timestamptz NOT NULL DEFAULT now(),
  posted_at     timestamptz
);

-- Records that a daily job completed for a business date, so each job runs
-- once per date and missed dates are caught up in order.
CREATE TABLE lending.job_runs (
  job           text NOT NULL,
  business_date date NOT NULL,
  completed_at  timestamptz NOT NULL DEFAULT now(),
  summary       jsonb NOT NULL DEFAULT '{}',
  PRIMARY KEY (job, business_date)
);

-- -------------------------------------------------------------------- payouts
CREATE TABLE lending.payouts (
  payout_id      uuid PRIMARY KEY,
  loan_id        uuid NOT NULL UNIQUE REFERENCES lending.loans,           -- one payout per loan
  customer_id    uuid NOT NULL,
  amount_minor   bigint NOT NULL CHECK (amount_minor > 0),
  bank_code      text NOT NULL,
  account_number text NOT NULL,
  account_name   text NOT NULL,                       -- from name enquiry
  enquiry_ref    text NOT NULL,
  -- Our reference for the transfer, fixed at creation. Every send, query,
  -- callback and report line is matched on it.
  reference      text NOT NULL UNIQUE,
  provider       text NOT NULL,
  provider_ref   text NOT NULL DEFAULT '',
  state          text NOT NULL CHECK (state IN ('PENDING','READY','HELD','SENDING','SUCCEEDED','FAILED','UNKNOWN','SETTLED','CANCELLED')),
  last_code      text NOT NULL DEFAULT '',
  attempts       int NOT NULL DEFAULT 0,
  next_attempt_at timestamptz,
  state_changed_at timestamptz NOT NULL DEFAULT now(),
  created_at     timestamptz NOT NULL DEFAULT now(),
  version        bigint NOT NULL DEFAULT 0
);
CREATE INDEX payouts_work ON lending.payouts (next_attempt_at) WHERE next_attempt_at IS NOT NULL;
CREATE INDEX payouts_unresolved ON lending.payouts (state_changed_at) WHERE state IN ('SENDING','UNKNOWN');

-- Provider callbacks, deduplicated on the provider's event id.
CREATE TABLE lending.payout_events (
  provider    text NOT NULL,
  event_id    text NOT NULL,
  reference   text NOT NULL,
  outcome     text NOT NULL,
  received_at timestamptz NOT NULL DEFAULT now(),
  raw         bytea NOT NULL,
  PRIMARY KEY (provider, event_id)
);

-- ------------------------------------------------------------- administration
CREATE TABLE lending.admin_actions (
  action_id   uuid PRIMARY KEY,
  kind        text NOT NULL CHECK (kind IN ('WRITE_OFF','RESTRUCTURE')),
  loan_id     uuid NOT NULL REFERENCES lending.loans,
  params      jsonb NOT NULL DEFAULT '{}',
  reason      text NOT NULL CHECK (length(reason) >= 10),
  state       text NOT NULL CHECK (state IN ('PROPOSED','APPROVED','REJECTED','EXECUTED','FAILED')),
  maker_id    uuid NOT NULL,
  checker_id  uuid,
  decision_note text,
  created_at  timestamptz NOT NULL DEFAULT now(),
  decided_at  timestamptz,
  executed_at timestamptz,
  intent_id   uuid UNIQUE REFERENCES lending.posting_intents,
  -- Maker-checker: the person who approves is never the person who proposed.
  CHECK (checker_id IS NULL OR checker_id <> maker_id)
);
CREATE UNIQUE INDEX one_open_admin_action ON lending.admin_actions (loan_id) WHERE state IN ('PROPOSED','APPROVED');

CREATE TABLE lending.recon_exceptions (
  exception_id uuid PRIMARY KEY,
  kind         text NOT NULL,
  entity_type  text NOT NULL,
  entity_id    text NOT NULL,
  amount_minor bigint NOT NULL DEFAULT 0,
  detail       jsonb NOT NULL DEFAULT '{}',
  state        text NOT NULL DEFAULT 'OPEN' CHECK (state IN ('OPEN','RESOLVED')),
  opened_at    timestamptz NOT NULL DEFAULT now(),
  resolved_at  timestamptz,
  resolved_by  text,
  resolution_note text
);
-- The same discrepancy is recorded once while it is open.
CREATE UNIQUE INDEX one_open_exception ON lending.recon_exceptions (kind, entity_type, entity_id) WHERE state = 'OPEN';
CREATE INDEX recon_exceptions_open ON lending.recon_exceptions (opened_at) WHERE state = 'OPEN';

-- Append-only audit trail of decisions and staff actions.
CREATE TABLE lending.audit_log (
  audit_id    bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  at          timestamptz NOT NULL DEFAULT now(),
  actor_kind  text NOT NULL,                          -- 'customer' | 'staff' | 'system'
  actor_id    text NOT NULL,
  action      text NOT NULL,
  entity_type text NOT NULL,
  entity_id   text NOT NULL,
  detail      jsonb NOT NULL DEFAULT '{}'
);
CREATE INDEX audit_log_entity ON lending.audit_log (entity_type, entity_id, at);

-- ------------------------------------------------------- idempotency / events
-- API idempotency. The row is written in the same transaction as the
-- resource it created, so a key is recorded if and only if the request took
-- effect.
CREATE TABLE lending.idempotency_keys (
  principal_id  uuid NOT NULL,
  endpoint      text NOT NULL,
  idem_key      text NOT NULL,
  request_hash  bytea NOT NULL,
  resource_id   uuid NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now(),
  expires_at    timestamptz NOT NULL,
  PRIMARY KEY (principal_id, endpoint, idem_key)
);
CREATE INDEX idempotency_keys_expiry ON lending.idempotency_keys (expires_at);

-- Consumer deduplication: an event is applied if and only if its id is
-- inserted here in the same transaction as its effect.
CREATE TABLE lending.inbox (
  consumer     text NOT NULL,
  event_id     uuid NOT NULL,
  processed_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (consumer, event_id)
);

CREATE TABLE lending.outbox (
  outbox_id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  event_id          uuid NOT NULL UNIQUE,
  topic             text NOT NULL,
  partition_key     text NOT NULL,
  event_type        text NOT NULL,
  schema_version    int  NOT NULL,
  aggregate_type    text NOT NULL,
  aggregate_id      text NOT NULL,
  aggregate_version bigint NOT NULL,
  payload           jsonb NOT NULL,
  traceparent       text,
  occurred_at       timestamptz NOT NULL DEFAULT now(),
  published_at      timestamptz
);
CREATE INDEX outbox_unpublished ON lending.outbox (outbox_id) WHERE published_at IS NULL;

-- ------------------------------------------------------------------ privileges
GRANT USAGE ON SCHEMA lending TO lending_app;
GRANT SELECT, INSERT, UPDATE ON ALL TABLES IN SCHEMA lending TO lending_app;
GRANT DELETE ON lending.outbox, lending.idempotency_keys, lending.inbox TO lending_app;
-- Evidence tables are append-only for the application role.
REVOKE UPDATE ON lending.audit_log, lending.decision_snapshots, lending.bureau_reports,
                 lending.payout_events, lending.interest_accruals FROM lending_app;

-- +goose Down
DROP SCHEMA lending CASCADE;
