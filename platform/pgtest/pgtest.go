// Package pgtest creates throwaway PostgreSQL databases for integration tests.
//
// Tests run against a real server (the compose instance on localhost:55432 by
// default). Each call creates a uniquely named database owned by the service's
// owner role, so test packages can run in parallel without sharing state.
//
// When the server is unreachable the test is skipped, unless
// REQUIRE_INTEGRATION=1 is set (as `make test` does), in which case it fails:
// a skipped financial test must never look like a pass in CI.
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

const defaultAdminURL = "postgres://postgres:devpassword@localhost:55432/postgres?sslmode=disable"

// Database describes a freshly created test database.
type Database struct {
	Name     string
	OwnerDSN string // for migrations
	AppDSN   string // what the service under test uses
	AdminDSN string // superuser, for fault injection in tests
}

// New creates a database owned by ownerRole and returns connection strings for
// the owner and application roles. Roles must already exist (see
// deploy/postgres-init.sql). The database is dropped when the test ends.
func New(t testing.TB, ownerRole, appRole string) Database {
	t.Helper()
	adminURL := os.Getenv("TEST_PG_ADMIN_URL")
	if adminURL == "" {
		adminURL = defaultAdminURL
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		if os.Getenv("REQUIRE_INTEGRATION") == "1" {
			t.Fatalf("integration database unavailable at TEST_PG_ADMIN_URL: %v", err)
		}
		t.Skipf("integration database unavailable (start it with `make infra-up`): %v", err)
	}
	defer admin.Close(ctx)

	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	name := "t_" + hex.EncodeToString(suffix)

	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s OWNER %s`, pgx.Identifier{name}.Sanitize(), pgx.Identifier{ownerRole}.Sanitize())); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c, err := pgx.Connect(ctx, adminURL)
		if err != nil {
			t.Logf("cleanup connect: %v", err)
			return
		}
		defer c.Close(ctx)
		if _, err := c.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, pgx.Identifier{name}.Sanitize())); err != nil {
			t.Logf("drop test database: %v", err)
		}
	})

	return Database{
		Name:     name,
		OwnerDSN: withUserAndDB(t, adminURL, ownerRole, name),
		AppDSN:   withUserAndDB(t, adminURL, appRole, name),
		AdminDSN: withUserAndDB(t, adminURL, "", name),
	}
}

// withUserAndDB rewrites the admin URL to target another role and database.
// Local roles all share the development password; role "" keeps the admin user.
func withUserAndDB(t testing.TB, adminURL, role, db string) string {
	t.Helper()
	u, err := url.Parse(adminURL)
	if err != nil {
		t.Fatalf("parse TEST_PG_ADMIN_URL: %v", err)
	}
	if role != "" {
		pw, _ := u.User.Password()
		u.User = url.UserPassword(role, pw)
	}
	u.Path = "/" + db
	return u.String()
}
