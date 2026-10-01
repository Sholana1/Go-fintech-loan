-- +goose Up
-- System accounts and posting rights needed by the personal-loan product.
-- GL codes follow the illustrative chart in the architecture plan (6.1).

INSERT INTO ledger.accounts (code, class, normal_side, currency, gl_code, owner_type) VALUES
  ('SYS:CASH_SETTLEMENT_BANK',       'ASSET',     'DEBIT',  'NGN', '1010', 'SYSTEM'),
  ('SYS:LOANS_PRINCIPAL',            'ASSET',     'DEBIT',  'NGN', '1210', 'SYSTEM'),
  ('SYS:LOANS_INTEREST_RECEIVABLE',  'ASSET',     'DEBIT',  'NGN', '1220', 'SYSTEM'),
  ('SYS:LOANS_FEES_RECEIVABLE',      'ASSET',     'DEBIT',  'NGN', '1230', 'SYSTEM'),
  ('SYS:PAYOUT_CLEARING',            'LIABILITY', 'CREDIT', 'NGN', '2310', 'SYSTEM'),
  ('SYS:INTEREST_INCOME',            'INCOME',    'CREDIT', 'NGN', '4010', 'SYSTEM'),
  ('SYS:FEE_INCOME',                 'INCOME',    'CREDIT', 'NGN', '4020', 'SYSTEM'),
  ('SYS:RECOVERIES_INCOME',          'INCOME',    'CREDIT', 'NGN', '4090', 'SYSTEM'),
  ('SYS:LOAN_WRITE_OFF_EXPENSE',     'EXPENSE',   'DEBIT',  'NGN', '5040', 'SYSTEM');

INSERT INTO ledger.balances (account_id, floor)
SELECT account_id, NULL FROM ledger.accounts WHERE owner_type = 'SYSTEM';

-- The lending service may post only loan journals; identity may only open
-- customer accounts. Nothing here lets lending move money between customers.
INSERT INTO ledger.posting_rights (caller, action) VALUES
  ('identity', 'OPEN_ACCOUNT'),
  ('lending',  'READ'),
  ('lending',  'LOAN_DISBURSEMENT'),
  ('lending',  'LOAN_REPAYMENT'),
  ('lending',  'LOAN_INTEREST_ACCRUAL'),
  ('lending',  'LOAN_FEE_ASSESSMENT'),
  ('lending',  'LOAN_WRITE_OFF'),
  ('lending',  'LOAN_RECOVERY'),
  ('lending',  'HOLD:LOAN_PAYOUT'),
  ('lending',  'LOAN_PAYOUT_CAPTURE'),
  ('lending',  'LOAN_PAYOUT_LATE_CAPTURE'),
  ('lending',  'LOAN_PAYOUT_SETTLEMENT');

-- What each journal type may touch. A right to post a journal type is a right
-- to post exactly this shape: at most this many customer accounts and only
-- these system accounts. Lending therefore cannot move money between two
-- customers or reach an unrelated system account, even with a bug.
CREATE TABLE ledger.journal_types (
  journal_type          text PRIMARY KEY,
  max_customer_accounts smallint NOT NULL CHECK (max_customer_accounts >= 0),
  system_accounts       text[] NOT NULL
);

INSERT INTO ledger.journal_types (journal_type, max_customer_accounts, system_accounts) VALUES
  ('LOAN_DISBURSEMENT',        1, '{SYS:LOANS_PRINCIPAL,SYS:FEE_INCOME}'),
  ('LOAN_REPAYMENT',           1, '{SYS:LOANS_PRINCIPAL,SYS:LOANS_INTEREST_RECEIVABLE,SYS:LOANS_FEES_RECEIVABLE}'),
  ('LOAN_INTEREST_ACCRUAL',    0, '{SYS:LOANS_INTEREST_RECEIVABLE,SYS:INTEREST_INCOME}'),
  ('LOAN_FEE_ASSESSMENT',      0, '{SYS:LOANS_FEES_RECEIVABLE,SYS:FEE_INCOME}'),
  ('LOAN_WRITE_OFF',           0, '{SYS:LOAN_WRITE_OFF_EXPENSE,SYS:LOANS_PRINCIPAL,SYS:LOANS_INTEREST_RECEIVABLE,SYS:LOANS_FEES_RECEIVABLE}'),
  ('LOAN_RECOVERY',            1, '{SYS:RECOVERIES_INCOME}'),
  ('LOAN_PAYOUT_CAPTURE',      1, '{SYS:PAYOUT_CLEARING}'),
  ('LOAN_PAYOUT_LATE_CAPTURE', 1, '{SYS:PAYOUT_CLEARING}'),
  ('LOAN_PAYOUT_SETTLEMENT',   0, '{SYS:PAYOUT_CLEARING,SYS:CASH_SETTLEMENT_BANK}');

-- +goose Down
DROP TABLE ledger.journal_types;
DELETE FROM ledger.posting_rights WHERE caller IN ('identity','lending');
