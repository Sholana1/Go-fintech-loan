// Package ledgertest starts a real ledger for integration tests: a freshly
// migrated PostgreSQL database and the gRPC server listening on localhost
// with mutual TLS. Other services' tests use it so that they exercise the
// real posting transactions and the real authorisation path, not a fake.
package ledgertest

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"

	commonv1 "bankplatform.internal/gen/bank/common/v1"
	ledgerv1 "bankplatform.internal/gen/bank/ledger/v1"
	"bankplatform.internal/platform/devcert"
	"bankplatform.internal/platform/grpcx"
	"bankplatform.internal/platform/pgtest"
	"bankplatform.internal/platform/pgxutil"
	"bankplatform.internal/services/ledger/app"
	"bankplatform.internal/services/ledger/grpcapi"
	"bankplatform.internal/services/ledger/migrations"
	"bankplatform.internal/services/ledger/postgres"
)

// Env is a running test ledger.
type Env struct {
	DB    pgtest.Database
	Pool  *pgxpool.Pool // application-role pool
	Store *postgres.Store
	Addr  string
	CA    *devcert.Authority
}

// NopMetrics discards ledger metrics.
type NopMetrics struct{}

func (NopMetrics) JournalPosted(string, bool, time.Duration) {}
func (NopMetrics) PostingRejected(string, string)            {}

// Start creates the database, applies migrations and the development seed,
// and serves the ledger on a random localhost port.
func Start(t testing.TB) *Env { return StartWithClock(t, time.Now) }

// StartWithClock is Start with a controllable clock, for tests that move
// time forward (the ledger refuses journals dated in its future).
func StartWithClock(t testing.TB, now func() time.Time) *Env {
	t.Helper()
	ctx := context.Background()

	db := pgtest.New(t, "ledger_owner", "ledger_app")
	if err := migrations.Apply(ctx, db.OwnerDSN, time.Now()); err != nil {
		t.Fatalf("migrate ledger: %v", err)
	}
	if err := migrations.ApplyDevSeed(ctx, db.OwnerDSN); err != nil {
		t.Fatalf("seed ledger: %v", err)
	}

	pool, err := pgxutil.NewPool(ctx, db.AppDSN, pgxutil.PoolOptions{
		ApplicationName: "ledger-test", MaxConns: 30,
		StatementTimeout: 10 * time.Second, LockTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("connect ledger pool: %v", err)
	}
	t.Cleanup(pool.Close)

	store := postgres.NewStore(pool, now, nil)
	rights, err := store.LoadRights(ctx)
	if err != nil {
		t.Fatalf("load rights: %v", err)
	}
	policies, err := store.LoadJournalPolicies(ctx)
	if err != nil {
		t.Fatalf("load journal policies: %v", err)
	}
	svc := app.NewService(store, rights, policies, NopMetrics{})

	ca, err := devcert.NewAuthority()
	if err != nil {
		t.Fatal(err)
	}
	serverCert, err := ca.Issue(grpcx.WorkloadURI("ledger"), "ledger.test")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, _, err := grpcx.NewServer(grpcx.ServerConfig{TLS: ca.ServerConfig(serverCert), Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	ledgerv1.RegisterLedgerServiceServer(srv, grpcapi.NewServer(svc, log))

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	return &Env{DB: db, Pool: pool, Store: store, Addr: lis.Addr().String(), CA: ca}
}

// Conn returns a client connection authenticated as the given workload.
func (e *Env) Conn(t testing.TB, caller string) *grpc.ClientConn {
	t.Helper()
	leaf, err := e.CA.Issue(grpcx.WorkloadURI(caller))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := grpcx.Dial(grpcx.ClientConfig{Target: e.Addr, TLS: e.CA.ClientConfig(leaf, "ledger.test")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// Client returns a ledger client authenticated as the given workload.
func (e *Env) Client(t testing.TB, caller string) ledgerv1.LedgerServiceClient {
	return ledgerv1.NewLedgerServiceClient(e.Conn(t, caller))
}

// OpenCustomer opens a deposit account for a new customer and returns its code.
func (e *Env) OpenCustomer(t testing.TB) (customerID uuid.UUID, code string) {
	t.Helper()
	customerID = uuid.New()
	code = "CUST:" + customerID.String() + ":MAIN"
	_, err := e.Client(t, "identity").OpenAccount(context.Background(), &ledgerv1.OpenAccountRequest{
		Code: code, Kind: ledgerv1.AccountKind_ACCOUNT_KIND_CUSTOMER_DEPOSIT, Currency: "NGN", OwnerId: customerID.String(),
	})
	if err != nil {
		t.Fatalf("open account: %v", err)
	}
	return customerID, code
}

// Fund credits a customer account from the development funding account.
func (e *Env) Fund(t testing.TB, code string, minor int64) {
	t.Helper()
	_, err := e.Client(t, "devtools").PostJournal(context.Background(), &ledgerv1.PostJournalRequest{
		Ref:         &ledgerv1.PostingRef{OpType: "DEV_FUNDING", OpId: uuid.NewString(), OpStep: "POST"},
		JournalType: "DEV_FUNDING",
		Lines: []*ledgerv1.Line{
			{AccountCode: "SYS:DEV_FUNDING", Direction: ledgerv1.Direction_DIRECTION_DEBIT, Amount: &commonv1.Money{MinorUnits: minor, Currency: "NGN"}},
			{AccountCode: code, Direction: ledgerv1.Direction_DIRECTION_CREDIT, Amount: &commonv1.Money{MinorUnits: minor, Currency: "NGN"}},
		},
	})
	if err != nil {
		t.Fatalf("fund %s: %v", code, err)
	}
}

// Balance returns (posted, held) of an account, read directly from the store.
func (e *Env) Balance(t testing.TB, code string) (posted, held int64) {
	t.Helper()
	b, err := e.Store.GetBalance(context.Background(), code)
	if err != nil {
		t.Fatalf("balance %s: %v", code, err)
	}
	return b.Posted, b.Held
}

// AssertInvariants fails the test if any ledger invariant is violated.
func (e *Env) AssertInvariants(t testing.TB) {
	t.Helper()
	v, err := e.Store.Verify(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("verify ledger: %v", err)
	}
	if v.Total() != 0 {
		t.Fatalf("ledger invariants violated: %+v", v)
	}
}
