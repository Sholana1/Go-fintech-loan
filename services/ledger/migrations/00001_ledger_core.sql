-- +goose Up
-- Core double-entry ledger. See docs/adr/0003-ledger-invariants.md for which
-- statement enforces which invariant.

CREATE SCHEMA ledger;

CREATE TYPE ledger.side AS ENUM ('DEBIT','CREDIT');
CREATE TYPE ledger.account_class AS ENUM ('ASSET','LIABILITY','EQUITY','INCOME','EXPENSE');

CREATE TABLE ledger.accounts (
  account_id   bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  code         text NOT NULL UNIQUE,
  class        ledger.account_class NOT NULL,
  normal_side  ledger.side NOT NULL,
  currency     char(3) NOT NULL,
  gl_code      text NOT NULL,
  owner_type   text NOT NULL CHECK (owner_type IN ('CUSTOMER','SYSTEM')),
  owner_id     uuid,
  status       text NOT NULL DEFAULT 'ACTIVE'
               CHECK (status IN ('ACTIVE','POST_NO_DEBIT','FROZEN','CLOSED')),
  created_at   timestamptz NOT NULL DEFAULT now(),
  UNIQUE (account_id, currency),
  CHECK ((class IN ('ASSET','EXPENSE')) = (normal_side = 'DEBIT')),
  CHECK ((owner_type = 'CUSTOMER') = (owner_id IS NOT NULL))
);

-- Projection of entries and holds. Written only by the triggers below; the
-- application role cannot update posted or held.
CREATE TABLE ledger.balances (
  account_id  bigint PRIMARY KEY REFERENCES ledger.accounts,
  posted      bigint NOT NULL DEFAULT 0,   -- signed in the account's normal side
  held        bigint NOT NULL DEFAULT 0 CHECK (held >= 0),
  floor       bigint,                      -- 0 for customer accounts; NULL = unconstrained (system accounts)
  version     bigint NOT NULL DEFAULT 0,
  updated_at  timestamptz NOT NULL DEFAULT now(),
  -- Backstop for "concurrent spending cannot exceed permitted funds". The
  -- application also checks under the row lock so it can return a clear
  -- error; this constraint holds even if that check has a bug.
  CONSTRAINT available_not_below_floor CHECK (floor IS NULL OR posted - held >= floor)
);

CREATE TABLE ledger.journals (
  journal_id    bigint GENERATED ALWAYS AS IDENTITY,
  posted_at     timestamptz NOT NULL DEFAULT clock_timestamp(),
  business_date date NOT NULL,              -- Africa/Lagos
  journal_type  text NOT NULL,
  op_type       text NOT NULL,
  op_id         uuid NOT NULL,
  op_step       text NOT NULL,
  reverses_journal_id bigint,
  narrative     text,
  created_by    text NOT NULL,              -- workload identity of the caller
  PRIMARY KEY (journal_id, posted_at)
) PARTITION BY RANGE (posted_at);

CREATE TABLE ledger.entries (
  entry_id      bigint GENERATED ALWAYS AS IDENTITY,
  journal_id    bigint NOT NULL,
  posted_at     timestamptz NOT NULL,
  account_id    bigint NOT NULL,
  currency      char(3) NOT NULL,
  direction     ledger.side NOT NULL,
  amount        bigint NOT NULL CHECK (amount > 0),
  balance_after bigint NOT NULL,            -- set by trigger; running balance for statements and drift detection
  PRIMARY KEY (entry_id, posted_at),
  FOREIGN KEY (journal_id, posted_at) REFERENCES ledger.journals (journal_id, posted_at),
  -- An entry's currency must be its account's currency.
  FOREIGN KEY (account_id, currency)  REFERENCES ledger.accounts (account_id, currency)
) PARTITION BY RANGE (posted_at);

CREATE INDEX entries_account_time ON ledger.entries (account_id, posted_at DESC, entry_id DESC);
CREATE INDEX entries_journal ON ledger.entries (journal_id);

-- Safety net: a row whose month partition is missing lands here instead of
-- failing the posting. The ledger verifier alerts when these are non-empty.
CREATE TABLE ledger.journals_default PARTITION OF ledger.journals DEFAULT;
CREATE TABLE ledger.entries_default  PARTITION OF ledger.entries  DEFAULT;

