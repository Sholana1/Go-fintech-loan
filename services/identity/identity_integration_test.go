package identity_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	identityv1 "bankplatform.internal/gen/bank/identity/v1"
	"bankplatform.internal/platform/authn"
	"bankplatform.internal/services/identity/identitytest"
	"bankplatform.internal/services/ledger/ledgertest"
)

func errorCode(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("not an error envelope: %s", body)
	}
	return e.Error.Code
}

func TestRegistrationCreatesVerifiedCustomerWithLedgerAccount(t *testing.T) {
	ledger := ledgertest.Start(t)
	env := identitytest.Start(t, ledger, identitytest.Options{})
	c := env.Register(t)

	p, err := env.Verifier.Verify(c.Token)
	if err != nil || p.Subject != c.ID.String() || p.Kind != authn.KindCustomer {
		t.Fatalf("token principal %+v err %v", p, err)
	}
	if posted, held := ledger.Balance(t, c.AccountCode); posted != 0 || held != 0 {
		t.Fatalf("new account should be empty: %d/%d", posted, held)
	}

	got, err := env.Client(t, "lending").GetCustomer(context.Background(), &identityv1.GetCustomerRequest{CustomerId: c.ID.String()})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetKycStatus() != identityv1.KycStatus_KYC_STATUS_VERIFIED || got.GetKycTier() != 2 ||
		got.GetStatus() != identityv1.CustomerStatus_CUSTOMER_STATUS_ACTIVE || got.GetDepositAccountCode() != c.AccountCode {
		t.Fatalf("unexpected customer %+v", got)
	}

	// The BVN must not be readable from the table.
	var clear int
	if err := env.Pool.QueryRow(context.Background(), `
		SELECT count(*) FROM identity.customers
		 WHERE position($1::bytea IN bvn_ciphertext) > 0 OR position($1::bytea IN bvn_hash) > 0 OR full_name LIKE '%' || $2 || '%'`,
		[]byte(c.BVN), c.BVN).Scan(&clear); err != nil {
		t.Fatal(err)
	}
	if clear != 0 {
		t.Fatal("BVN is stored in clear")
	}
}

func TestRegistrationRejections(t *testing.T) {
	ledger := ledgertest.Start(t)
	env := identitytest.Start(t, ledger, identitytest.Options{})

	base := func() map[string]string {
		return map[string]string{"phone": env.NextPhone(), "full_name": "Ada Test Customer", "date_of_birth": "1990-05-17", "bvn": env.NextBVN(""), "pin": "482915"}
	}
	count := func() (n int) {
		if err := env.Pool.QueryRow(context.Background(), `SELECT count(*) FROM identity.customers`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	cases := []struct {
		name   string
		mutate func(m map[string]string)
		status int
		code   string
	}{
		{"underage", func(m map[string]string) { m["date_of_birth"] = time.Now().AddDate(-17, 0, 0).Format(time.DateOnly) }, 400, "INVALID_REQUEST"},
		{"weak pin", func(m map[string]string) { m["pin"] = "111111" }, 400, "INVALID_REQUEST"},
		{"short pin", func(m map[string]string) { m["pin"] = "1234" }, 400, "INVALID_REQUEST"},
		{"foreign phone", func(m map[string]string) { m["phone"] = "+14155550100" }, 400, "INVALID_REQUEST"},
		{"bad bvn", func(m map[string]string) { m["bvn"] = "123" }, 400, "INVALID_REQUEST"},
		{"bvn not found", func(m map[string]string) { m["bvn"] = env.NextBVN("0000") }, 422, "IDENTITY_NOT_VERIFIED"},
		{"bvn mismatch", func(m map[string]string) { m["bvn"] = env.NextBVN("1111") }, 422, "IDENTITY_NOT_VERIFIED"},
		{"provider timeout", func(m map[string]string) { m["bvn"] = env.NextBVN("9999") }, 503, "IDENTITY_PROVIDER_UNAVAILABLE"},
		{"provider malformed", func(m map[string]string) { m["bvn"] = env.NextBVN("8888") }, 503, "IDENTITY_PROVIDER_UNAVAILABLE"},
	}
	for _, c := range cases {
		m := base()
		c.mutate(m)
		st, body := env.Post(t, "/v1/customers", m)
		if st != c.status || errorCode(t, body) != c.code {
			t.Errorf("%s: got %d %s, want %d %s", c.name, st, body, c.status, c.code)
		}
	}
	if n := count(); n != 0 {
		t.Fatalf("%d customers were created by rejected registrations", n)
	}

	// Unknown JSON fields are refused rather than ignored.
	st, body := env.Post(t, "/v1/customers", map[string]string{"phone": env.NextPhone(), "role": "admin"})
	if st != 400 || errorCode(t, body) != "INVALID_JSON" {
		t.Fatalf("unknown field: %d %s", st, body)
	}
}

// Many simultaneous registrations with the same BVN produce one customer.
func TestConcurrentDuplicateRegistrationCreatesOneCustomer(t *testing.T) {
	ledger := ledgertest.Start(t)
	env := identitytest.Start(t, ledger, identitytest.Options{})
	bvn := env.NextBVN("")

	var created, conflict atomic.Int64
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, body := env.Post(t, "/v1/customers", map[string]string{
				"phone": env.NextPhone(), "full_name": "Ada Test Customer", "date_of_birth": "1990-05-17", "bvn": bvn, "pin": "482915"})
			switch st {
			case http.StatusCreated:
				created.Add(1)
			case http.StatusConflict:
				conflict.Add(1)
			default:
				t.Errorf("unexpected %d %s", st, body)
			}
		}()
	}
	wg.Wait()
	if created.Load() != 1 || conflict.Load() != 11 {
		t.Fatalf("created=%d conflict=%d, want 1/11", created.Load(), conflict.Load())
	}
}

