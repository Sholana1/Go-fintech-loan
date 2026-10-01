// Command identityd runs the identity service.
//
//	identityd serve         REST (registration, login) and internal gRPC
//	identityd migrate       apply migrations (owner role)
//	identityd create-staff  bootstrap a staff account (local/test only)
package main

import (
	"context"
	"fmt"
	"os"

	"bankplatform.internal/platform/config"
	"bankplatform.internal/services/identity/migrations"
)

var version = "dev"

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(context.Background())
	case "migrate":
		l := config.FromEnv()
		dsn := l.String("IDENTITY_OWNER_DATABASE_URL")
		if err = l.Err(); err == nil {
			err = migrations.Apply(context.Background(), dsn)
		}
	case "create-staff":
		err = createStaff(context.Background())
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "identityd:", err)
		os.Exit(1)
	}
}
