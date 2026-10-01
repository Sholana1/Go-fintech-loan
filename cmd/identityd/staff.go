package main

import (
	"context"
	"fmt"
	"strings"

	"bankplatform.internal/platform/config"
	"bankplatform.internal/platform/pgxutil"
	"bankplatform.internal/services/identity/app"
	"bankplatform.internal/services/identity/postgres"
	"bankplatform.internal/services/identity/secrets"
)

// createStaff bootstraps a staff account from environment variables so the
// password never appears in shell history or process arguments.
func createStaff(ctx context.Context) error {
	l := config.FromEnv()
	env := l.Environment()
	dsn := l.String("IDENTITY_OWNER_DATABASE_URL")
	email := l.String("STAFF_EMAIL")
	password := l.String("STAFF_PASSWORD")
	roles := strings.Split(l.String("STAFF_ROLES"), ",")
	if err := l.Err(); err != nil {
		return err
	}
	if !env.AllowsSimulators() {
		return fmt.Errorf("create-staff is refused in environment %q: production staff sign in through SSO", env)
	}
	pool, err := pgxutil.NewPool(ctx, dsn, pgxutil.PoolOptions{ApplicationName: "identityd-bootstrap", MaxConns: 2})
	if err != nil {
		return err
	}
	defer pool.Close()
	id, err := app.CreateStaff(ctx, postgres.NewStore(pool), secrets.DefaultHashParams, email, password, roles)
	if err != nil {
		return err
	}
	fmt.Println(id)
	return nil
}
