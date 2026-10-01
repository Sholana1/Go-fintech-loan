// Package migrate applies a service's embedded SQL migrations with goose.
//
// Migrations run as the schema owner role, in a separate step from service
// startup (`<service>d migrate`). The application role never has DDL rights.
package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/pressly/goose/v3"
)

// Up applies all pending migrations found at the root of fsys.
func Up(ctx context.Context, ownerDSN string, fsys fs.FS) error {
	db, err := sql.Open("pgx", ownerDSN)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	provider, err := goose.NewProvider(goose.DialectPostgres, db, fsys)
	if err != nil {
		return fmt.Errorf("load migrations: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}
