#!/usr/bin/env bash
# Local acceptance scenario for the personal-loan product.
#
# Prerequisites (see README): make infra-up dev-setup migrate topics, then the
# four processes running (make run-ledger / run-sim / run-identity / run-lending).
#
# Everything here runs against local processes and a provider SIMULATOR. No
# real money, customer or provider is involved.
set -euo pipefail
cd "$(dirname "$0")/.."
set -a; . .dev/local.env; set +a

ID=http://127.0.0.1:8002
LN=http://127.0.0.1:8003
j() { python3 -c "import sys,json; d=json.load(sys.stdin); print(d$1)"; }
say() { printf '\n== %s\n' "$*"; }

PHONE="+23480$(printf '%08d' $((RANDOM * 3000 + RANDOM)))"
BVN="$(printf '%07d' $((RANDOM * 300 + RANDOM)))4242"
PIN=482915

say "1. Register a customer (BVN verified by the simulator) and sign in"
CUST=$(curl -sf -X POST $ID/v1/customers -H 'Content-Type: application/json' \
  -d "{\"phone\":\"$PHONE\",\"full_name\":\"Ada Test Customer\",\"date_of_birth\":\"1990-05-17\",\"bvn\":\"$BVN\",\"pin\":\"$PIN\"}")
CUSTOMER_ID=$(echo "$CUST" | j "['customer_id']")
echo "$CUST"
TOKEN=$(curl -sf -X POST $ID/v1/sessions -H 'Content-Type: application/json' -d "{\"phone\":\"$PHONE\",\"pin\":\"$PIN\"}" | j "['access_token']")
AUTH="Authorization: Bearer $TOKEN"

say "2. Apply for NGN 100,000 over 3 months (T0)"
APP=$(curl -sf -X POST $LN/v1/loan-applications -H "$AUTH" -H 'Content-Type: application/json' -H "Idempotency-Key: scenario-apply-$CUSTOMER_ID" \
  -d '{"product_id":"personal-loan","amount_minor":10000000,"currency":"NGN","tenor_months":3,"stated_monthly_income_minor":30000000,"consent_credit_check":true}')
APP_ID=$(echo "$APP" | j "['application_id']")
echo "application $APP_ID status $(echo "$APP" | j "['status']")"

say "3. Wait for the decision (T1)"
for _ in $(seq 1 100); do
  APP=$(curl -sf $LN/v1/loan-applications/$APP_ID -H "$AUTH")
  STATUS=$(echo "$APP" | j "['status']")
  [ "$STATUS" != PROCESSING ] && break
  sleep 0.1
done
echo "status $STATUS"
[ "$STATUS" = OFFER_READY ] || { echo "$APP"; exit 1; }
OFFER_ID=$(echo "$APP" | j "['offer']['offer_id']")
HASH=$(echo "$APP" | j "['offer']['disclosure_hash']")
echo "$APP" | python3 -c "import sys,json; o=json.load(sys.stdin)['offer']; print({k:o[k] for k in ['principal_minor','origination_fee_minor','net_disbursement_minor','monthly_rate_bps','effective_annual_cost_bps','instalment_minor','total_repayable_minor']})"

say "4. Accept the offer with the disclosure hash and PIN (T2 -> T3)"
LOAN=$(curl -sf -X POST $LN/v1/loan-offers/$OFFER_ID/accept -H "$AUTH" -H 'Content-Type: application/json' -H "Idempotency-Key: scenario-accept-$CUSTOMER_ID" \
  -d "{\"disclosure_hash\":\"$HASH\",\"pin\":\"$PIN\",\"auto_debit_authorised\":false,\"destination\":{\"type\":\"DEPOSIT_ACCOUNT\"}}")
LOAN_ID=$(echo "$LOAN" | j "['loan_id']")
echo "loan $LOAN_ID status $(echo "$LOAN" | j "['status']")"

say "5. Repeat the acceptance with the same Idempotency-Key: same loan, no second disbursement"
AGAIN=$(curl -sf -X POST $LN/v1/loan-offers/$OFFER_ID/accept -H "$AUTH" -H 'Content-Type: application/json' -H "Idempotency-Key: scenario-accept-$CUSTOMER_ID" \
  -d "{\"disclosure_hash\":\"$HASH\",\"pin\":\"$PIN\",\"auto_debit_authorised\":false,\"destination\":{\"type\":\"DEPOSIT_ACCOUNT\"}}")
[ "$(echo "$AGAIN" | j "['loan_id']")" = "$LOAN_ID" ] && echo "same loan returned"

say "6. Payoff quote, then settle early (only the payoff is taken; unearned interest is waived)"
curl -sf $LN/v1/loans/$LOAN_ID/payoff-quote -H "$AUTH"; echo
APP_ENV=local go run ./cmd/devsetup fund "$CUSTOMER_ID" 500000
REP=$(curl -sf -X POST $LN/v1/loans/$LOAN_ID/repayments -H "$AUTH" -H 'Content-Type: application/json' -H "Idempotency-Key: scenario-repay-$CUSTOMER_ID" \
  -d '{"amount_minor":10400000,"currency":"NGN"}')
echo "$REP"

say "7. Statement"
curl -sf $LN/v1/loans/$LOAN_ID/statement -H "$AUTH" | python3 -c "import sys,json; s=json.load(sys.stdin); print('loan status:', s['loan']['status']); print('totals:', s['totals']); print('repayments:', [(r['status'], r['applied_minor'], r['unapplied_minor']) for r in s['repayments']])"

say "8. Ledger invariants"
go run ./cmd/ledgerd verify
