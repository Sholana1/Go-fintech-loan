package httpapi

import (
	"time"

	"bankplatform.internal/services/lending/app"
	"bankplatform.internal/services/lending/domain"
)

type instalmentJSON struct {
	Seq            int    `json:"seq"`
	DueDate        string `json:"due_date"`
	PrincipalMinor int64  `json:"principal_due_minor"`
	InterestMinor  int64  `json:"interest_due_minor"`
	FeesMinor      int64  `json:"fees_due_minor"`
	PrincipalPaid  int64  `json:"principal_paid_minor"`
	InterestPaid   int64  `json:"interest_paid_minor"`
	FeesPaid       int64  `json:"fees_paid_minor"`
	Paid           bool   `json:"paid"`
}

func instalmentsJSON(insts []domain.Instalment) []instalmentJSON {
	out := make([]instalmentJSON, 0, len(insts))
	for _, i := range insts {
		out = append(out, instalmentJSON{
			Seq: i.Seq, DueDate: i.DueDate.Format(time.DateOnly),
			PrincipalMinor: i.PrincipalDue, InterestMinor: i.InterestDue, FeesMinor: i.FeesDue,
			PrincipalPaid: i.PrincipalPaid, InterestPaid: i.InterestPaid, FeesPaid: i.FeesPaid, Paid: i.FullyPaid(),
		})
	}
	return out
}

type loanSummary struct {
	LoanID         string     `json:"loan_id"`
	Status         string     `json:"status"`
	PrincipalMinor int64      `json:"principal_minor"`
	TenorMonths    int        `json:"tenor_months"`
	MonthlyRateBps int        `json:"monthly_rate_bps"`
	DisbursedAt    *time.Time `json:"disbursed_at,omitempty"`
	DaysPastDue    int        `json:"days_past_due"`
	ClosedAt       *time.Time `json:"closed_at,omitempty"`
}

func summariseLoan(l domain.Loan) loanSummary {
	return loanSummary{
		LoanID: l.ID.String(), Status: loanStatus(l.State), PrincipalMinor: l.PrincipalMinor, TenorMonths: l.TenorMonths,
		MonthlyRateBps: l.MonthlyRateBps, DisbursedAt: l.DisbursedAt, DaysPastDue: l.DaysPastDue, ClosedAt: l.ClosedAt,
	}
}

type payoutJSON struct {
	Status      string `json:"status"`
	AmountMinor int64  `json:"amount_minor"`
	BankCode    string `json:"bank_code"`
	// Only the last four digits of the account number are returned.
	AccountLast4 string `json:"account_last4"`
}

type loanResponse struct {
	loanSummary
	ApplicationID       string           `json:"application_id"`
	OriginationFeeMinor int64            `json:"origination_fee_minor"`
	AutoDebitAuthorised bool             `json:"auto_debit_authorised"`
	ArrearsBucket       string           `json:"arrears_bucket"`
	Restructured        bool             `json:"restructured"`
	ScheduleVersion     int              `json:"schedule_version"`
	Instalments         []instalmentJSON `json:"instalments"`
	Payout              *payoutJSON      `json:"payout,omitempty"`
}

func loanJSON(v app.LoanView) loanResponse {
	out := loanResponse{
		loanSummary: summariseLoan(v.Loan), ApplicationID: v.Loan.ApplicationID.String(),
		OriginationFeeMinor: v.Loan.OriginationFeeMinor, AutoDebitAuthorised: v.Loan.AutoDebitAuthorised,
		ArrearsBucket: v.Loan.ArrearsBucket, Restructured: v.Loan.Restructured, ScheduleVersion: v.Loan.ScheduleVersion,
		Instalments: instalmentsJSON(v.Instalments),
	}
	if v.Payout != nil {
		out.Payout = payoutToJSON(*v.Payout)
	}
	return out
}

func payoutToJSON(p domain.Payout) *payoutJSON {
	last4 := p.AccountNumber
	if len(last4) > 4 {
		last4 = last4[len(last4)-4:]
	}
	return &payoutJSON{Status: payoutStatus(p.State), AmountMinor: p.AmountMinor, BankCode: p.BankCode, AccountLast4: last4}
}

type payoffJSON struct {
	AsOf           string `json:"as_of"`
	Currency       string `json:"currency"`
	PrincipalMinor int64  `json:"principal_minor"`
	InterestMinor  int64  `json:"interest_minor"`
	FeesMinor      int64  `json:"fees_minor"`
	TotalMinor     int64  `json:"total_minor"`
}

