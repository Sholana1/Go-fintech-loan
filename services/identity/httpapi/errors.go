package httpapi

import (
	"errors"
	"net/http"

	"bankplatform.internal/platform/httpx"
	"bankplatform.internal/services/identity/app"
	"bankplatform.internal/services/identity/domain"
)

// translate maps domain errors to API errors. Messages are deliberately
// uninformative where detail would help an attacker enumerate customers.
func (h *Handler) translate(r *http.Request, err error) error {
	switch {
	case errors.Is(err, domain.ErrInvalid):
		return httpx.Errorf(http.StatusBadRequest, "INVALID_REQUEST", "%s", err.Error())
	case errors.Is(err, domain.ErrAlreadyRegistered):
		return httpx.Errorf(http.StatusConflict, "REGISTRATION_CONFLICT", "registration cannot be completed with these details")
	case errors.Is(err, domain.ErrIdentityNotMatched):
		return httpx.Errorf(http.StatusUnprocessableEntity, "IDENTITY_NOT_VERIFIED", "we could not verify your identity with the details provided")
	case errors.Is(err, app.ErrProviderUnavailable):
		return httpx.Errorf(http.StatusServiceUnavailable, "IDENTITY_PROVIDER_UNAVAILABLE", "identity verification is temporarily unavailable; please try again")
	case errors.Is(err, domain.ErrBadCredentials), errors.Is(err, domain.ErrBlocked):
		return httpx.Errorf(http.StatusUnauthorized, "INVALID_CREDENTIALS", "the credentials are not valid")
	case errors.Is(err, domain.ErrLocked):
		return httpx.Errorf(http.StatusTooManyRequests, "CREDENTIAL_LOCKED", "too many failed attempts; try again later")
	default:
		h.log.ErrorContext(r.Context(), "identity request failed", "error", err.Error())
		return err
	}
}
