// Command lendingd runs the lending service (personal loans).
//
//	lendingd serve    REST API, workers, outbox relay and event consumer
//	lendingd migrate  apply migrations (owner role)
package main

import (
	"context"
	"fmt"
	"os"

	"bankplatform.internal/platform/config"
	"bankplatform.internal/services/lending/migrations"
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
		dsn := l.String("LENDING_OWNER_DATABASE_URL")
		if err = l.Err(); err == nil {
			err = migrations.Apply(context.Background(), dsn)
		}
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "lendingd:", err)
		os.Exit(1)
	}
}
