package identity_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	ledgerv1 "bankplatform.internal/gen/bank/ledger/v1"
	"bankplatform.internal/services/ledger/ledgertest"
)

func openViaClient(t *testing.T, ledger *ledgertest.Env, id uuid.UUID, code string) error {
	_, err := ledger.Client(t, "identity").OpenAccount(context.Background(), &ledgerv1.OpenAccountRequest{
		Code: code, Kind: ledgerv1.AccountKind_ACCOUNT_KIND_CUSTOMER_DEPOSIT, Currency: "NGN", OwnerId: id.String(),
	})
	return err
}
