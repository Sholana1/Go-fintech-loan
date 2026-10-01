// Package ledgerclient adapts the ledger's gRPC API to the one thing identity
// needs from it: opening a customer's deposit account.
//
// Why synchronous gRPC: registration should hand back a usable account, and
// the call is a single idempotent hop with a short deadline. If it fails,
// identity completes it later from its own table; no message broker is
// needed for one retryable call.
package ledgerclient

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	ledgerv1 "bankplatform.internal/gen/bank/ledger/v1"
	"bankplatform.internal/platform/grpcx"
	"bankplatform.internal/services/ledger/contract"
)

type Client struct {
	ledger ledgerv1.LedgerServiceClient
	log    *slog.Logger
}

func New(ledger ledgerv1.LedgerServiceClient, log *slog.Logger) *Client {
	return &Client{ledger: ledger, log: log}
}

// OpenCustomerDeposit opens (or finds) the customer's NGN deposit account.
// OpenAccount is idempotent on the account code, so it is retried on
// transient failures.
func (c *Client) OpenCustomerDeposit(ctx context.Context, customerID uuid.UUID) (string, error) {
	code := contract.CustomerDepositCode(customerID.String())
	err := grpcx.RetryIdempotent(ctx, grpcx.RetryPolicy{
		Attempts: 3, BaseBackoff: 50 * time.Millisecond, PerAttempt: 2 * time.Second,
		OnRetry: func(attempt int, err error) {
			c.log.WarnContext(ctx, "retrying ledger OpenAccount", "attempt", attempt, "error", err.Error())
		},
	}, func(ctx context.Context) error {
		_, err := c.ledger.OpenAccount(ctx, &ledgerv1.OpenAccountRequest{
			Code: code, Kind: ledgerv1.AccountKind_ACCOUNT_KIND_CUSTOMER_DEPOSIT, Currency: "NGN", OwnerId: customerID.String(),
		})
		return err
	})
	if err != nil {
		return "", err
	}
	return code, nil
}
