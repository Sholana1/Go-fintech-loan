package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"bankplatform.internal/services/lending/domain"
	"bankplatform.internal/services/lending/postgres"
)

// ReconExceptions lists reconciliation exceptions in the given state.
func (s *Service) ReconExceptions(ctx context.Context, state string) ([]domain.ReconException, error) {
	if state != "OPEN" && state != "RESOLVED" {
		return nil, fmt.Errorf("%w: state must be OPEN or RESOLVED", domain.ErrValidation)
	}
	return s.store.Exceptions(ctx, state, 200)
}

// ResolveReconException closes an exception with a note explaining what was
// found and done. It records who resolved it. It does not itself change any
// money: a correcting entry, if one is needed, is its own audited action.
func (s *Service) ResolveReconException(ctx context.Context, staffID, exceptionID uuid.UUID, note string) error {
	note = strings.TrimSpace(note)
	if len(note) < 10 {
		return fmt.Errorf("%w: a resolution note of at least 10 characters is required", domain.ErrValidation)
	}
	return s.store.InTx(ctx, func(q *postgres.Queries) error {
		ok, err := q.ResolveException(ctx, exceptionID, staffID.String(), note, s.now())
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: exception is not open", domain.ErrConflict)
		}
		return q.Audit(ctx, postgres.Actor{Kind: "staff", ID: staffID.String()}, "RECON_EXCEPTION_RESOLVED", "recon_exception", exceptionID.String(),
			map[string]any{"note": note})
	})
}

// PortfolioReport returns the loan book grouped by state and arrears bucket.
func (s *Service) PortfolioReport(ctx context.Context) ([]postgres.PortfolioRow, error) {
	return s.store.PortfolioReport(ctx)
}

// OpsLoan returns any loan with its schedule, for staff.
func (s *Service) OpsLoan(ctx context.Context, loanID uuid.UUID) (LoanView, error) {
	loan, err := s.store.GetLoan(ctx, loanID)
	if err != nil {
		return LoanView{}, err
	}
	return s.loanView(ctx, loan)
}
