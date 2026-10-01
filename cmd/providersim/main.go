// Command providersim runs the provider simulator (identity verification,
// credit bureau, payout provider and inbound-payment provider) for local
// development and benchmarks. It is not a real provider
// and refuses to run outside local and test environments.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"bankplatform.internal/platform/config"
	"bankplatform.internal/platform/lifecycle"
	"bankplatform.internal/platform/obs"
	sim "bankplatform.internal/simulator"
)

func main() {
	l := config.FromEnv()
	env := l.Environment()
	addr := l.StringOr("PROVIDER_SIM_ADDR", ":8090")
	opts := sim.Options{
		APIKey:               l.String("PROVIDER_SIM_API_KEY"),
		CallbackSecret:       l.String("PROVIDER_SIM_CALLBACK_SECRET"),
		CallbackURL:          l.StringOr("PROVIDER_SIM_CALLBACK_URL", ""),
		CollectionWebhookURL: l.StringOr("PROVIDER_SIM_COLLECTION_WEBHOOK_URL", ""),
		TransferCallbackURL:  l.StringOr("PROVIDER_SIM_TRANSFER_CALLBACK_URL", ""),
		AutoCallback:         l.BoolOr("PROVIDER_SIM_AUTO_CALLBACK", true),
	}
	if err := l.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "providersim:", err)
		os.Exit(1)
	}
	if !env.AllowsSimulators() {
		fmt.Fprintf(os.Stderr, "providersim: refused in environment %q\n", env)
		os.Exit(1)
	}
	log := obs.NewLogger("providersim", string(env))
	log.Info("provider simulator starting (NOT a real provider)", "addr", addr)
	if err := lifecycle.Run(context.Background(), log, 5*time.Second,
		lifecycle.HTTPServer("http", addr, sim.New(opts).Handler(), 3*time.Second)); err != nil {
		os.Exit(1)
	}
}
