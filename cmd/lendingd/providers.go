package main

import (
	"time"

	"bankplatform.internal/services/lending/app"
	"bankplatform.internal/services/lending/httpapi"
	"bankplatform.internal/services/lending/providers/bureausim"
	"bankplatform.internal/services/lending/providers/collectsim"
	"bankplatform.internal/services/lending/providers/payoutsim"
	"bankplatform.internal/services/lending/providers/paystack"
)

// providers are the external-provider adapters selected by configuration.
type providers struct {
	Bureau          app.CreditBureau
	Payouts         app.PayoutProvider             // nil: external payouts disabled
	Collections     app.PaymentVerifier            // nil: external repayments disabled
	PayoutCallbacks httpapi.PayoutCallbackVerifier // nil: endpoint disabled
	PaymentWebhooks httpapi.PaymentWebhookVerifier // nil: endpoint disabled
}

// buildProviders is the one place an adapter is chosen. loadSettings has
// already refused simulators outside local and test environments and
// checked that each selected adapter has its credentials.
func buildProviders(cfg settings, currency string) (providers, error) {
	var p providers
	// loadSettings accepts only "simulator" for the bureau.
	p.Bureau = bureausim.New(cfg.SimURL, cfg.SimAPIKey, time.Now)

	var ps *paystack.Client
	if cfg.PayoutProvider == "paystack" || cfg.CollectionProvider == "paystack" {
		var err error
		ps, err = paystack.New(paystack.Config{BaseURL: cfg.PaystackBaseURL, SecretKey: cfg.PaystackSecretKey, Currency: currency})
		if err != nil {
			return providers{}, err
		}
	}
	paystackHooks := paystack.Webhooks{SecretKey: cfg.PaystackSecretKey}

	switch cfg.PayoutProvider {
	case "simulator":
		p.Payouts = payoutsim.New(cfg.SimURL, cfg.SimAPIKey, currency)
		p.PayoutCallbacks = payoutsim.Callbacks{Secret: cfg.SimCallbackSecret}
	case "paystack":
		p.Payouts = ps
		if cfg.PaystackWebhooks {
			p.PayoutCallbacks = paystackHooks
		}
	}
	switch cfg.CollectionProvider {
	case "simulator":
		p.Collections = collectsim.New(cfg.SimURL, cfg.SimAPIKey)
		p.PaymentWebhooks = collectsim.Webhooks{Secret: cfg.SimCallbackSecret}
	case "paystack":
		p.Collections = ps
		if cfg.PaystackWebhooks {
			p.PaymentWebhooks = paystackHooks
		}
	}
	return p, nil
}
