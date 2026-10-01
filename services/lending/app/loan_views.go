package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"bankplatform.internal/services/lending/domain"
	"bankplatform.internal/services/lending/postgres"
)

// LoanView is a loan with its current schedule.
type LoanView struct {
	Loan        domain.Loan
	Instalments []domain.Instalment
	Payout      *domain.Payout
}

// GetLoan returns the customer's own loan.
func (s *Service) GetLoan(ctx context.Context, customerID, loanID uuid.UUID) (LoanView, error) {
	loan, err := s.store.GetLoanForCustomer(ctx, loanID, customerID)
	if err != nil {
		return LoanView{}, err
	}
	return s.loanView(ctx, loan)
}

func (s *Service) loanView(ctx context.Context, loan domain.Loan) (LoanView, error) {
	v := LoanView{Loan: loan}
	var err error
	if v.Instalments, err = s.store.Instalments(ctx, loan.ID, loan.ScheduleVersion); err != nil {
		return v, err
	}
	if p, err := s.store.PayoutByLoan(ctx, loan.ID); err == nil {
		v.Payout = &p
	} else if !errors.Is(err, domain.ErrNotFound) {
		return v, err
	}
	return v, nil
}

// PayoffQuote returns what it would cost to settle the loan today.
func (s *Service) PayoffQuote(ctx context.Context, customerID, loanID uuid.UUID) (domain.PayoffQuote, time.Time, error) {
	loan, err := s.store.GetLoanForCustomer(ctx, loanID, customerID)
	if err != nil {
		return domain.PayoffQuote{}, time.Time{}, err
	}
	if !loan.State.Servicing() {
		return domain.PayoffQuote{}, time.Time{}, fmt.Errorf("%w: loan is %s", domain.ErrLoanNotRepayable, loan.State)
	}
	insts, err := s.store.Instalments(ctx, loan.ID, loan.ScheduleVersion)
	if err != nil {
		return domain.PayoffQuote{}, time.Time{}, err
	}
	today := s.today()
	return domain.Payoff(insts, today), today, nil
}

// Statement is a loan's complete history for the customer.
type Statement struct {
	Loan            domain.Loan
	Instalments     []domain.Instalment
	Repayments      []domain.Repayment
	Fees            []postgres.LoanFee
	InterestAccrued int64 // recognised to date
	Payoff          *domain.PayoffQuote
	AsOf            time.Time
	Payout          *domain.Payout
}

// GetStatement assembles the customer's loan statement.
func (s *Service) GetStatement(ctx context.Context, customerID, loanID uuid.UUID) (Statement, error) {
	view, err := s.GetLoan(ctx, customerID, loanID)
	if err != nil {
		return Statement{}, err
	}
	st := Statement{Loan: view.Loan, Instalments: view.Instalments, Payout: view.Payout, AsOf: s.today()}
	if st.Repayments, err = s.store.Repayments(ctx, loanID); err != nil {
		return st, err
	}
	if st.Fees, err = s.store.LoanFees(ctx, loanID); err != nil {
		return st, err
	}
	for _, i := range view.Instalments {
		st.InterestAccrued += i.InterestAccrued
	}
	if view.Loan.State.Servicing() {
		q := domain.Payoff(view.Instalments, st.AsOf)
		st.Payoff = &q
	}
	return st, nil
}

// ListLoans returns the customer's loans, newest first.
func (s *Service) ListLoans(ctx context.Context, customerID uuid.UUID) ([]domain.Loan, error) {
	return s.store.LoansForCustomer(ctx, customerID, 50)
}
