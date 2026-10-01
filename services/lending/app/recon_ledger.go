package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"bankplatform.internal/services/ledger/contract"
	"bankplatform.internal/services/lending/domain"
	"bankplatform.internal/services/lending/postgres"
)

// Reconciliation.
//
// Two independent comparisons, each of which opens an exception (never a
// silent correction) when the two sides disagree:
//
//   - ReconcileLedger: lending's loan book against the ledger's control
//     accounts, and each recent posting against its journal.
//   - ReconcilePayouts: our payout records against the provider's settlement
//     report, which is also where unknown outcomes are finally resolved and
//     where settled transfers are posted against cash at the settlement bank.
//
// The recon service of the architecture plan does not exist yet (it arrives
// with transfers). Until then these two jobs live in lending because lending
// is the only product posting to these accounts; see docs/adr.

// LedgerReconResult summarises one ledger reconciliation pass.
type LedgerReconResult struct {
	Skipped         bool   `json:"skipped"`
	SkipReason      string `json:"skip_reason,omitempty"`
	ControlBreaks   int    `json:"control_breaks"`
	JournalsChecked int    `json:"journals_checked"`
	JournalBreaks   int    `json:"journal_breaks"`
}

// ReconcileLedger compares lending's subledger with the ledger.
//
// The control-account comparison is only meaningful at a quiet moment: while
// a posting is in flight the two sides legitimately differ. The pass
// therefore reads lending's totals, then the ledger, then lending's totals
// again, and compares only if nothing was pending and lending's totals did
// not move in between. Otherwise it reports "skipped" and tries again later.
func (s *Service) ReconcileLedger(ctx context.Context, journalsSince time.Time) (LedgerReconResult, error) {
	var res LedgerReconResult

	// books is everything lending believes the control accounts hold.
	type books struct {
		postgres.SubledgerTotals
		CollectionsCreditedMinor int64
	}
	quiet := func() (books, bool, string, error) {
		pending, err := s.store.PendingPostingCount(ctx)
		if err != nil {
			return books{}, false, "", err
		}
		if pending > 0 {
			return books{}, false, "postings in flight", nil
		}
		unposted, err := s.store.UnpostedAccrualDates(ctx)
		if err != nil {
			return books{}, false, "", err
		}
		if unposted > 0 {
			return books{}, false, "accrual run not yet posted", nil
		}
		collections, err := s.store.CollectionsTotals(ctx)
		if err != nil {
			return books{}, false, "", err
		}
		if collections.InFlight > 0 {
			return books{}, false, "external payment credit in flight", nil
		}
		t, err := s.store.SubledgerTotals(ctx)
		return books{SubledgerTotals: t, CollectionsCreditedMinor: collections.CreditedMinor}, true, "", err
	}

	before, ok, why, err := quiet()
	if err != nil {
		return res, err
	}
	if !ok {
		res.Skipped, res.SkipReason = true, why
		return res, nil
	}

	controls := []struct {
		account   string
		subledger int64
	}{
		{contract.AccountLoansPrincipal, before.PrincipalMinor},
		{contract.AccountLoansInterestReceivable, before.InterestReceivableMinor},
		{contract.AccountLoansFeesReceivable, before.FeesReceivableMinor},
		// What payment providers owe us for confirmed customer payments.
		// Provider settlement postings (clearing -> cash at bank) do not
		// exist yet, so the account holds every confirmed payment.
		{contract.AccountCollectionsClearing, before.CollectionsCreditedMinor},
	}
	ledgerBalances := make([]int64, len(controls))
	for i, c := range controls {
		balCtx, cancel := context.WithTimeout(ctx, s.cfg.LedgerTimeout)
		ledgerBalances[i], err = s.ledger.Posted(balCtx, c.account)
		cancel()
		if err != nil {
			return res, fmt.Errorf("read ledger balance of %s: %w", c.account, err)
		}
	}

	after, ok, why, err := quiet()
	if err != nil {
		return res, err
	}
	if !ok || after != before {
		res.Skipped, res.SkipReason = true, orDefault(why, "loan book changed during the pass")
		return res, nil
	}

	now := s.now()
	for i, c := range controls {
		if diff := ledgerBalances[i] - c.subledger; diff != 0 {
			res.ControlBreaks++
			if err := s.raise(ctx, domain.ExceptionControlAccount, "ledger_account", c.account, diff,
				map[string]any{"ledger_minor": ledgerBalances[i], "subledger_minor": c.subledger}); err != nil {
				return res, err
			}
		} else if err := s.store.ResolveExceptionsFor(ctx, domain.ExceptionControlAccount, "ledger_account", c.account, "control account agrees with the loan book", now); err != nil {
			return res, err
		}
	}

	// Journal-level check: every posting lending believes it made exists in
	// the ledger with exactly the lines lending asked for.
	intents, err := s.store.RecentPostedIntents(ctx, journalsSince, 1000)
	if err != nil {
		return res, err
	}
	for _, in := range intents {
		jCtx, cancel := context.WithTimeout(ctx, s.cfg.LedgerTimeout)
		j, err := s.ledger.Journal(jCtx, JournalRef{OpType: intentOpType(in.Kind), OpID: in.ID, OpStep: "POST"})
		cancel()
		res.JournalsChecked++
		problem := ""
		switch {
		case errors.Is(err, ErrLedgerNotFound):
			problem = "journal missing in ledger"
		case err != nil:
			return res, fmt.Errorf("read journal for intent %s: %w", in.ID, err)
		case in.JournalID == nil || j.ID != *in.JournalID:
			problem = "journal id differs"
		case !sameLines(in.Lines, j.Lines):
			problem = "journal lines differ"
		}
		if problem != "" {
			res.JournalBreaks++
			if err := s.raise(ctx, domain.ExceptionJournalMismatch, "posting_intent", in.ID.String(), 0,
				map[string]any{"kind": string(in.Kind), "problem": problem}); err != nil {
				return res, err
			}
		}
	}
	return res, nil
}

func sameLines(a, b []domain.JournalLine) bool {
	if len(a) != len(b) {
		return false
	}
	key := func(l domain.JournalLine) string {
		return fmt.Sprintf("%s|%s|%d", l.AccountCode, l.Direction, l.AmountMinor)
	}
	x, y := make([]string, len(a)), make([]string, len(b))
	for i := range a {
		x[i], y[i] = key(a[i]), key(b[i])
	}
	sort.Strings(x)
	sort.Strings(y)
	return slices.Equal(x, y)
}
