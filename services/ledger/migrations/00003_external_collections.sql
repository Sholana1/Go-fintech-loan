-- +goose Up
-- Inbound payments collected for the bank by an external payment provider
-- (a customer repaying a loan by card or bank transfer from outside).
--
-- When the provider confirms a payment, the money is the customer's and is
-- owed to the bank by the provider until it settles:
--
--   Dr SYS:COLLECTIONS_CLEARING   (asset: receivable from the provider)
--   Cr customer deposit           (liability: the customer's money)
--
-- The clearing balance is reconciled against the provider's settlements.
INSERT INTO ledger.accounts (code, class, normal_side, currency, gl_code, owner_type) VALUES
  ('SYS:COLLECTIONS_CLEARING', 'ASSET', 'DEBIT', 'NGN', '1110', 'SYSTEM');

INSERT INTO ledger.balances (account_id, floor)
SELECT account_id, NULL FROM ledger.accounts WHERE code = 'SYS:COLLECTIONS_CLEARING';

-- This journal credits a customer from a system account, so the right to
-- post it is the right to create customer money. It is granted to lending
-- only for provider-verified payments, each posted once under the payment's
-- id, and every such journal is matched to a provider record by
-- reconciliation.
INSERT INTO ledger.journal_types (journal_type, max_customer_accounts, system_accounts) VALUES
  ('EXTERNAL_COLLECTION', 1, '{SYS:COLLECTIONS_CLEARING}');

INSERT INTO ledger.posting_rights (caller, action) VALUES
  ('lending', 'EXTERNAL_COLLECTION');

-- +goose Down
DELETE FROM ledger.posting_rights WHERE caller = 'lending' AND action = 'EXTERNAL_COLLECTION';
DELETE FROM ledger.journal_types WHERE journal_type = 'EXTERNAL_COLLECTION';
