package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"bankplatform.internal/platform/money"
	"bankplatform.internal/services/ledger/domain"
)

// GetJournal returns a journal and its lines by posting reference.
func (s *Store) GetJournal(ctx context.Context, ref domain.PostingRef) (domain.Journal, error) {
	var j domain.Journal
	err := s.pool.QueryRow(ctx, `
		SELECT j.journal_id, j.journal_type, j.posted_at, j.business_date, j.created_by
		  FROM ledger.posting_refs r
		  JOIN ledger.journals j ON j.journal_id = r.journal_id AND j.posted_at = r.posted_at
		 WHERE r.op_type = $1 AND r.op_id = $2 AND r.op_step = $3`,
		ref.OpType, ref.OpID, ref.OpStep).Scan(&j.ID, &j.Type, &j.PostedAt, &j.BusinessDate, &j.CreatedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Journal{}, domain.ErrJournalNotFound
	}
	if err != nil {
		return domain.Journal{}, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT a.code, e.direction::text, e.amount, e.currency
		  FROM ledger.entries e JOIN ledger.accounts a USING (account_id)
		 WHERE e.journal_id = $1 AND e.posted_at = $2
		 ORDER BY e.entry_id`, j.ID, j.PostedAt)
	if err != nil {
		return domain.Journal{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			code, dir, cur string
			amount         int64
		)
		if err := rows.Scan(&code, &dir, &amount, &cur); err != nil {
			return domain.Journal{}, err
		}
		amt, err := money.New(amount, money.Currency(cur))
		if err != nil {
			return domain.Journal{}, err
		}
		j.Lines = append(j.Lines, domain.Line{AccountCode: code, Direction: domain.Direction(dir), Amount: amt})
	}
	return j, rows.Err()
}

// LoadRights returns every (caller, action) pair. Rights change only through
// migrations, so the service loads them once at startup.
func (s *Store) LoadRights(ctx context.Context) (map[string]map[string]bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT caller, action FROM ledger.posting_rights`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	rights := map[string]map[string]bool{}
	for rows.Next() {
		var caller, action string
		if err := rows.Scan(&caller, &action); err != nil {
			return nil, err
		}
		if rights[caller] == nil {
			rights[caller] = map[string]bool{}
		}
		rights[caller][action] = true
	}
	return rights, rows.Err()
}

// JournalPolicy is the permitted shape of one journal type.
type JournalPolicy struct {
	MaxCustomerAccounts int
	SystemAccounts      map[string]bool
}

// LoadJournalPolicies returns the permitted shape of every journal type.
// Like rights, policies change only through migrations.
func (s *Store) LoadJournalPolicies(ctx context.Context) (map[string]JournalPolicy, error) {
	rows, err := s.pool.Query(ctx, `SELECT journal_type, max_customer_accounts, system_accounts FROM ledger.journal_types`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]JournalPolicy{}
	for rows.Next() {
		var (
			jt  string
			max int
			sys []string
			pol = JournalPolicy{SystemAccounts: map[string]bool{}}
		)
		if err := rows.Scan(&jt, &max, &sys); err != nil {
			return nil, err
		}
		pol.MaxCustomerAccounts = max
		for _, code := range sys {
			pol.SystemAccounts[code] = true
		}
		out[jt] = pol
	}
	return out, rows.Err()
}
