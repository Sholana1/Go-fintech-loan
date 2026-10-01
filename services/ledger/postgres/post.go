package postgres

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"bankplatform.internal/platform/money"
	"bankplatform.internal/platform/outbox"
	"bankplatform.internal/platform/pgxutil"
	"bankplatform.internal/services/ledger/contract"
	"bankplatform.internal/services/ledger/domain"
)

// PostCommand is a request to post one journal.
type PostCommand struct {
	Ref          domain.PostingRef
	JournalType  string
	Lines        []domain.Line
	BusinessDate *time.Time // nil = posting date in Africa/Lagos
	Narrative    string
	Caller       string
}

// PostJournal atomically checks and posts a journal. See the package comment
// for the protocol.
func (s *Store) PostJournal(ctx context.Context, cmd PostCommand) (domain.Journal, error) {
	var out domain.Journal
	err := s.tx.InTx(ctx, func(tx pgx.Tx) error {
		existing, claimed, err := claimRef(ctx, tx, cmd.Ref, domain.Fingerprint(cmd.JournalType, cmd.Lines))
		if err != nil {
			return err
		}
		if !claimed {
			out = existing
			return nil
		}
		out, err = s.post(ctx, tx, cmd)
		return err
	})
	return out, err
}

// claimRef inserts the posting reference. If it already exists (committed by
// an earlier or concurrent request, which this statement waits for), it
// returns the original journal, or ErrRefReused when the content differs.
func claimRef(ctx context.Context, tx pgx.Tx, ref domain.PostingRef, hash []byte) (domain.Journal, bool, error) {
	tag, err := tx.Exec(ctx, `
		INSERT INTO ledger.posting_refs (op_type, op_id, op_step, request_hash)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT DO NOTHING`, ref.OpType, ref.OpID, ref.OpStep, hash)
	if err != nil {
		return domain.Journal{}, false, err
	}
	if tag.RowsAffected() == 1 {
		return domain.Journal{}, true, nil
	}

	var (
		storedHash []byte
		j          domain.Journal
	)
	err = tx.QueryRow(ctx, `
		SELECT r.request_hash, j.journal_id, j.journal_type, j.posted_at, j.business_date, j.created_by
		  FROM ledger.posting_refs r
		  JOIN ledger.journals j ON j.journal_id = r.journal_id AND j.posted_at = r.posted_at
		 WHERE r.op_type = $1 AND r.op_id = $2 AND r.op_step = $3`,
		ref.OpType, ref.OpID, ref.OpStep).Scan(&storedHash, &j.ID, &j.Type, &j.PostedAt, &j.BusinessDate, &j.CreatedBy)
	if err != nil {
		return domain.Journal{}, false, fmt.Errorf("read existing posting reference: %w", err)
	}
	if !bytes.Equal(storedHash, hash) {
		return domain.Journal{}, false, domain.ErrRefReused
	}
	j.AlreadyPosted = true
	return j, false, nil
}

// lockedAccount is an account row read under its balance row lock.
type lockedAccount struct {
	id         int64
	code       string
	currency   money.Currency
	status     string
	normalSide domain.Direction
	posted     int64
	held       int64
	floor      *int64
}

// lockAccounts locks the balance rows of the given accounts in the
// platform-wide deterministic order and returns them keyed by code.
func lockAccounts(ctx context.Context, tx pgx.Tx, codes []string) (map[string]*lockedAccount, []*lockedAccount, error) {
	rows, err := tx.Query(ctx, `
		SELECT a.account_id, a.code, a.currency, a.status, a.normal_side::text, b.posted, b.held, b.floor
		  FROM ledger.accounts a
		  JOIN ledger.balances b USING (account_id)
		 WHERE a.code = ANY($1)
		 ORDER BY (a.owner_type = 'SYSTEM'), a.account_id
		   FOR UPDATE OF b`, codes)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	byCode := make(map[string]*lockedAccount, len(codes))
	var ordered []*lockedAccount
	for rows.Next() {
		a := &lockedAccount{}
		var cur, side string
		if err := rows.Scan(&a.id, &a.code, &cur, &a.status, &side, &a.posted, &a.held, &a.floor); err != nil {
			return nil, nil, err
		}
		a.currency = money.Currency(cur)
		a.normalSide = domain.Direction(side)
		byCode[a.code] = a
		ordered = append(ordered, a)
	}
	return byCode, ordered, rows.Err()
}