-- Not partitioned, so its primary key is global: one journal per operation
-- step, ever. This is the "cannot be posted twice" invariant.
CREATE TABLE ledger.posting_refs (
  op_type      text NOT NULL,
  op_id        uuid NOT NULL,
  op_step      text NOT NULL,
  request_hash bytea NOT NULL,              -- detects reuse of a reference with different content
  journal_id   bigint,
  posted_at    timestamptz,
  PRIMARY KEY (op_type, op_id, op_step)
);

CREATE TABLE ledger.holds (
  hold_id     uuid PRIMARY KEY,
  account_id  bigint NOT NULL REFERENCES ledger.accounts,
  currency    char(3) NOT NULL,
  amount      bigint NOT NULL CHECK (amount > 0),
  status      text NOT NULL DEFAULT 'ACTIVE'
              CHECK (status IN ('ACTIVE','CAPTURED','RELEASED','EXPIRED')),
  captured_amount bigint NOT NULL DEFAULT 0
              CHECK (captured_amount >= 0 AND captured_amount <= amount),
  op_type     text NOT NULL,
  op_id       uuid NOT NULL,
  expires_at  timestamptz,
  created_by  text NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now(),
  closed_at   timestamptz,
  UNIQUE (op_type, op_id),
  CHECK ((status = 'ACTIVE') = (closed_at IS NULL)),
  FOREIGN KEY (account_id, currency) REFERENCES ledger.accounts (account_id, currency)
);
CREATE INDEX holds_account_active ON ledger.holds (account_id) WHERE status = 'ACTIVE';

