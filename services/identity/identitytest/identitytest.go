// Package identitytest starts a real identity service for integration tests:
// migrated database, REST handler on an httptest server, and the internal
// gRPC server with mutual TLS, wired to a real test ledger.
package identitytest

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"

	identityv1 "bankplatform.internal/gen/bank/identity/v1"
	"bankplatform.internal/platform/authn"
	"bankplatform.internal/platform/grpcx"
	"bankplatform.internal/platform/pgtest"
	"bankplatform.internal/platform/pgxutil"
	"bankplatform.internal/platform/ratelimit"
	"bankplatform.internal/services/identity/app"
	"bankplatform.internal/services/identity/grpcapi"
	"bankplatform.internal/services/identity/httpapi"
	"bankplatform.internal/services/identity/ledgerclient"
	"bankplatform.internal/services/identity/migrations"
	"bankplatform.internal/services/identity/postgres"
	"bankplatform.internal/services/identity/providers/bvnsim"
	"bankplatform.internal/services/identity/secrets"
	"bankplatform.internal/services/ledger/ledgertest"
	sim "bankplatform.internal/simulator"
)

// Env is a running test identity service.
type Env struct {
	DB       pgtest.Database
	Pool     *pgxpool.Pool
	Store    *postgres.Store
	Service  *app.Service
	URL      string // REST base URL
	GRPCAddr string
	Verifier *authn.Verifier
	Ledger   *ledgertest.Env
	Clock    *Clock
	// KYC is the simulated identity provider this service calls over HTTP.
	KYC *sim.Server

	phoneSeq atomic.Int64
}

// Clock is a controllable time source shared by the service under test.
type Clock struct{ offset atomic.Int64 }

func (c *Clock) Now() time.Time          { return time.Now().Add(time.Duration(c.offset.Load())) }
func (c *Clock) Advance(d time.Duration) { c.offset.Add(int64(d)) }

// Options customise Start.
type Options struct {
	// Accounts overrides the ledger account opener (to simulate an outage).
	Accounts app.AccountOpener
	// TierOnBVNMatch defaults to 2.
	TierOnBVNMatch int
	// Guard and LoginLimit enable sign-in rate limiting; nil disables it.
	Guard      *ratelimit.Guard
	LoginLimit ratelimit.Limit
	// KYCTimeout bounds one call to the identity provider; defaults to 2s.
	KYCTimeout time.Duration
}

// LightHash keeps argon2 cheap in tests; production uses secrets.DefaultHashParams.
var LightHash = secrets.HashParams{Time: 1, Memory: 64, Threads: 1}

// Start runs identity against the given test ledger.
func Start(t testing.TB, ledger *ledgertest.Env, opts Options) *Env {
	return StartWithClock(t, ledger, opts, &Clock{})
}

