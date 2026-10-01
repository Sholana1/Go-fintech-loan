package paystack_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"bankplatform.internal/services/lending/app"
	"bankplatform.internal/services/lending/domain"
)

var ctx = context.Background()

var instruction = app.PayoutInstruction{
	Reference: "lp-0b6f6f0e-58a4-4d0b-9d0f-0c5a2f0f7a11", AmountMinor: 9_900_000,
	BankCode: "058", AccountNumber: "0123456789", AccountName: "ADA OBI", Narration: "Loan disbursement",
}

func transferObject(status string) map[string]any {
	return map[string]any{"reference": instruction.Reference, "status": status, "transfer_code": "TRF_abc123", "amount": instruction.AmountMinor, "currency": "NGN"}
}

func recipientCreated() http.HandlerFunc {
	return reply(http.StatusCreated, true, "Transfer recipient created successfully", map[string]any{"recipient_code": "RCP_x1"})
}

func TestSendCreatesARecipientThenTransfersWithOurReference(t *testing.T) {
	f := newFake(t)
	f.on("POST /transferrecipient", recipientCreated())
	f.on("POST /transfer", reply(http.StatusOK, true, "Transfer has been queued", transferObject("success")))

	res, err := f.client().Send(ctx, instruction)
	if err != nil || res.Outcome != domain.ProviderSuccess || res.ProviderRef != "TRF_abc123" {
		t.Fatalf("send: %+v %v", res, err)
	}

	calls := f.recorded()
	if len(calls) != 2 {
		t.Fatalf("%d calls, want 2", len(calls))
	}
	for _, c := range calls {
		if c.Auth != "Bearer "+testKey {
			t.Fatalf("%s %s: Authorization %q", c.Method, c.Path, c.Auth)
		}
	}
	rcp := calls[0].Body
	if rcp["type"] != "nuban" || rcp["name"] != "ADA OBI" || rcp["account_number"] != "0123456789" || rcp["bank_code"] != "058" || rcp["currency"] != "NGN" {
		t.Fatalf("recipient request: %v", rcp)
	}
	trf := calls[1].Body
	if trf["source"] != "balance" || trf["recipient"] != "RCP_x1" || trf["reference"] != instruction.Reference ||
		trf["amount"] != float64(instruction.AmountMinor) || trf["currency"] != "NGN" || trf["reason"] != "Loan disbursement" {
		t.Fatalf("transfer request: %v", trf)
	}
}

func TestTransferStatusesMapToOutcomes(t *testing.T) {
	want := map[string]domain.ProviderOutcome{
		"success": domain.ProviderSuccess, "failed": domain.ProviderFailed, "reversed": domain.ProviderFailed,
		// Listed by the specification without a definition: never guessed
		// into a final outcome.
		"pending": domain.ProviderPending, "otp": domain.ProviderPending, "abandoned": domain.ProviderPending,
		"blocked": domain.ProviderPending, "rejected": domain.ProviderPending, "received": domain.ProviderPending,
	}
	for status, outcome := range want {
		f := newFake(t)
		f.on("POST /transferrecipient", recipientCreated())
		f.on("POST /transfer", reply(http.StatusOK, true, "", transferObject(status)))
		res, err := f.client().Send(ctx, instruction)
		if err != nil || res.Outcome != outcome {
			t.Errorf("status %q: outcome %q err %v, want %q", status, res.Outcome, err, outcome)
		}
	}

	// A status that is not in the specification is not interpreted at all.
	f := newFake(t)
	f.on("POST /transferrecipient", recipientCreated())
	f.on("POST /transfer", reply(http.StatusOK, true, "", transferObject("teleported")))
	if res, err := f.client().Send(ctx, instruction); err == nil {
		t.Fatalf("an unknown status produced an outcome: %+v", res)
	}
}

