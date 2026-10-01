// Package migrations embeds the ledger database migrations.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
