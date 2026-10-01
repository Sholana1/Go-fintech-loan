package httpapi

import (
	"encoding/json"
	"time"

	"bankplatform.internal/services/lending/app"
	"bankplatform.internal/services/lending/domain"
)

type applicationResponse struct {
	ApplicationID        string         `json:"application_id"`
	Status               string         `json:"status"`
	ProductID            string         `json:"product_id"`
	RequestedAmountMinor int64          `json:"requested_amount_minor"`
	TenorMonths          int            `json:"tenor_months"`
	ReasonCodes          []string       `json:"reason_codes"`
	SubmittedAt          time.Time      `json:"submitted_at"`
	DecidedAt            *time.Time     `json:"decided_at,omitempty"`
	ExpiresAt            time.Time      `json:"expires_at"`
	Offer                *offerResponse `json:"offer,omitempty"`
	LoanID               string         `json:"loan_id,omitempty"`
	// Message explains a waiting state in plain words.
	Message string `json:"message,omitempty"`
}

func applicationJSON(v app.ApplicationView) applicationResponse {
	a := v.Application
	out := applicationResponse{
		ApplicationID: a.ID.String(), Status: applicationStatus(a.State), ProductID: a.ProductID,
		RequestedAmountMinor: a.RequestedPrincipalMinor, TenorMonths: a.TenorMonths,
		ReasonCodes: a.StateReason, SubmittedAt: a.SubmittedAt, DecidedAt: a.DecidedAt, ExpiresAt: a.ExpiresAt,
	}
	if out.ReasonCodes == nil {
		out.ReasonCodes = []string{}
	}
	if v.Offer != nil && (a.State == domain.AppOffered || a.State == domain.AppAccepted || a.State == domain.AppDisbursed) {
		o := offerJSON(*v.Offer)
		out.Offer = &o
	}
	if v.LoanID != nil {
		out.LoanID = v.LoanID.String()
	}
	switch {
	case a.State == domain.AppAssessing && a.WaitingOn != "":
		out.Message = "We are still checking your application. You do not need to apply again."
	case a.State == domain.AppReferred:
		out.Message = "Your application is being reviewed by a member of our team."
	}
	return out
}

type offerResponse struct {
	OfferID                string    `json:"offer_id"`
	Status                 string    `json:"status"`
	Currency               string    `json:"currency"`
	PrincipalMinor         int64     `json:"principal_minor"`
	OriginationFeeMinor    int64     `json:"origination_fee_minor"`
	NetDisbursementMinor   int64     `json:"net_disbursement_minor"`
	TenorMonths            int       `json:"tenor_months"`
	MonthlyRateBps         int       `json:"monthly_rate_bps"`
	NominalAnnualRateBps   int       `json:"nominal_annual_rate_bps"`
	EffectiveAnnualCostBps int       `json:"effective_annual_cost_bps"`
	InstalmentMinor        int64     `json:"instalment_minor"`
	TotalInterestMinor     int64     `json:"total_interest_minor"`
	TotalRepayableMinor    int64     `json:"total_repayable_minor"`
	ExpiresAt              time.Time `json:"expires_at"`
	// Disclosure is the exact document the hash covers. The client shows it
	// and returns DisclosureHash when accepting.
	Disclosure     json.RawMessage `json:"disclosure"`
	DisclosureHash string          `json:"disclosure_hash"`
}

func offerJSON(o domain.Offer) offerResponse {
	var d domain.Disclosure
	currency := ""
	if json.Unmarshal(o.Disclosure, &d) == nil {
		currency = d.Currency
	}
	return offerResponse{
		OfferID: o.ID.String(), Status: string(o.State), Currency: currency,
		PrincipalMinor: o.PrincipalMinor, OriginationFeeMinor: o.OriginationFeeMinor, NetDisbursementMinor: o.NetDisbursementMinor,
		TenorMonths: o.TenorMonths, MonthlyRateBps: o.MonthlyRateBps, NominalAnnualRateBps: o.NominalAnnualRateBps,
		EffectiveAnnualCostBps: o.EffectiveAnnualCostBps, InstalmentMinor: o.InstalmentMinor,
		TotalInterestMinor: o.TotalInterestMinor, TotalRepayableMinor: o.TotalRepayableMinor,
		ExpiresAt: o.ExpiresAt, Disclosure: o.Disclosure, DisclosureHash: o.DisclosureHash,
	}
}
