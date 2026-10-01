package httpapi

import (
	"encoding/json"
	"net/http"
	"time"

	"bankplatform.internal/platform/httpx"
	"bankplatform.internal/services/lending/app"
	"bankplatform.internal/services/lending/domain"
)

type decisionJSON struct {
	DecisionID             string          `json:"decision_id"`
	Outcome                string          `json:"outcome"`
	ReasonCodes            []string        `json:"reason_codes"`
	ApprovedPrincipalMinor int64           `json:"approved_principal_minor"`
	RiskBand               string          `json:"risk_band"`
	PolicyVersion          string          `json:"policy_version"`
	DecidedBy              string          `json:"decided_by"`
	Features               domain.Features `json:"features"`
	CreatedAt              time.Time       `json:"created_at"`
}

type reviewItemJSON struct {
	ApplicationID        string         `json:"application_id"`
	CustomerID           string         `json:"customer_id"`
	RequestedAmountMinor int64          `json:"requested_amount_minor"`
	TenorMonths          int            `json:"tenor_months"`
	SubmittedAt          time.Time      `json:"submitted_at"`
	ReasonCodes          []string       `json:"reason_codes"`
	Decisions            []decisionJSON `json:"decisions"`
}

func reviewJSON(it app.ReviewItem) reviewItemJSON {
	a := it.Application
	out := reviewItemJSON{
		ApplicationID: a.ID.String(), CustomerID: a.CustomerID.String(), RequestedAmountMinor: a.RequestedPrincipalMinor,
		TenorMonths: a.TenorMonths, SubmittedAt: a.SubmittedAt, ReasonCodes: a.StateReason,
	}
	for _, d := range it.Decisions {
		out.Decisions = append(out.Decisions, decisionJSON{
			DecisionID: d.ID.String(), Outcome: string(d.Decision.Outcome), ReasonCodes: d.Decision.ReasonCodes,
			ApprovedPrincipalMinor: d.Decision.ApprovedPrincipalMinor, RiskBand: d.Decision.RiskBand,
			PolicyVersion: d.PolicyVersion, DecidedBy: d.DecidedBy, Features: d.Features, CreatedAt: d.CreatedAt,
		})
	}
	return out
}

type adminAction struct {
	ActionID   string          `json:"action_id"`
	Kind       string          `json:"kind"`
	LoanID     string          `json:"loan_id"`
	Params     json.RawMessage `json:"params"`
	Reason     string          `json:"reason"`
	State      string          `json:"state"`
	MakerID    string          `json:"maker_id"`
	CheckerID  string          `json:"checker_id,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	DecidedAt  *time.Time      `json:"decided_at,omitempty"`
	ExecutedAt *time.Time      `json:"executed_at,omitempty"`
}

func adminActionJSON(a domain.AdminAction) adminAction {
	out := adminAction{
		ActionID: a.ID.String(), Kind: string(a.Kind), LoanID: a.LoanID.String(), Params: a.Params, Reason: a.Reason,
		State: string(a.State), MakerID: a.MakerID.String(), CreatedAt: a.CreatedAt, DecidedAt: a.DecidedAt, ExecutedAt: a.ExecutedAt,
	}
	if a.CheckerID != nil {
		out.CheckerID = a.CheckerID.String()
	}
	return out
}

type exceptionJSON struct {
	ExceptionID    string          `json:"exception_id"`
	Kind           string          `json:"kind"`
	EntityType     string          `json:"entity_type"`
	EntityID       string          `json:"entity_id"`
	AmountMinor    int64           `json:"amount_minor"`
	Detail         json.RawMessage `json:"detail"`
	State          string          `json:"state"`
	OpenedAt       time.Time       `json:"opened_at"`
	ResolvedAt     *time.Time      `json:"resolved_at,omitempty"`
	ResolvedBy     string          `json:"resolved_by,omitempty"`
	ResolutionNote string          `json:"resolution_note,omitempty"`
}

func exceptionToJSON(e domain.ReconException) exceptionJSON {
	return exceptionJSON{
		ExceptionID: e.ID.String(), Kind: e.Kind, EntityType: e.EntityType, EntityID: e.EntityID, AmountMinor: e.AmountMinor,
		Detail: e.Detail, State: e.State, OpenedAt: e.OpenedAt, ResolvedAt: e.ResolvedAt, ResolvedBy: e.ResolvedBy, ResolutionNote: e.ResolutionNote,
	}
}

func marshalParams(p map[string]any) (json.RawMessage, error) {
	if p == nil {
		return json.RawMessage(`{}`), nil
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, httpx.Errorf(http.StatusBadRequest, "VALIDATION_FAILED", "params are not valid JSON")
	}
	return raw, nil
}
