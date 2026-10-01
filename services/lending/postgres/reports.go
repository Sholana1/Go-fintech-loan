package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// SubledgerTotals are lending's own view of what the ledger control accounts
// should hold. Reconciliation compares them with the ledger.
type SubledgerTotals struct {
	// Principal outstanding on loans that have been disbursed and never
	// written off.
	PrincipalMinor int64
	// Interest recognised (accrued) but not yet paid, on the same loans.
	InterestReceivableMinor int64
	// Fees charged but not yet paid, on the same loans.
	FeesReceivableMinor int64
}

// SubledgerTotals sums the loan book. PENDING postings are excluded on both
// sides by construction: a repayment changes instalments only when its
// journal has posted, so the totals move together with the ledger.
func (q *Queries) SubledgerTotals(ctx context.Context) (SubledgerTotals, error) {
	var t SubledgerTotals
	err := q.db.QueryRow(ctx, `
		SELECT coalesce(sum(i.principal_due - i.principal_paid), 0)::bigint,
		       coalesce(sum(i.interest_accrued - i.interest_paid), 0)::bigint,
		       coalesce(sum(i.fees_due - i.fees_paid), 0)::bigint
		  FROM lending.loans l
		  JOIN lending.instalments i ON i.loan_id = l.loan_id AND i.schedule_version = l.schedule_version
		 WHERE l.state IN ('ACTIVE','IN_ARREARS','CLOSED')
		   -- A written-off loan's receivables have been removed from the
		   -- ledger; its instalments still show what was never paid, so it is
		   -- excluded here even after recoveries close it.
		   AND l.written_off_minor = 0`).Scan(&t.PrincipalMinor, &t.InterestReceivableMinor, &t.FeesReceivableMinor)
	return t, err
}

// PendingPostingCount is the number of postings in flight; reconciliation of
// control accounts is only meaningful when it is zero.
func (q *Queries) PendingPostingCount(ctx context.Context) (int, error) {
	var n int
	err := q.db.QueryRow(ctx, `SELECT count(*) FROM lending.posting_intents WHERE state = 'PENDING'`).Scan(&n)
	return n, err
}

// PortfolioRow is one line of the operational portfolio report.
type PortfolioRow struct {
	State                string `json:"state"`
	ArrearsBucket        string `json:"arrears_bucket"`
	Loans                int    `json:"loans"`
	PrincipalOutstanding int64  `json:"principal_outstanding_minor"`
	InterestOutstanding  int64  `json:"interest_due_unpaid_minor"`
	FeesOutstanding      int64  `json:"fees_unpaid_minor"`
	WrittenOff           int64  `json:"written_off_minor"`
	Recovered            int64  `json:"recovered_minor"`
}

// PortfolioReport groups the loan book by state and arrears bucket. It
// contains no customer data.
func (q *Queries) PortfolioReport(ctx context.Context) ([]PortfolioRow, error) {
	rows, err := q.db.Query(ctx, `
		SELECT l.state, l.arrears_bucket, count(DISTINCT l.loan_id),
		       coalesce(sum(i.principal_due - i.principal_paid), 0)::bigint,
		       coalesce(sum(i.interest_due - i.interest_paid), 0)::bigint,
		       coalesce(sum(i.fees_due - i.fees_paid), 0)::bigint,
		       coalesce((SELECT sum(written_off_minor) FROM lending.loans x WHERE x.state = l.state AND x.arrears_bucket = l.arrears_bucket), 0)::bigint,
		       coalesce((SELECT sum(recovered_minor) FROM lending.loans x WHERE x.state = l.state AND x.arrears_bucket = l.arrears_bucket), 0)::bigint
		  FROM lending.loans l
		  LEFT JOIN lending.instalments i ON i.loan_id = l.loan_id AND i.schedule_version = l.schedule_version
		 GROUP BY l.state, l.arrears_bucket
		 ORDER BY l.state, l.arrears_bucket`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (PortfolioRow, error) {
		var r PortfolioRow
		err := row.Scan(&r.State, &r.ArrearsBucket, &r.Loans, &r.PrincipalOutstanding, &r.InterestOutstanding, &r.FeesOutstanding, &r.WrittenOff, &r.Recovered)
		return r, err
	})
}

// Gauges are point-in-time operational measurements read from the database.
type Gauges struct {
	OldestPendingIntentAge time.Duration // includes disbursements waiting to post
	PendingDisbursements   int
	OldestPendingDisbursal time.Duration
	UnknownPayouts         int
	OldestUnknownPayoutAge time.Duration
	ApplicationsAssessing  int
	ApplicationsReferred   int
	// External payments the provider has confirmed that have not finished
	// being credited and applied. Money has arrived; the customer is waiting.
	ConfirmedPaymentsInFlight  int
	OldestConfirmedPaymentWait time.Duration
	OpenExceptionsByKind       map[string]int
}

// ReadGauges collects the operational gauges in one round trip per group.
func (q *Queries) ReadGauges(ctx context.Context, now time.Time) (Gauges, error) {
	g := Gauges{OpenExceptionsByKind: map[string]int{}}
	var intentAge, disbAge, payoutAge float64
	err := q.db.QueryRow(ctx, `
		SELECT
		  coalesce(extract(epoch FROM $1 - (SELECT min(created_at) FROM lending.posting_intents WHERE state = 'PENDING')), 0),
		  (SELECT count(*) FROM lending.posting_intents WHERE state = 'PENDING' AND kind = 'DISBURSEMENT'),
		  coalesce(extract(epoch FROM $1 - (SELECT min(created_at) FROM lending.posting_intents WHERE state = 'PENDING' AND kind = 'DISBURSEMENT')), 0),
		  (SELECT count(*) FROM lending.payouts WHERE state IN ('SENDING','UNKNOWN')),
		  coalesce(extract(epoch FROM $1 - (SELECT min(state_changed_at) FROM lending.payouts WHERE state IN ('SENDING','UNKNOWN'))), 0),
		  (SELECT count(*) FROM lending.applications WHERE state IN ('SUBMITTED','ASSESSING')),
		  (SELECT count(*) FROM lending.applications WHERE state = 'REFERRED')`, now).
		Scan(&intentAge, &g.PendingDisbursements, &disbAge, &g.UnknownPayouts, &payoutAge, &g.ApplicationsAssessing, &g.ApplicationsReferred)
	if err != nil {
		return g, err
	}
	g.OldestPendingIntentAge = time.Duration(intentAge * float64(time.Second))
	g.OldestPendingDisbursal = time.Duration(disbAge * float64(time.Second))
	g.OldestUnknownPayoutAge = time.Duration(payoutAge * float64(time.Second))

	var paymentWait float64
	if err := q.db.QueryRow(ctx, `
		SELECT count(*), coalesce(extract(epoch FROM $1 - min(state_changed_at)), 0)
		  FROM lending.external_payments WHERE state IN ('VERIFIED','CREDITED')`, now).
		Scan(&g.ConfirmedPaymentsInFlight, &paymentWait); err != nil {
		return g, err
	}
	g.OldestConfirmedPaymentWait = time.Duration(paymentWait * float64(time.Second))

	rows, err := q.db.Query(ctx, `SELECT kind, count(*) FROM lending.recon_exceptions WHERE state = 'OPEN' GROUP BY kind`)
	if err != nil {
		return g, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			kind string
			n    int
		)
		if err := rows.Scan(&kind, &n); err != nil {
			return g, err
		}
		g.OpenExceptionsByKind[kind] = n
	}
	return g, rows.Err()
}