// If the recipient cannot be registered, no transfer was attempted: that is
// a certain failure, not an unknown.
func TestRecipientFailureIsACleanFailureAndNoTransferIsAttempted(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"declined":  reply(http.StatusBadRequest, false, "Account number is invalid", nil),
		"error":     reply(http.StatusInternalServerError, false, "An error occurred", nil),
		"malformed": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"status": tr`)) },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			f.on("POST /transferrecipient", h)
			res, err := f.client().Send(ctx, instruction)
			if err != nil || res.Outcome != domain.ProviderFailed || res.Code != "RECIPIENT_NOT_CREATED" {
				t.Fatalf("send: %+v %v", res, err)
			}
			if calls := f.recorded(); len(calls) != 1 {
				t.Fatalf("%d calls: a transfer was attempted without a recipient", len(calls))
			}
		})
	}
}

// Once POST /transfer has been attempted, anything short of a well-formed
// answer about our transfer is an unknown outcome (an error), never FAILED.
func TestAnUnclearTransferResponseIsUnknownNeverFailed(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"http 500":        reply(http.StatusInternalServerError, false, "An error occurred", nil),
		"http 400":        reply(http.StatusBadRequest, false, "Your balance is not enough to fulfil this request", nil),
		"http 401":        reply(http.StatusUnauthorized, false, "Invalid key", nil),
		"status false":    reply(http.StatusOK, false, "something", nil),
		"not json":        func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`<html>bad gateway</html>`)) },
		"truncated":       func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"status":true,"data":{"refer`)) },
		"other reference": reply(http.StatusOK, true, "", map[string]any{"reference": "lp-someone-else", "status": "success", "amount": instruction.AmountMinor, "currency": "NGN"}),
		"other amount":    reply(http.StatusOK, true, "", map[string]any{"reference": instruction.Reference, "status": "success", "amount": 1, "currency": "NGN"}),
		"other currency":  reply(http.StatusOK, true, "", map[string]any{"reference": instruction.Reference, "status": "success", "amount": instruction.AmountMinor, "currency": "GHS"}),
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			f.on("POST /transferrecipient", recipientCreated())
			f.on("POST /transfer", h)
			res, err := f.client().Send(ctx, instruction)
			if err == nil {
				t.Fatalf("got outcome %+v, want an error (unknown)", res)
			}
			if strings.Contains(err.Error(), testKey) {
				t.Fatalf("the secret key leaked into an error: %v", err)
			}
		})
	}
}

func TestATransferTimeoutIsUnknown(t *testing.T) {
	f := newFake(t)
	f.on("POST /transferrecipient", recipientCreated())
	f.on("POST /transfer", func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })

	deadline, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	res, err := f.client().Send(deadline, instruction)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("send: %+v %v, want a deadline error", res, err)
	}
}

func TestQuery(t *testing.T) {
	path := "GET /transfer/verify/" + instruction.Reference

	t.Run("final and pending statuses", func(t *testing.T) {
		for status, outcome := range map[string]domain.ProviderOutcome{"success": domain.ProviderSuccess, "failed": domain.ProviderFailed, "pending": domain.ProviderPending} {
			f := newFake(t)
			f.on(path, reply(http.StatusOK, true, "Transfer retrieved", transferObject(status)))
			res, err := f.client().Query(ctx, instruction.Reference)
			if err != nil || res.Outcome != outcome {
				t.Errorf("status %q: %+v %v", status, res, err)
			}
		}
	})

	t.Run("404 is NOT_FOUND, which is not a failure", func(t *testing.T) {
		f := newFake(t)
		f.on(path, reply(http.StatusNotFound, false, "Transfer not found", nil))
		res, err := f.client().Query(ctx, instruction.Reference)
		if err != nil || res.Outcome != domain.ProviderNotFound {
			t.Fatalf("query: %+v %v", res, err)
		}
	})

	t.Run("errors and wrong references are unknown", func(t *testing.T) {
		for name, h := range map[string]http.HandlerFunc{
			"http 500":        reply(http.StatusInternalServerError, false, "", nil),
			"other reference": reply(http.StatusOK, true, "", map[string]any{"reference": "lp-other", "status": "success"}),
		} {
			f := newFake(t)
			f.on(path, h)
			if res, err := f.client().Query(ctx, instruction.Reference); err == nil {
				t.Errorf("%s: got outcome %+v, want an error", name, res)
			}
		}
	})
}