// StartWithClock is Start with a clock shared with other services under test.
func StartWithClock(t testing.TB, ledger *ledgertest.Env, opts Options, clock *Clock) *Env {
	t.Helper()
	ctx := context.Background()
	db := pgtest.New(t, "identity_owner", "identity_app")
	if err := migrations.Apply(ctx, db.OwnerDSN); err != nil {
		t.Fatalf("migrate identity: %v", err)
	}
	pool, err := pgxutil.NewPool(ctx, db.AppDSN, pgxutil.PoolOptions{ApplicationName: "identity-test", MaxConns: 20, StatementTimeout: 10 * time.Second, LockTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := authn.NewSigner(priv, "identity", 15*time.Minute, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := authn.NewVerifier(pub, "identity", clock.Now)
	if err != nil {
		t.Fatal(err)
	}

	hmacKey, encKey := make([]byte, 32), make([]byte, 32)
	_, _ = rand.Read(hmacKey)
	_, _ = rand.Read(encKey)
	sealer, err := secrets.NewSealer(hmacKey, encKey)
	if err != nil {
		t.Fatal(err)
	}

	accounts := opts.Accounts
	if accounts == nil {
		accounts = ledgerclient.New(ledger.Client(t, "identity"), log)
	}
	tier := opts.TierOnBVNMatch
	if tier == 0 {
		tier = 2
	}
	kycTimeout := opts.KYCTimeout
	if kycTimeout == 0 {
		kycTimeout = 2 * time.Second
	}
	const kycAPIKey = "test-kyc-key"
	kyc := sim.New(sim.Options{APIKey: kycAPIKey, Now: clock.Now})
	kycSrv := httptest.NewServer(kyc.Handler())
	t.Cleanup(kycSrv.Close)

	store := postgres.NewStore(pool)
	svc, err := app.NewService(store, bvnsim.New(kycSrv.URL, kycAPIKey, kycTimeout), accounts, signer, sealer, app.Config{
		HashParams:             LightHash,
		Lockout:                postgres.LockoutPolicy{MaxFailures: 5, LockFor: 15 * time.Minute},
		InternalCallers:        []string{"lending", "payments"},
		BureauSubjectCallers:   []string{"lending"},
		RecipientLookupCallers: []string{"payments"},
		TierOnBVNMatch:         tier,
	}, clock.Now, log)
	if err != nil {
		t.Fatal(err)
	}

	httpSrv := httptest.NewServer(httpapi.New(svc, httpapi.Config{Logger: log, Guard: opts.Guard, LoginLimit: opts.LoginLimit}))
	t.Cleanup(httpSrv.Close)

	serverCert, err := ledger.CA.Issue(grpcx.WorkloadURI("identity"), "identity.test")
	if err != nil {
		t.Fatal(err)
	}
	grpcSrv, _, err := grpcx.NewServer(grpcx.ServerConfig{TLS: ledger.CA.ServerConfig(serverCert), Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	identityv1.RegisterIdentityServiceServer(grpcSrv, grpcapi.NewServer(svc, log))
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = grpcSrv.Serve(lis) }()
	t.Cleanup(grpcSrv.Stop)

	return &Env{DB: db, Pool: pool, Store: store, Service: svc, URL: httpSrv.URL, GRPCAddr: lis.Addr().String(),
		Verifier: verifier, Ledger: ledger, Clock: clock, KYC: kyc}
}

// Conn returns a gRPC connection to identity authenticated as caller.
func (e *Env) Conn(t testing.TB, caller string) *grpc.ClientConn {
	t.Helper()
	leaf, err := e.Ledger.CA.Issue(grpcx.WorkloadURI(caller))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := grpcx.Dial(grpcx.ClientConfig{Target: e.GRPCAddr, TLS: e.Ledger.CA.ClientConfig(leaf, "identity.test")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// Client returns an identity gRPC client authenticated as caller.
func (e *Env) Client(t testing.TB, caller string) identityv1.IdentityServiceClient {
	return identityv1.NewIdentityServiceClient(e.Conn(t, caller))
}

// Customer is a registered test customer.
type Customer struct {
	ID          uuid.UUID
	Phone       string
	PIN         string
	BVN         string
	FullName    string
	Token       string
	AccountCode string
}

// NextPhone returns a unique valid phone number.
func (e *Env) NextPhone() string { return fmt.Sprintf("+23480%08d", 10_000_000+e.phoneSeq.Add(1)) }

// NextBVN returns a unique BVN that the simulator will match. suffix, when
// four digits, selects a simulator behaviour.
func (e *Env) NextBVN(suffix string) string {
	if suffix == "" {
		suffix = "4242"
	}
	return fmt.Sprintf("%07d%s", 2_000_000+e.phoneSeq.Add(1), suffix)
}

// Register registers and logs in a new adult customer through the REST API.
func (e *Env) Register(t testing.TB) Customer {
	t.Helper()
	c := Customer{Phone: e.NextPhone(), PIN: "482915", BVN: e.NextBVN(""), FullName: "Ada Test Customer"}
	status, body := e.Post(t, "/v1/customers", map[string]string{
		"phone": c.Phone, "full_name": c.FullName, "date_of_birth": "1990-05-17", "bvn": c.BVN, "pin": c.PIN,
	})
	if status != http.StatusCreated {
		t.Fatalf("register: %d %s", status, body)
	}
	var reg struct {
		CustomerID string `json:"customer_id"`
	}
	if err := json.Unmarshal(body, &reg); err != nil {
		t.Fatal(err)
	}
	c.ID = uuid.MustParse(reg.CustomerID)
	c.AccountCode = "CUST:" + reg.CustomerID + ":MAIN"
	c.Token = e.Login(t, c.Phone, c.PIN)
	return c
}

// Login returns an access token for the customer.
func (e *Env) Login(t testing.TB, phone, pin string) string {
	t.Helper()
	status, body := e.Post(t, "/v1/sessions", map[string]string{"phone": phone, "pin": pin})
	if status != http.StatusOK {
		t.Fatalf("login: %d %s", status, body)
	}
	var s struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &s); err != nil {
		t.Fatal(err)
	}
	return s.AccessToken
}

// StaffToken creates a staff member with the given roles and logs them in.
func (e *Env) StaffToken(t testing.TB, roles ...string) (uuid.UUID, string) {
	t.Helper()
	email := fmt.Sprintf("staff%d@bank.test", e.phoneSeq.Add(1))
	const password = "correct-horse-battery"
	// Staff rows are created by the owner role (the bootstrap command's path).
	ownerPool, err := pgxpool.New(context.Background(), e.DB.OwnerDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer ownerPool.Close()
	id, err := app.CreateStaff(context.Background(), postgres.NewStore(ownerPool), LightHash, email, password, roles)
	if err != nil {
		t.Fatalf("create staff: %v", err)
	}
	status, body := e.Post(t, "/v1/staff/sessions", map[string]string{"email": email, "password": password})
	if status != http.StatusOK {
		t.Fatalf("staff login: %d %s", status, body)
	}
	var s struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.Unmarshal(body, &s)
	return id, s.AccessToken
}

// Post sends a JSON POST to the REST API and returns status and body.
func (e *Env) Post(t testing.TB, path string, payload any) (int, []byte) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(e.URL+path, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}
