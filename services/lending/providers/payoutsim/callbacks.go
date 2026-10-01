package payoutsim

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"bankplatform.internal/services/lending/app"
	"bankplatform.internal/services/lending/domain"
	"bankplatform.internal/services/lending/providers/simsig"
)

// Callbacks authenticates and decodes payout callbacks. It implements the
// payout-callback verifier the HTTP layer expects.
type Callbacks struct {
	Secret string
}

// VerifyPayoutCallback authenticates a callback and decodes it.
func (v Callbacks) VerifyPayoutCallback(h http.Header, body []byte, now time.Time) (app.PayoutCallback, error) {
	if err := simsig.Authenticate(v.Secret, h, body, now); err != nil {
		return app.PayoutCallback{}, err
	}

	var cb struct {
		EventID     string `json:"event_id"`
		Reference   string `json:"reference"`
		Status      string `json:"status"`
		ProviderRef string `json:"provider_ref"`
		Code        string `json:"code"`
	}
	if err := json.Unmarshal(body, &cb); err != nil || cb.EventID == "" || cb.Reference == "" {
		return app.PayoutCallback{}, fmt.Errorf("%w: callback body is malformed", domain.ErrValidation)
	}
	o, err := outcome(cb.Status)
	if err != nil {
		return app.PayoutCallback{}, fmt.Errorf("%w: %v", domain.ErrValidation, err)
	}
	return app.PayoutCallback{
		Provider: ProviderName, EventID: cb.EventID, Reference: cb.Reference, Outcome: o,
		ProviderRef: cb.ProviderRef, Code: cb.Code, Raw: body,
	}, nil
}
