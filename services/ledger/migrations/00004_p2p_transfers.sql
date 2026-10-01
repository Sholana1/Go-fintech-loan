-- +goose Up
-- Real-time transfers between customers, and from a customer to an account
-- at another bank.
--
-- Between two customers the whole movement is one journal:
--
--   Dr sender's deposit      amount
--   Cr recipient's deposit   amount
--
-- Both balance rows are locked in one transaction, so the debit and the
-- credit happen together or not at all, and the sender's funds are checked
-- under that lock.
--
-- To another bank the money leaves through a payment rail, whose outcome may
-- be unknown for a while: the amount is HELD on the sender's account, and
-- only captured (debit sender, credit transfer clearing) when the rail
-- confirms the transfer.
INSERT INTO ledger.accounts (code, class, normal_side, currency, gl_code, owner_type) VALUES
  -- What the bank owes the rail for confirmed outbound transfers until
  -- settlement moves cash out of the settlement bank account.
  ('SYS:TRANSFER_CLEARING', 'LIABILITY', 'CREDIT', 'NGN', '2320', 'SYSTEM');

INSERT INTO ledger.balances (account_id, floor)
SELECT account_id, NULL FROM ledger.accounts WHERE code = 'SYS:TRANSFER_CLEARING';

-- P2P_TRANSFER is the only journal type that may touch two customer
-- accounts, and it may touch no system account: it cannot create money.
INSERT INTO ledger.journal_types (journal_type, max_customer_accounts, system_accounts) VALUES
  ('P2P_TRANSFER',              2, '{}'),
  ('P2P_OUTBOUND_CAPTURE',      1, '{SYS:TRANSFER_CLEARING}'),
  ('P2P_OUTBOUND_LATE_CAPTURE', 1, '{SYS:TRANSFER_CLEARING}');

-- Only the payments service moves money between customers. Lending still
-- cannot.
INSERT INTO ledger.posting_rights (caller, action) VALUES
  ('payments', 'READ'),
  ('payments', 'P2P_TRANSFER'),
  ('payments', 'HOLD:P2P_OUTBOUND'),
  ('payments', 'P2P_OUTBOUND_CAPTURE'),
  ('payments', 'P2P_OUTBOUND_LATE_CAPTURE');

-- +goose Down
DELETE FROM ledger.posting_rights WHERE caller = 'payments';
DELETE FROM ledger.journal_types WHERE journal_type IN ('P2P_TRANSFER','P2P_OUTBOUND_CAPTURE','P2P_OUTBOUND_LATE_CAPTURE');