func TestLoginLockoutCannotBeOutrunByParallelGuessing(t *testing.T) {
	ledger := ledgertest.Start(t)
	env := identitytest.Start(t, ledger, identitytest.Options{})
	c := env.Register(t)

	// 30 wrong guesses at once. The row lock serialises them: exactly four
	// are evaluated as wrong, the fifth triggers the lock, the rest are
	// refused without the PIN being checked at all.
	var wrong, locked atomic.Int64
	var wg sync.WaitGroup
	for range 30 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, body := env.Post(t, "/v1/sessions", map[string]string{"phone": c.Phone, "pin": "000001"})
			switch errorCode(t, body) {
			case "INVALID_CREDENTIALS":
				wrong.Add(1)
			case "CREDENTIAL_LOCKED":
				locked.Add(1)
			default:
				t.Errorf("unexpected %d %s", st, body)
			}
		}()
	}
	wg.Wait()
	if wrong.Load() != 4 || locked.Load() != 26 {
		t.Fatalf("wrong=%d locked=%d, want 4/26", wrong.Load(), locked.Load())
	}

	// Even the correct PIN is refused while locked.
	if st, body := env.Post(t, "/v1/sessions", map[string]string{"phone": c.Phone, "pin": c.PIN}); st != http.StatusTooManyRequests {
		t.Fatalf("correct PIN during lock: %d %s", st, body)
	}
	// After the lock expires the correct PIN works again.
	env.Clock.Advance(16 * time.Minute)
	if tok := env.Login(t, c.Phone, c.PIN); tok == "" {
		t.Fatal("login after lock expiry failed")
	}

	// An unknown phone number looks exactly like a wrong PIN.
	st, body := env.Post(t, "/v1/sessions", map[string]string{"phone": env.NextPhone(), "pin": "482915"})
	if st != http.StatusUnauthorized || errorCode(t, body) != "INVALID_CREDENTIALS" {
		t.Fatalf("unknown phone: %d %s", st, body)
	}
}

