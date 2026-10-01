package postgres

import (
	"context"
	"time"
)

// Violations counts breaches of the ledger's invariants. Every field should
// always be zero; a non-zero value is a paging alert, not a warning.
type Violations struct {
	UnbalancedJournals   int64 // journals whose debits and credits differ in some currency
	BalanceDrift         int64 // accounts whose posted balance differs from the sum of their entries
	RunningBalanceBreaks int64 // accounts whose latest entry's balance_after differs from posted
	HeldDrift            int64 // accounts whose held differs from the sum of their active holds
	DefaultPartitionRows int64 // rows that landed in a default partition (a month partition is missing)
	// TrialBalanceDifference is the sum of debit-normal balances minus the sum
	// of credit-normal balances, across all accounts. Double entry makes it 0.
	TrialBalanceDifference int64
}

// Total returns the sum of all violation counts.
func (v Violations) Total() int64 {
	diff := v.TrialBalanceDifference
	if diff < 0 {
		diff = -diff
	}
	return v.UnbalancedJournals + v.BalanceDrift + v.RunningBalanceBreaks + v.HeldDrift + v.DefaultPartitionRows + diff
}

// Verify recomputes the invariants from the immutable entries. Journals are
// checked from `since`; balances are checked for every account.
//
// This is a full recomputation and is appropriate for the current data
// volume and for tests. At scale it should run against a replica on a
// schedule, with the cheap running-balance check kept on the primary.
func (s *Store) Verify(ctx context.Context, since time.Time) (Violations, error) {
	var v Violations
	err := s.pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM (
		     SELECT journal_id FROM ledger.entries WHERE posted_at >= $1
		      GROUP BY journal_id, currency
		     HAVING sum(CASE direction WHEN 'DEBIT' THEN amount ELSE -amount END) <> 0) u),
		  (SELECT count(*) FROM ledger.balances b
		     JOIN ledger.accounts a USING (account_id)
		     LEFT JOIN (SELECT e.account_id,
		                       sum(CASE WHEN e.direction = a2.normal_side THEN e.amount ELSE -e.amount END) AS total
		                  FROM ledger.entries e JOIN ledger.accounts a2 USING (account_id)
		                 GROUP BY e.account_id) r USING (account_id)
		    WHERE b.posted <> coalesce(r.total, 0)),
		  (SELECT count(*) FROM ledger.balances b
		     JOIN LATERAL (SELECT balance_after FROM ledger.entries
		                    WHERE account_id = b.account_id
		                    ORDER BY entry_id DESC LIMIT 1) last ON true
		    WHERE last.balance_after <> b.posted),
		  (SELECT count(*) FROM ledger.balances b
		     LEFT JOIN (SELECT account_id, sum(amount) AS total FROM ledger.holds
		                 WHERE status = 'ACTIVE' GROUP BY account_id) h USING (account_id)
		    WHERE b.held <> coalesce(h.total, 0)),
		  (SELECT count(*) FROM ledger.entries_default) + (SELECT count(*) FROM ledger.journals_default),
		  (SELECT coalesce(sum(CASE a.normal_side WHEN 'DEBIT' THEN b.posted ELSE -b.posted END), 0)::bigint
		     FROM ledger.balances b JOIN ledger.accounts a USING (account_id))`,
		since).Scan(&v.UnbalancedJournals, &v.BalanceDrift, &v.RunningBalanceBreaks, &v.HeldDrift, &v.DefaultPartitionRows, &v.TrialBalanceDifference)
	return v, err
}
