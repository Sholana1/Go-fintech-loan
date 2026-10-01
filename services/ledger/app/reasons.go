package app

import (
	"errors"

	"bankplatform.internal/services/ledger/contract"
	"bankplatform.internal/services/ledger/domain"
)

var reasons = []struct {
	err    error
	reason string
}{
	{domain.ErrUnbalanced, contract.ReasonUnbalanced},
	{domain.ErrInvalid, contract.ReasonInvalid},
	{domain.ErrNotAuthorised, contract.ReasonNotAuthorised},
	{domain.ErrAccountNotFound, contract.ReasonAccountNotFound},
	{domain.ErrAccountNotActive, contract.ReasonAccountNotActive},
	{domain.ErrCurrencyMismatch, contract.ReasonCurrencyMismatch},
	{domain.ErrInsufficientFunds, contract.ReasonInsufficientFunds},
	{domain.ErrRefReused, contract.ReasonRefReused},
	{domain.ErrHoldNotFound, contract.ReasonHoldNotFound},
	{domain.ErrHoldNotActive, contract.ReasonHoldNotActive},
	{domain.ErrHoldCaptured, contract.ReasonHoldCaptured},
	{domain.ErrJournalNotFound, contract.ReasonJournalNotFound},
	{domain.ErrAccountConflict, contract.ReasonAccountConflict},
}

// Reason maps a domain error to its stable contract reason.
func Reason(err error) string {
	for _, r := range reasons {
		if errors.Is(err, r.err) {
			return r.reason
		}
	}
	return contract.ReasonInternal
}
