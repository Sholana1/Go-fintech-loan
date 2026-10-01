package httpapi

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	"bankplatform.internal/platform/httpx"
	"bankplatform.internal/services/lending/domain"
)

// fail writes an error response, translating domain errors to API errors.
func (h *handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var apiErr *httpx.APIError
	if errors.As(err, &apiErr) {
		httpx.WriteError(w, r, err)
		return
	}
	translated := translate(err)
	if translated.Status >= 500 && translated.Code == "INTERNAL" {
		h.log.ErrorContext(r.Context(), "lending request failed", "error", err.Error(), "request_id", httpx.RequestID(r.Context()))
	}
	if translated.Status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "5")
	}
	if translated.Code == "OPERATION_IN_PROGRESS" {
		w.Header().Set("Retry-After", "1")
	}
	httpx.WriteError(w, r, translated)
}

// translate maps domain errors to the stable API error contract.
func translate(err error) *httpx.APIError {
	type mapping struct {
		target  error
		status  int
		code    string
		message string
	}
	for _, m := range []mapping{
		{domain.ErrValidation, http.StatusBadRequest, "VALIDATION_FAILED", ""},
		{domain.ErrInvalidTerms, http.StatusBadRequest, "VALIDATION_FAILED", ""},
		{domain.ErrNotFound, http.StatusNotFound, "NOT_FOUND", "the resource does not exist"},
		{domain.ErrApplicationOpen, http.StatusConflict, "APPLICATION_ALREADY_OPEN", "you already have an application in progress for this product"},
		{domain.ErrIdempotencyMismatch, http.StatusUnprocessableEntity, "IDEMPOTENCY_KEY_REUSED", "this Idempotency-Key was already used with a different request"},
		{domain.ErrOfferExpired, http.StatusGone, "OFFER_EXPIRED", "this offer has expired"},
		{domain.ErrOfferNotOpen, http.StatusConflict, "OFFER_NOT_OPEN", "this offer can no longer be accepted"},
		{domain.ErrDisclosureMismatch, http.StatusUnprocessableEntity, "DISCLOSURE_MISMATCH", "the disclosure hash does not match the terms of this offer"},
		{domain.ErrStepUpLocked, http.StatusTooManyRequests, "CREDENTIAL_LOCKED", "too many failed attempts; try again later"},
		{domain.ErrStepUpFailed, http.StatusForbidden, "STEP_UP_FAILED", "the PIN is not correct"},
		{domain.ErrDestinationRejected, http.StatusUnprocessableEntity, "DESTINATION_NOT_VERIFIED", "the destination account could not be verified as your own"},
		{domain.ErrOperationInProgress, http.StatusConflict, "OPERATION_IN_PROGRESS", "another operation on this loan is in progress; retry shortly"},
		{domain.ErrLoanNotRepayable, http.StatusConflict, "LOAN_NOT_REPAYABLE", ""},
		{domain.ErrForbidden, http.StatusForbidden, "FORBIDDEN", ""},
		{domain.ErrConflict, http.StatusConflict, "CONFLICT", ""},
		{domain.ErrDependencyUnavailable, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE", "a required service is temporarily unavailable; retry with the same Idempotency-Key"},
	} {
		if errors.Is(err, m.target) {
			msg := m.message
			if msg == "" {
				msg = err.Error()
			}
			return &httpx.APIError{Status: m.status, Code: m.code, Message: msg}
		}
	}
	return &httpx.APIError{Status: http.StatusInternalServerError, Code: "INTERNAL", Message: "internal error"}
}

func pathID(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		// A malformed id cannot name anything: answer as for a missing resource.
		return uuid.Nil, httpx.Errorf(http.StatusNotFound, "NOT_FOUND", "the resource does not exist")
	}
	return id, nil
}

func idempotencyKey(r *http.Request) string { return r.Header.Get("Idempotency-Key") }
