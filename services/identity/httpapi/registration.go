package httpapi

import (
	"net/http"
	"time"

	"bankplatform.internal/platform/httpx"
	"bankplatform.internal/services/identity/domain"
)

type registerRequest struct {
	Phone       string `json:"phone"`
	FullName    string `json:"full_name"`
	DateOfBirth string `json:"date_of_birth"`
	BVN         string `json:"bvn"`
	PIN         string `json:"pin"`
}

type customerResponse struct {
	CustomerID     string `json:"customer_id"`
	KYCStatus      string `json:"kyc_status"`
	KYCTier        int    `json:"kyc_tier"`
	AccountPending bool   `json:"account_pending"`
}

func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	dob, err := time.Parse(time.DateOnly, req.DateOfBirth)
	if err != nil {
		httpx.WriteError(w, r, httpx.Errorf(http.StatusBadRequest, "INVALID_REQUEST", "date_of_birth must be YYYY-MM-DD"))
		return
	}
	c, err := h.svc.Register(r.Context(), domain.Registration{Phone: req.Phone, FullName: req.FullName, DateOfBirth: dob, BVN: req.BVN, PIN: req.PIN})
	if err != nil {
		httpx.WriteError(w, r, h.translate(r, err))
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, customerResponse{
		CustomerID: c.ID.String(), KYCStatus: string(c.KYCStatus), KYCTier: c.KYCTier, AccountPending: c.DepositAccountCode == "",
	})
}