type repaymentResponse struct {
	RepaymentID    string     `json:"repayment_id"`
	Status         string     `json:"status"`
	Source         string     `json:"source"`
	RequestedMinor int64      `json:"requested_minor"`
	AppliedMinor   int64      `json:"applied_minor"`
	UnappliedMinor int64      `json:"unapplied_minor"`
	FeesMinor      int64      `json:"fees_minor"`
	InterestMinor  int64      `json:"interest_minor"`
	PrincipalMinor int64      `json:"principal_minor"`
	SettlesLoan    bool       `json:"settles_loan"`
	CreatedAt      time.Time  `json:"created_at"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
}

func repaymentJSON(r domain.Repayment) repaymentResponse {
	status := "PROCESSING"
	switch r.State {
	case domain.RepaymentAllocated:
		status = "SUCCESSFUL"
	case domain.RepaymentRejected:
		status = "FAILED"
	}
	out := repaymentResponse{
		RepaymentID: r.ID.String(), Status: status, Source: r.Source, RequestedMinor: r.RequestedMinor,
		AppliedMinor: r.AppliedMinor, UnappliedMinor: r.UnappliedMinor,
		FeesMinor: r.Allocation.Fees, InterestMinor: r.Allocation.Interest, PrincipalMinor: r.Allocation.Principal,
		SettlesLoan: r.Allocation.Settles, CreatedAt: r.CreatedAt.UTC(),
	}
	if r.CompletedAt != nil {
		done := r.CompletedAt.UTC()
		out.CompletedAt = &done
	}
	if r.IsRecovery {
		// A recovery is not allocated to a schedule.
		out.PrincipalMinor, out.InterestMinor, out.FeesMinor = 0, 0, 0
	}
	return out
}

type feeJSON struct {
	Kind        string    `json:"kind"`
	DueDate     string    `json:"instalment_due_date"`
	AmountMinor int64     `json:"amount_minor"`
	ChargedAt   time.Time `json:"charged_at"`
}

type statementResponse struct {
	AsOf       string              `json:"as_of"`
	Currency   string              `json:"currency"`
	Loan       loanResponse        `json:"loan"`
	Repayments []repaymentResponse `json:"repayments"`
	Fees       []feeJSON           `json:"fees"`
	Totals     statementTotals     `json:"totals"`
	Payoff     *payoffJSON         `json:"payoff,omitempty"`
}

type statementTotals struct {
	PrincipalOutstandingMinor int64 `json:"principal_outstanding_minor"`
	PrincipalPaidMinor        int64 `json:"principal_paid_minor"`
	InterestPaidMinor         int64 `json:"interest_paid_minor"`
	InterestEarnedMinor       int64 `json:"interest_earned_to_date_minor"`
	FeesChargedMinor          int64 `json:"fees_charged_minor"`
	FeesPaidMinor             int64 `json:"fees_paid_minor"`
}

func statementJSON(st app.Statement, currency string) statementResponse {
	out := statementResponse{
		AsOf: st.AsOf.Format(time.DateOnly), Currency: currency,
		Loan:       loanJSON(app.LoanView{Loan: st.Loan, Instalments: st.Instalments, Payout: st.Payout}),
		Repayments: make([]repaymentResponse, 0, len(st.Repayments)),
		Fees:       make([]feeJSON, 0, len(st.Fees)),
	}
	for _, r := range st.Repayments {
		out.Repayments = append(out.Repayments, repaymentJSON(r))
	}
	for _, f := range st.Fees {
		if f.State != "POSTED" {
			continue
		}
		out.Fees = append(out.Fees, feeJSON{Kind: f.Kind, DueDate: f.DueDate.Format(time.DateOnly), AmountMinor: f.AmountMinor, ChargedAt: f.CreatedAt.UTC()})
	}
	for _, i := range st.Instalments {
		out.Totals.PrincipalOutstandingMinor += i.PrincipalOutstanding()
		out.Totals.PrincipalPaidMinor += i.PrincipalPaid
		out.Totals.InterestPaidMinor += i.InterestPaid
		out.Totals.InterestEarnedMinor += i.EntitledInterest(st.AsOf)
		out.Totals.FeesChargedMinor += i.FeesDue
		out.Totals.FeesPaidMinor += i.FeesPaid
	}
	if st.Payoff != nil {
		out.Payoff = &payoffJSON{AsOf: out.AsOf, Currency: currency, PrincipalMinor: st.Payoff.Principal,
			InterestMinor: st.Payoff.Interest, FeesMinor: st.Payoff.Fees, TotalMinor: st.Payoff.Total()}
	}
	return out
}
