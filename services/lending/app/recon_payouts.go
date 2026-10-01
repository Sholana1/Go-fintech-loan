package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"bankplatform.internal/services/ledger/contract"
	"bankplatform.internal/services/lending/domain"
	"bankplatform.internal/services/lending/postgres"
)

// PayoutReconResult summarises one payout reconciliation pass.
type PayoutReconResult struct {
	ReportItems int `json:"report_items"`
	Resolved    int `json:"resolved"`
	Settled     int `json:"settled"`
	Exceptions  int `json:"exceptions"`
}

// ReconcilePayouts compares our payouts with the provider's settlement
// report for one business date.
//
// For each line of the report:
//   - a reference we do not know is an exception;
//   - an amount that differs from ours is an exception;
//   - its outcome is applied through the same applyProviderOutcome used
//     everywhere else, which resolves payouts still UNKNOWN, detects a late
//     success after a recorded failure, and detects a reported failure after
//     a capture;
//   - a transfer the provider marks settled, which we hold as SUCCEEDED, is
//     settled in the ledger: Dr payout clearing, Cr cash at settlement bank.
//
// For each of our payouts from that date that the report does not mention:
//   - SUCCEEDED but absent is an exception;
//   - still UNKNOWN and absent is released as failed only if the provider's
//     contract makes the report final (AbsentFromReportMeansFailed) and the
//     date is closed; otherwise it stays unknown and the exception stays open.
func (s *Service) ReconcilePayouts(ctx context.Context, businessDate time.Time) (PayoutReconResult, error) {
	var res PayoutReconResult
	if s.payouts == nil {
		return res, nil
	}
	reportCtx, cancel := context.WithTimeout(ctx, s.cfg.PayoutTimeout)
	items, err := s.payouts.SettlementReport(reportCtx, businessDate)
	cancel()
	if err != nil {
		return res, fmt.Errorf("fetch settlement report: %w", err)
	}
	res.ReportItems = len(items)
	seen := make(map[string]bool, len(items))

	for _, item := range items {
		seen[item.Reference] = true
		p, err := s.store.PayoutByReference(ctx, item.Reference)
		if errors.Is(err, domain.ErrNotFound) {
			res.Exceptions++
			if err := s.raise(ctx, domain.ExceptionUnknownAtUs, "payout_reference", item.Reference, item.AmountMinor,
				map[string]any{"source": "settlement_report", "provider_ref": item.ProviderRef, "outcome": string(item.Outcome)}); err != nil {
				return res, err
			}
			continue
		}
		if err != nil {
			return res, err
		}
		if item.AmountMinor != p.AmountMinor {
			res.Exceptions++
			if err := s.raise(ctx, domain.ExceptionAmountMismatch, "payout", p.ID.String(), item.AmountMinor-p.AmountMinor,
				map[string]any{"ours_minor": p.AmountMinor, "provider_minor": item.AmountMinor}); err != nil {
				return res, err
			}
			continue // do not act on a line whose amount we do not recognise
		}

		before := p.State
		if err := s.applyProviderOutcome(ctx, p, PayoutResult{Outcome: item.Outcome, ProviderRef: item.ProviderRef, Code: "SETTLEMENT_REPORT"}, "settlement_report"); err != nil {
			return res, err
		}
		if p, err = s.store.GetPayout(ctx, p.ID); err != nil {
			return res, err
		}
		if p.State != before {
			res.Resolved++
		}

		if item.Settled && item.Outcome == domain.ProviderSuccess && p.State == domain.PayoutSucceeded {
			if err := s.settlePayout(ctx, p); err != nil {
				return res, err
			}
			res.Settled++
		}
	}

	closedDate := businessDate.Before(s.today())
	ours, err := s.store.PayoutsInStates(ctx,
		[]domain.PayoutState{domain.PayoutSucceeded, domain.PayoutUnknown, domain.PayoutSending},
		endOfBusinessDate(businessDate), 5000)
	if err != nil {
		return res, err
	}
	for _, p := range ours {
		if seen[p.Reference] || !sameBusinessDate(p.CreatedAt, businessDate) {
			continue
		}
		switch p.State {
		case domain.PayoutSucceeded:
			res.Exceptions++
			if err := s.raise(ctx, domain.ExceptionMissingAtProvider, "payout", p.ID.String(), p.AmountMinor,
				map[string]any{"reference": p.Reference, "report_date": businessDate.Format(time.DateOnly)}); err != nil {
				return res, err
			}
		case domain.PayoutUnknown, domain.PayoutSending:
			if s.cfg.AbsentFromReportMeansFailed && closedDate {
				if err := s.releasePayout(ctx, p, PayoutResult{Outcome: domain.ProviderFailed, Code: "ABSENT_FROM_SETTLEMENT_REPORT"}); err != nil {
					return res, err
				}
				res.Resolved++
			}
		}
	}
	return res, nil
}

// settlePayout posts the settlement of a successful transfer: the bank's
// obligation to the rail (payout clearing) is discharged from cash at the
// settlement bank. Idempotent on the payout id.
func (s *Service) settlePayout(ctx context.Context, p domain.Payout) error {
	ledgerCtx, cancel := context.WithTimeout(ctx, s.cfg.LedgerTimeout)
	_, err := s.ledger.Post(ledgerCtx, PostRequest{
		Ref:         JournalRef{OpType: contract.HoldLoanPayout, OpID: p.ID, OpStep: "SETTLE"},
		JournalType: contract.JournalPayoutSettlement,
		Lines: []domain.JournalLine{
			{AccountCode: contract.AccountPayoutClearing, Direction: "DEBIT", AmountMinor: p.AmountMinor},
			{AccountCode: contract.AccountCashSettlementBank, Direction: "CREDIT", AmountMinor: p.AmountMinor},
		},
	})
	cancel()
	if err != nil {
		return fmt.Errorf("post settlement for payout %s: %w", p.ID, err)
	}
	_, err = s.movePayout(ctx, p, domain.PayoutSettled, postgres.PayoutUpdate{Code: "SETTLED"}, false)
	return err
}