// post performs steps 2-5 of the protocol. It reads balances under lock, so
// it sees changes made earlier in the same transaction (for example a hold
// closed by CaptureHold, whose amount no longer counts against available).
func (s *Store) post(ctx context.Context, tx pgx.Tx, cmd PostCommand) (domain.Journal, error) {
	codes := distinctCodes(cmd.Lines)
	byCode, ordered, err := lockAccounts(ctx, tx, codes)
	if err != nil {
		return domain.Journal{}, err
	}

	// Net effect per account, signed in the account's normal side.
	net := make(map[int64]int64, len(ordered))
	for i, l := range cmd.Lines {
		a, ok := byCode[l.AccountCode]
		if !ok {
			return domain.Journal{}, fmt.Errorf("%w: %s", domain.ErrAccountNotFound, l.AccountCode)
		}
		if a.currency != l.Amount.Currency() {
			return domain.Journal{}, fmt.Errorf("%w: line %d", domain.ErrCurrencyMismatch, i)
		}
		if !statusPermits(a.status, l.Direction) {
			return domain.Journal{}, fmt.Errorf("%w: %s is %s", domain.ErrAccountNotActive, a.code, a.status)
		}
		delta := l.Amount.Minor()
		if l.Direction != a.normalSide {
			delta = -delta
		}
		sum, ok := money.AddInt64(net[a.id], delta)
		if !ok {
			return domain.Journal{}, fmt.Errorf("%w: account total overflows", domain.ErrInvalid)
		}
		net[a.id] = sum
	}

	// Funds check under lock. Only accounts with a floor (customer accounts)
	// are constrained, and only when the journal reduces them.
	for _, a := range ordered {
		if a.floor == nil || net[a.id] >= 0 {
			continue
		}
		if a.posted+net[a.id]-a.held < *a.floor {
			return domain.Journal{}, fmt.Errorf("%w: %s", domain.ErrInsufficientFunds, a.code)
		}
	}

	today := domain.BusinessDate(s.now())
	businessDate := today
	if cmd.BusinessDate != nil {
		if cmd.BusinessDate.After(today) {
			return domain.Journal{}, fmt.Errorf("%w: business date is in the future", domain.ErrInvalid)
		}
		businessDate = *cmd.BusinessDate
	}

	j := domain.Journal{Type: cmd.JournalType, BusinessDate: businessDate, Lines: cmd.Lines, CreatedBy: cmd.Caller}
	err = tx.QueryRow(ctx, `
		INSERT INTO ledger.journals (business_date, journal_type, op_type, op_id, op_step, narrative, created_by)
		VALUES ($1, $2, $3, $4, $5, nullif($6, ''), $7)
		RETURNING journal_id, posted_at`,
		businessDate, cmd.JournalType, cmd.Ref.OpType, cmd.Ref.OpID, cmd.Ref.OpStep, cmd.Narrative, cmd.Caller,
	).Scan(&j.ID, &j.PostedAt)
	if err != nil {
		return domain.Journal{}, fmt.Errorf("insert journal: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE ledger.posting_refs SET journal_id = $1, posted_at = $2
		 WHERE op_type = $3 AND op_id = $4 AND op_step = $5`,
		j.ID, j.PostedAt, cmd.Ref.OpType, cmd.Ref.OpID, cmd.Ref.OpStep); err != nil {
		return domain.Journal{}, fmt.Errorf("link posting reference: %w", err)
	}

	var (
		accountIDs = make([]int64, len(cmd.Lines))
		currencies = make([]string, len(cmd.Lines))
		directions = make([]string, len(cmd.Lines))
		amounts    = make([]int64, len(cmd.Lines))
		eventLines = make([]contract.JournalPostedLine, len(cmd.Lines))
	)
	for i, l := range cmd.Lines {
		accountIDs[i] = byCode[l.AccountCode].id
		currencies[i] = string(l.Amount.Currency())
		directions[i] = string(l.Direction)
		amounts[i] = l.Amount.Minor()
		eventLines[i] = contract.JournalPostedLine{AccountCode: l.AccountCode, Direction: string(l.Direction), AmountMinor: l.Amount.Minor(), Currency: string(l.Amount.Currency())}
	}
	// The BEFORE INSERT trigger applies each entry to its (already locked)
	// balance row and stamps balance_after; the 0 supplied here is replaced.
	if _, err := tx.Exec(ctx, `
		INSERT INTO ledger.entries (journal_id, posted_at, account_id, currency, direction, amount, balance_after)
		SELECT $1, $2, t.account_id, t.currency, t.direction::ledger.side, t.amount, 0
		  FROM unnest($3::bigint[], $4::text[], $5::text[], $6::bigint[])
		       WITH ORDINALITY AS t(account_id, currency, direction, amount, ord)
		 ORDER BY t.ord`,
		j.ID, j.PostedAt, accountIDs, currencies, directions, amounts); err != nil {
		if pgxutil.Constraint(err) == "available_not_below_floor" {
			// The backstop fired: the application check above missed a case.
			return domain.Journal{}, fmt.Errorf("%w (database constraint)", domain.ErrInsufficientFunds)
		}
		return domain.Journal{}, fmt.Errorf("insert entries: %w", err)
	}

	if _, err := outbox.Insert(ctx, tx, outboxTable, outbox.Event{
		Topic:            contract.TopicJournals,
		PartitionKey:     cmd.Ref.OpID.String(),
		Type:             contract.EventJournalPosted,
		SchemaVersion:    1,
		AggregateType:    "journal",
		AggregateID:      fmt.Sprintf("%d", j.ID),
		AggregateVersion: 1,
		Data: contract.JournalPosted{
			JournalID:    j.ID,
			JournalType:  cmd.JournalType,
			BusinessDate: businessDate.Format(time.DateOnly),
			OpType:       cmd.Ref.OpType,
			OpID:         cmd.Ref.OpID.String(),
			OpStep:       cmd.Ref.OpStep,
			Lines:        eventLines,
		},
	}); err != nil {
		return domain.Journal{}, err
	}
	return j, nil
}

// statusPermits applies the account-status rules: a frozen or closed account
// takes no entries; post-no-debit accepts credits only.
func statusPermits(status string, d domain.Direction) bool {
	switch status {
	case "ACTIVE":
		return true
	case "POST_NO_DEBIT":
		return d == domain.Credit
	default:
		return false
	}
}

func distinctCodes(lines []domain.Line) []string {
	seen := make(map[string]struct{}, len(lines))
	var out []string
	for _, l := range lines {
		if _, ok := seen[l.AccountCode]; !ok {
			seen[l.AccountCode] = struct{}{}
			out = append(out, l.AccountCode)
		}
	}
	return out
}
