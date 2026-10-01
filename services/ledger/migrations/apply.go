package migrations

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"bankplatform.internal/platform/migrate"
)

// PartitionMonthsAhead is how many monthly partitions exist beyond the
// current month after Apply. `ledgerd migrate` runs on every deploy and from
// a scheduled job, so partitions always exist well before they are needed.
const PartitionMonthsAhead = 3

// Apply runs pending migrations as the schema owner and makes sure month
// partitions exist for the current month and the months ahead.
func Apply(ctx context.Context, ownerDSN string, now time.Time) error {
	if err := migrate.Up(ctx, ownerDSN, FS); err != nil {
		return err
	}
	conn, err := pgx.Connect(ctx, ownerDSN)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `SELECT ledger.ensure_month_partitions($1::date, $2)`,
		now.UTC().Format(time.DateOnly), PartitionMonthsAhead+1); err != nil {
		return fmt.Errorf("ensure ledger partitions: %w", err)
	}
	return nil
}

// ApplyDevSeed adds a funding account and a `devtools` caller that may credit
// customer accounts from it. It exists so local scenarios and tests can give
// a customer money before inbound transfers are built.
//
// It must never run in staging or production: callers check the environment
// before invoking it. Real money enters the ledger only through a payment
// rail with reconciliation.
func ApplyDevSeed(ctx context.Context, ownerDSN string) error {
	conn, err := pgx.Connect(ctx, ownerDSN)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	_, err = conn.Exec(ctx, `
		WITH a AS (
		  INSERT INTO ledger.accounts (code, class, normal_side, currency, gl_code, owner_type)
		  VALUES ('SYS:DEV_FUNDING', 'ASSET', 'DEBIT', 'NGN', '1999', 'SYSTEM')
		  ON CONFLICT (code) DO NOTHING
		  RETURNING account_id)
		INSERT INTO ledger.balances (account_id, floor) SELECT account_id, NULL FROM a;
		INSERT INTO ledger.posting_rights (caller, action) VALUES
		  ('devtools', 'DEV_FUNDING'), ('devtools', 'READ')
		ON CONFLICT DO NOTHING;
		INSERT INTO ledger.journal_types (journal_type, max_customer_accounts, system_accounts)
		VALUES ('DEV_FUNDING', 1, '{SYS:DEV_FUNDING}')
		ON CONFLICT DO NOTHING;`)
	return err
}