func TestInternalRPCsAreRestrictedByWorkload(t *testing.T) {
	ledger := ledgertest.Start(t)
	env := identitytest.Start(t, ledger, identitytest.Options{})
	c := env.Register(t)
	ctx := context.Background()

	lending := env.Client(t, "lending")
	other := env.Client(t, "notify")

	if _, err := other.GetCustomer(ctx, &identityv1.GetCustomerRequest{CustomerId: c.ID.String()}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("want PERMISSION_DENIED, got %v", err)
	}
	if _, err := other.GetCreditBureauSubject(ctx, &identityv1.GetCreditBureauSubjectRequest{CustomerId: c.ID.String(), ConsentRef: "x"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("want PERMISSION_DENIED, got %v", err)
	}
	if _, err := lending.GetCustomer(ctx, &identityv1.GetCustomerRequest{CustomerId: uuid.NewString()}); status.Code(err) != codes.NotFound {
		t.Fatalf("want NOT_FOUND, got %v", err)
	}

	// The bureau subject is released only with a consent reference, and
	// every release leaves an audit record.
	if _, err := lending.GetCreditBureauSubject(ctx, &identityv1.GetCreditBureauSubjectRequest{CustomerId: c.ID.String()}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("missing consent: want INVALID_ARGUMENT, got %v", err)
	}
	sub, err := lending.GetCreditBureauSubject(ctx, &identityv1.GetCreditBureauSubjectRequest{CustomerId: c.ID.String(), ConsentRef: "consent-123"})
	if err != nil || sub.GetBvn() != c.BVN {
		t.Fatalf("bureau subject: %v", err)
	}
	var audits int
	if err := env.Pool.QueryRow(ctx, `
		SELECT count(*) FROM identity.audit_log
		 WHERE action = 'BUREAU_SUBJECT_RELEASED' AND subject_id = $1 AND actor = 'lending' AND detail->>'consent_ref' = 'consent-123'`, c.ID).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 1 {
		t.Fatalf("%d audit records, want 1", audits)
	}
	// The audit log cannot be rewritten by the application role.
	if _, err := env.Pool.Exec(ctx, `DELETE FROM identity.audit_log`); err == nil {
		t.Fatal("application role must not be able to delete audit records")
	}

	// PIN step-up.
	v, err := lending.VerifyCustomerPin(ctx, &identityv1.VerifyCustomerPinRequest{CustomerId: c.ID.String(), Pin: c.PIN})
	if err != nil || !v.GetVerified() {
		t.Fatalf("verify pin: %+v %v", v, err)
	}
	v, err = lending.VerifyCustomerPin(ctx, &identityv1.VerifyCustomerPinRequest{CustomerId: c.ID.String(), Pin: "000001"})
	if err != nil || v.GetVerified() || v.GetLocked() {
		t.Fatalf("wrong pin: %+v %v", v, err)
	}
}

type flakyOpener struct {
	inner interface {
		OpenCustomerDeposit(ctx context.Context, id uuid.UUID) (string, error)
	}
	down atomic.Bool
}

func (f *flakyOpener) OpenCustomerDeposit(ctx context.Context, id uuid.UUID) (string, error) {
	if f.down.Load() {
		return "", errors.New("ledger unavailable")
	}
	return f.inner.OpenCustomerDeposit(ctx, id)
}

type realOpener struct {
	ledger *ledgertest.Env
	t      *testing.T
}

func (r realOpener) OpenCustomerDeposit(_ context.Context, id uuid.UUID) (string, error) {
	code := "CUST:" + id.String() + ":MAIN"
	return code, openViaClient(r.t, r.ledger, id, code)
}

// If the ledger is down at registration the customer is still created, and
// the account is opened later by the background completion task.
func TestLedgerOutageAtRegistrationIsCompletedLater(t *testing.T) {
	ledger := ledgertest.Start(t)
	opener := &flakyOpener{inner: realOpener{ledger: ledger, t: t}}
	opener.down.Store(true)
	env := identitytest.Start(t, ledger, identitytest.Options{Accounts: opener})

	st, body := env.Post(t, "/v1/customers", map[string]string{
		"phone": env.NextPhone(), "full_name": "Ada Test Customer", "date_of_birth": "1990-05-17", "bvn": env.NextBVN(""), "pin": "482915"})
	if st != http.StatusCreated || !strings.Contains(string(body), `"account_pending":true`) {
		t.Fatalf("registration during ledger outage: %d %s", st, body)
	}
	if err := env.Service.CompletePendingAccounts(context.Background()); err == nil {
		t.Fatal("completion should report the outage")
	}

	opener.down.Store(false)
	if err := env.Service.CompletePendingAccounts(context.Background()); err != nil {
		t.Fatalf("completion after recovery: %v", err)
	}
	var pending int
	if err := env.Pool.QueryRow(context.Background(), `SELECT count(*) FROM identity.customers WHERE deposit_account_code IS NULL`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("%d customers still without an account", pending)
	}
	// Running it again is harmless.
	if err := env.Service.CompletePendingAccounts(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestStaffTokenCarriesRoles(t *testing.T) {
	ledger := ledgertest.Start(t)
	env := identitytest.Start(t, ledger, identitytest.Options{})
	id, tok := env.StaffToken(t, authn.RoleOpsMaker, authn.RoleOpsViewer)
	p, err := env.Verifier.Verify(tok)
	if err != nil || p.Kind != authn.KindStaff || p.Subject != id.String() || !p.HasRole(authn.RoleOpsMaker) || p.HasRole(authn.RoleOpsChecker) {
		t.Fatalf("staff principal %+v err %v", p, err)
	}
}
