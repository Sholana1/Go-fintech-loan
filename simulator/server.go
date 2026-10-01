// Package simulator is a deterministic HTTP simulator of the external
// providers the platform depends on:
//
//	kyc.go              identity (BVN) verification
//	bureau.go           credit bureau reports
//	payout_*.go         outbound bank transfers: name enquiry, transfer,
//	                    status query, settlement report, callbacks
//	collections.go      inbound payments (a customer paying from outside):
//	                    payment status query and webhooks
//	notifications.go    customer notifications (accepts messages, sends
//	                    nothing to anyone)
//
// IT IS NOT A REAL PROVIDER AND MODELS NO REAL PROVIDER'S API. The paths,
// fields and codes are this project's own simulator contract (documented in
// docs/contracts/provider-simulator.md). Each simulated provider has a
// client adapter that makes real HTTP calls to it; the services refuse to
// start with those adapters outside local and test environments. Where a
// real provider's public API could be verified (Paystack, for transfers and
// payment verification) a production adapter exists beside the simulator
// adapter; see docs/integrations.
//
// The simulator exists to make every outcome reproducible on demand:
// success, rejection, timeout, a success whose response is lost, a delayed
// success, a malformed response, duplicate and out-of-order callbacks, and
// settlement reports that disagree with our records. Behaviour is selected
// by the last digits of the BVN or account number, or set explicitly by a
// test. There are no sleeps: a "timeout" is a handler that waits for the
// caller to give up.
package simulator

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// Options configure the simulator.
type Options struct {
	APIKey string
	// CallbackSecret signs callbacks and webhooks. CallbackURL, when set,
	// receives payout callbacks; CollectionWebhookURL receives inbound
	// payment webhooks.
	CallbackSecret       string
	CallbackURL          string
	CollectionWebhookURL string
	// TransferCallbackURL receives payout callbacks for references that
	// start with "tr-" (customer transfers, owned by the payments service).
	// All other payout callbacks go to CallbackURL (loan payouts, lending).
	TransferCallbackURL string
	// AutoCallback sends a callback after each transfer that reaches a final
	// status. Tests leave it off and send callbacks explicitly.
	AutoCallback bool
	Now          func() time.Time
}

// Server is the simulator.
type Server struct {
	opts Options

	mu            sync.Mutex
	bureauByBVN   map[string]BureauBehaviour
	bureauCalls   map[string]int
	bureauReports map[string][]byte // request_ref -> response body (idempotent)
	names         map[string]string
	payoutByAcct  map[string]string // account number -> behaviour code override
	transfers     map[string]*Transfer
	extraReport   []reportItem
	hidden        map[string]bool // references omitted from settlement reports
	kycByBVN      map[string]string
	kycChecks     map[string][]byte // request_ref -> response body (idempotent)
	kycCalls      map[string]int
	payments      map[string]*Payment
	paymentFaults map[string]string
	messages      map[string]Message
	messageOrder  []string
	notifyFault   string
	notifyCalls   int
	client        *http.Client
}

// New returns a simulator.
func New(opts Options) *Server {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Server{
		opts:          opts,
		bureauByBVN:   map[string]BureauBehaviour{},
		bureauCalls:   map[string]int{},
		bureauReports: map[string][]byte{},
		names:         map[string]string{},
		payoutByAcct:  map[string]string{},
		transfers:     map[string]*Transfer{},
		hidden:        map[string]bool{},
		kycByBVN:      map[string]string{},
		kycChecks:     map[string][]byte{},
		kycCalls:      map[string]int{},
		payments:      map[string]*Payment{},
		paymentFaults: map[string]string{},
		messages:      map[string]Message{},
		client:        &http.Client{Timeout: 5 * time.Second},
	}
}

// Handler returns the simulator's HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /kyc/v1/bvn-verifications", s.auth(s.verifyBVN))
	mux.HandleFunc("POST /bureau/v1/reports", s.auth(s.bureauReport))
	mux.HandleFunc("POST /payout/v1/accounts/resolve", s.auth(s.resolveAccount))
	mux.HandleFunc("POST /payout/v1/transfers", s.auth(s.createTransfer))
	mux.HandleFunc("GET /payout/v1/transfers/{reference}", s.auth(s.getTransfer))
	mux.HandleFunc("GET /payout/v1/settlement-report", s.auth(s.settlementReport))
	mux.HandleFunc("GET /collections/v1/payments/{reference}", s.auth(s.getPayment))
	mux.HandleFunc("POST /collections/v1/sandbox/payments", s.auth(s.sandboxPay))
	mux.HandleFunc("POST /notify/v1/messages", s.auth(s.acceptMessage))
	return mux
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.opts.APIKey != "" && r.Header.Get("X-Api-Key") != s.opts.APIKey {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "UNAUTHORIZED"})
			return
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// hang waits until the caller gives up. It models a provider that accepted
// the connection and never answered.
func hang(r *http.Request) { <-r.Context().Done() }
