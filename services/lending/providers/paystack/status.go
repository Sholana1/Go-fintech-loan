package paystack

import (
	"fmt"

	"bankplatform.internal/services/lending/domain"
)

// transferOutcome maps a Paystack transfer status to an outcome.
//
// The specification's enum is: pending, success, failed, otp, abandoned,
// reversed, blocked, rejected, received. It names them without defining
// them, so only the three whose meaning is unambiguous are treated as final:
//
//	success   the money was sent
//	failed    the transfer did not happen
//	reversed  the money came back to our balance
//
// Every other listed status is reported as PENDING, carrying the status as
// its code so operations can see it. That keeps the customer's funds held
// until the provider says something final, or a person decides. Treating
// "blocked" or "rejected" as failed would release funds on a guess.
//
// A status not in the enum is an error: this code does not know it.
func transferOutcome(status string) (domain.ProviderOutcome, string, error) {
	switch status {
	case "success":
		return domain.ProviderSuccess, "success", nil
	case "failed", "reversed":
		return domain.ProviderFailed, status, nil
	case "pending", "otp", "abandoned", "blocked", "rejected", "received":
		return domain.ProviderPending, "paystack:" + status, nil
	default:
		return "", "", fmt.Errorf("paystack: unrecognised transfer status %q", status)
	}
}

// paymentOutcome maps the status of a payment a customer made to us
// (GET /transaction/verify). "abandoned" means the customer did not complete
// the payment, which for us is "not paid yet": the reference may still be
// paid until it expires.
func paymentOutcome(status string) (domain.ProviderOutcome, string, error) {
	switch status {
	case "success":
		return domain.ProviderSuccess, "success", nil
	case "failed", "reversed":
		return domain.ProviderFailed, status, nil
	case "abandoned", "ongoing", "pending", "processing", "queued":
		return domain.ProviderPending, "paystack:" + status, nil
	default:
		return "", "", fmt.Errorf("paystack: unrecognised transaction status %q", status)
	}
}
