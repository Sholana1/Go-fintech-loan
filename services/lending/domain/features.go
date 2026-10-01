package domain

// Outcome of a credit decision.
type Outcome string

const (
	OutcomeApprove Outcome = "APPROVE"
	OutcomeRefer   Outcome = "REFER"
	OutcomeDecline Outcome = "DECLINE"
)

// Reason codes are stable identifiers. They are stored with the decision,
// shown to reviewers, and mapped to customer-facing wording by the client.
const (
	ReasonKYCInsufficient       = "KYC_INSUFFICIENT"
	ReasonCustomerBlocked       = "CUSTOMER_NOT_ACTIVE"
	ReasonUnderage              = "BELOW_MINIMUM_AGE"
	ReasonFraudRisk             = "FRAUD_RISK"
	ReasonFraudReview           = "FRAUD_REVIEW"
	ReasonExistingLoan          = "EXISTING_ACTIVE_LOAN"
	ReasonPriorWriteOff         = "PRIOR_WRITE_OFF"
	ReasonBureauScoreLow        = "BUREAU_SCORE_LOW"
	ReasonBureauDelinquency     = "BUREAU_DELINQUENCY"
	ReasonThinFile              = "THIN_CREDIT_FILE"
	ReasonAffordability         = "AFFORDABILITY_INSUFFICIENT"
	ReasonAmountReduced         = "AMOUNT_REDUCED_AFFORDABILITY"
	ReasonAmountReducedExposure = "AMOUNT_REDUCED_EXPOSURE"
	ReasonPricingUnavailable    = "PRICING_UNAVAILABLE"
	ReasonExposureLimit         = "EXPOSURE_LIMIT"
	ReasonAboveAutoApprove      = "ABOVE_AUTO_APPROVAL_LIMIT"
	ReasonManualApproval        = "MANUAL_REVIEW_APPROVED"
	ReasonManualDecline         = "MANUAL_REVIEW_DECLINED"
	ReasonBureauUnavailable     = "CREDIT_BUREAU_UNAVAILABLE"
	ReasonApplicationExpired    = "APPLICATION_EXPIRED"
	ReasonOfferExpired          = "OFFER_EXPIRED"
	ReasonDisbursementRejected  = "DISBURSEMENT_REJECTED"
)

// FraudDecision is the fraud screen's verdict.
type FraudDecision string

const (
	FraudClear  FraudDecision = "CLEAR"
	FraudReview FraudDecision = "REVIEW"
	FraudBlock  FraudDecision = "BLOCK"
)

// Features are the complete inputs to a credit decision. The decision
// snapshot stores exactly this structure, so Evaluate can be replayed on it
// later and must return the same Decision (see the reproducibility test).
//
// Only data the decision needs is here. In particular there is no device
// content, contact list, location history or message data: the product does
// not collect them.
type Features struct {
	KYCVerified    bool `json:"kyc_verified"`
	KYCTier        int  `json:"kyc_tier"`
	CustomerActive bool `json:"customer_active"`
	AgeYears       int  `json:"age_years"`

	FraudDecision FraudDecision `json:"fraud_decision"`
	FraudSignals  []string      `json:"fraud_signals"`

	// BureauScore is nil for a thin file (the bureau has no score).
	BureauScore                   *int  `json:"bureau_score"`
	BureauWorstDelinquencyDays12m int   `json:"bureau_worst_delinquency_days_12m"`
	BureauMonthlyObligationsMinor int64 `json:"bureau_monthly_obligations_minor"`

	StatedMonthlyIncomeMinor int64 `json:"stated_monthly_income_minor"`

	InternalOutstandingPrincipalMinor int64 `json:"internal_outstanding_principal_minor"`
	HasActiveLoan                     bool  `json:"has_active_loan"`
	HasPriorWriteOff                  bool  `json:"has_prior_write_off"`

	RequestedPrincipalMinor int64 `json:"requested_principal_minor"`
	TenorMonths             int   `json:"tenor_months"`
}

// Decision is the policy's output.
type Decision struct {
	Outcome                Outcome  `json:"outcome"`
	ReasonCodes            []string `json:"reason_codes"`
	ApprovedPrincipalMinor int64    `json:"approved_principal_minor"`
	RiskBand               string   `json:"risk_band"`
	MonthlyRateBps         int      `json:"monthly_rate_bps"`
}