CREATE TABLE ledger.outbox (
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
CREATE INDEX outbox_unpublished ON ledger.outbox (outbox_id) WHERE published_at IS NULL;

-- Which workload may perform which action. Actions are journal types,
-- 'HOLD:<op_type>', 'OPEN_ACCOUNT' and 'READ'. Rows are added by reviewed
-- migrations only.
CREATE TABLE ledger.posting_rights (
  caller  text NOT NULL,
  action  text NOT NULL,
  PRIMARY KEY (caller, action)
);

-- Balances change only through these two triggers. They run as the table
-- owner (SECURITY DEFINER) so the application role needs no UPDATE right on
-- posted or held.

-- +goose StatementBegin
CREATE FUNCTION ledger.apply_entry() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = ledger, pg_temp AS $$
DECLARE v_normal ledger.side; v_delta bigint;
BEGIN
  SELECT normal_side INTO STRICT v_normal FROM ledger.accounts WHERE account_id = NEW.account_id;
  v_delta := CASE WHEN NEW.direction = v_normal THEN NEW.amount ELSE -NEW.amount END;
  UPDATE ledger.balances
     SET posted = posted + v_delta, version = version + 1, updated_at = now()
   WHERE account_id = NEW.account_id
   RETURNING posted INTO STRICT NEW.balance_after;
  RETURN NEW;
END $$;
-- +goose StatementEnd

CREATE TRIGGER entries_apply BEFORE INSERT ON ledger.entries
  FOR EACH ROW EXECUTE FUNCTION ledger.apply_entry();

-- +goose StatementBegin
CREATE FUNCTION ledger.apply_hold() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = ledger, pg_temp AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'ACTIVE' THEN RAISE EXCEPTION 'hold must be created ACTIVE'; END IF;
    UPDATE ledger.balances SET held = held + NEW.amount, version = version + 1, updated_at = now()
     WHERE account_id = NEW.account_id;
    RETURN NEW;
  END IF;
  IF (NEW.amount, NEW.account_id, NEW.currency, NEW.op_type, NEW.op_id)
       IS DISTINCT FROM (OLD.amount, OLD.account_id, OLD.currency, OLD.op_type, OLD.op_id) THEN
    RAISE EXCEPTION 'hold amount, account and reference are immutable';
  END IF;
  IF OLD.status <> 'ACTIVE' THEN
    RAISE EXCEPTION 'hold % is closed and cannot change', OLD.hold_id;
  END IF;
  IF NEW.status <> 'ACTIVE' THEN
    UPDATE ledger.balances SET held = held - OLD.amount, version = version + 1, updated_at = now()
     WHERE account_id = OLD.account_id;
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd

CREATE TRIGGER holds_apply BEFORE INSERT OR UPDATE ON ledger.holds
  FOR EACH ROW EXECUTE FUNCTION ledger.apply_hold();

-- A journal must balance per currency and have at least two entries. A
-- row-level CHECK cannot express this because it is a property of a set of
-- rows; it can only be evaluated once all rows exist, which is at COMMIT.
-- +goose StatementBegin
CREATE FUNCTION ledger.assert_balanced() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE v_bad int;
BEGIN
  SELECT count(*) INTO v_bad FROM (
    SELECT currency FROM ledger.entries
     WHERE journal_id = NEW.journal_id AND posted_at = NEW.posted_at
     GROUP BY currency
    HAVING sum(CASE direction WHEN 'DEBIT' THEN amount ELSE -amount END) <> 0 OR count(*) < 2
  ) x;
  IF v_bad > 0 THEN
    RAISE EXCEPTION 'journal % is not balanced', NEW.journal_id USING ERRCODE = '23514', CONSTRAINT = 'journal_balanced';
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER entries_balanced AFTER INSERT ON ledger.entries
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION ledger.assert_balanced();

-- Posted rows are immutable. Corrections are new journals that reference the
-- original through reverses_journal_id.
-- +goose StatementBegin
CREATE FUNCTION ledger.reject_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION '% on % is not allowed: ledger rows are immutable', TG_OP, TG_TABLE_NAME;
END $$;
-- +goose StatementEnd

CREATE TRIGGER journals_immutable BEFORE UPDATE OR DELETE ON ledger.journals
  FOR EACH ROW EXECUTE FUNCTION ledger.reject_change();
CREATE TRIGGER entries_immutable BEFORE UPDATE OR DELETE ON ledger.entries
  FOR EACH ROW EXECUTE FUNCTION ledger.reject_change();
CREATE TRIGGER journals_no_truncate BEFORE TRUNCATE ON ledger.journals
  FOR EACH STATEMENT EXECUTE FUNCTION ledger.reject_change();
CREATE TRIGGER entries_no_truncate BEFORE TRUNCATE ON ledger.entries
  FOR EACH STATEMENT EXECUTE FUNCTION ledger.reject_change();

-- Monthly partitions are created ahead of time by `ledgerd migrate`.
-- +goose StatementBegin
CREATE FUNCTION ledger.ensure_month_partitions(p_from date, p_months int) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
  v_start date := date_trunc('month', p_from)::date;
  v_end   date;
  v_name  text;
BEGIN
  FOR i IN 0 .. p_months - 1 LOOP
    v_end  := (v_start + interval '1 month')::date;
    v_name := to_char(v_start, 'YYYY_MM');
    EXECUTE format('CREATE TABLE IF NOT EXISTS ledger.journals_%s PARTITION OF ledger.journals FOR VALUES FROM (%L) TO (%L)',
                   v_name, v_start::timestamptz, v_end::timestamptz);
    EXECUTE format('CREATE TABLE IF NOT EXISTS ledger.entries_%s PARTITION OF ledger.entries FOR VALUES FROM (%L) TO (%L)',
                   v_name, v_start::timestamptz, v_end::timestamptz);
    v_start := v_end;
  END LOOP;
END $$;
-- +goose StatementEnd

-- Application role: may insert facts; cannot edit a balance, a journal or an
-- entry. The role itself is created by infrastructure, not by migrations.
GRANT USAGE ON SCHEMA ledger TO ledger_app;
GRANT SELECT ON ALL TABLES IN SCHEMA ledger TO ledger_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA ledger GRANT SELECT ON TABLES TO ledger_app;
GRANT INSERT ON ledger.accounts, ledger.journals, ledger.entries,
                ledger.holds, ledger.posting_refs, ledger.outbox TO ledger_app;
-- A new account's balance row can only be created at zero.
GRANT INSERT (account_id, floor) ON ledger.balances TO ledger_app;
GRANT UPDATE (journal_id, posted_at) ON ledger.posting_refs TO ledger_app;
GRANT UPDATE (status, captured_amount, closed_at) ON ledger.holds TO ledger_app;
GRANT UPDATE (published_at) ON ledger.outbox TO ledger_app;
GRANT DELETE ON ledger.outbox TO ledger_app;
-- Needed only so SELECT ... FOR UPDATE on balances is permitted.
GRANT UPDATE (updated_at) ON ledger.balances TO ledger_app;

-- +goose Down
DROP SCHEMA ledger CASCADE;
