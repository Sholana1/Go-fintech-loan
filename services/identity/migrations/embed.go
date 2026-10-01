// Package migrations embeds the identity database migrations.
package migrations

import (
	"context"
	"embed"

	"bankplatform.internal/platform/migrate"
)

//go:embed *.sql
var FS embed.FS

// Apply runs pending migrations as the schema owner.
func Apply(ctx context.Context, ownerDSN string) error {
	return migrate.Up(ctx, ownerDSN, FS)
}
