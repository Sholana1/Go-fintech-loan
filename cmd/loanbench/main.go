// Command loanbench measures the automated loan journey against locally
// running services with simulated providers.
//
// It reports, per stage and overall, the latency percentiles it observed.
// These are measurements of this machine with a provider simulator that
// answers in microseconds. They say how much time the platform's own code
// and database add; they are NOT a prediction of production performance,
// where a real credit bureau alone is budgeted 25 seconds (plan 8.11).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

type stageTimes struct {
	decision     time.Duration // T0 -> T1: submit to offer visible
	disbursement time.Duration // T2 -> T3: accept request to money in account
	system       time.Duration // decision + disbursement
}

func main() {
	customers := flag.Int("customers", 100, "number of customers to take through the journey")
	concurrency := flag.Int("concurrency", 8, "concurrent journeys")
	identityURL := flag.String("identity", "http://127.0.0.1:8002", "identity base URL")
	lendingURL := flag.String("lending", "http://127.0.0.1:8003", "lending base URL")
	flag.Parse()

	client := &http.Client{Timeout: 30 * time.Second}
	var (
		mu       sync.Mutex
		times    []stageTimes
		failures atomic.Int64
		wg       sync.WaitGroup
		work     = make(chan int)
	)
	started := time.Now()
	for range *concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				st, err := journey(context.Background(), client, *identityURL, *lendingURL)
				if err != nil {
					failures.Add(1)
					fmt.Fprintf(os.Stderr, "journey %d: %v\n", i, err)
					continue
				}
				mu.Lock()
				times = append(times, st)
				mu.Unlock()
			}
		}()
	}
	for i := 0; i < *customers; i++ {
		work <- i
	}
	close(work)
	wg.Wait()
	elapsed := time.Since(started)

	fmt.Printf("environment: %s/%s, %d CPUs, Go %s\n", runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), runtime.Version())
	fmt.Printf("workload: %d journeys, concurrency %d, providers simulated (no network latency)\n", *customers, *concurrency)
	fmt.Printf("completed %d, failed %d, in %s (%.1f journeys/s)\n\n", len(times), failures.Load(), elapsed.Round(time.Millisecond), float64(len(times))/elapsed.Seconds())
	if len(times) == 0 {
		os.Exit(1)
	}
	report("decision (T0->T1)", times, func(s stageTimes) time.Duration { return s.decision })
	report("disbursement (T2->T3)", times, func(s stageTimes) time.Duration { return s.disbursement })
	report("system time", times, func(s stageTimes) time.Duration { return s.system })
	fmt.Println("\nThese figures measure the platform's own overhead on this machine. They are not production numbers.")
	if failures.Load() > 0 {
		os.Exit(1)
	}
}

func report(name string, all []stageTimes, pick func(stageTimes) time.Duration) {
	d := make([]time.Duration, len(all))
	for i, s := range all {
		d[i] = pick(s)
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	p := func(q float64) time.Duration {
		return d[min(len(d)-1, int(float64(len(d))*q))].Round(100 * time.Microsecond)
	}
	fmt.Printf("%-24s p50 %-10s p95 %-10s p99 %-10s max %s\n", name, p(0.50), p(0.95), p(0.99), d[len(d)-1].Round(100*time.Microsecond))
}

func journey(ctx context.Context, c *http.Client, identityURL, lendingURL string) (stageTimes, error) {
	var st stageTimes
	const pin = "482915"
	// Random identities, so repeated runs do not collide with customers
	// registered by earlier runs. A rare collision is retried.
	var phone string
	for attempt := 0; ; attempt++ {
		n := rand.Int64N(100_000_000)
		phone = fmt.Sprintf("+23480%08d", n)
		bvn := fmt.Sprintf("%07d4242", rand.Int64N(10_000_000))
		body, err := call(ctx, c, "POST", identityURL+"/v1/customers", "", "", map[string]string{
			"phone": phone, "full_name": "Ada Test Customer", "date_of_birth": "1990-05-17", "bvn": bvn, "pin": pin}, 201)
		if err == nil {
			break
		}
		if attempt >= 5 || !bytes.Contains(body, []byte("REGISTRATION_CONFLICT")) {
			return st, fmt.Errorf("register: %w", err)
		}
	}
	var session struct {
		AccessToken string `json:"access_token"`
	}
	body, err := call(ctx, c, "POST", identityURL+"/v1/sessions", "", "", map[string]string{"phone": phone, "pin": pin}, 200)
	if err != nil {
		return st, fmt.Errorf("login: %w", err)
	}
	_ = json.Unmarshal(body, &session)

	// T0: submit.
	t0 := time.Now()
	body, err = call(ctx, c, "POST", lendingURL+"/v1/loan-applications", session.AccessToken, uuid.NewString(), map[string]any{
		"product_id": "personal-loan", "amount_minor": 10_000_000, "currency": "NGN", "tenor_months": 3,
		"stated_monthly_income_minor": 30_000_000, "consent_credit_check": true}, 202)
	if err != nil {
		return st, fmt.Errorf("submit: %w", err)
	}
	var app struct {
		ApplicationID string `json:"application_id"`
		Status        string `json:"status"`
		Offer         *struct {
			OfferID        string `json:"offer_id"`
			DisclosureHash string `json:"disclosure_hash"`
		} `json:"offer"`
	}
	_ = json.Unmarshal(body, &app)

	// T1: poll until the decision is visible. The poll interval bounds the
	// resolution of this measurement.
	deadline := time.Now().Add(60 * time.Second)
	for app.Status == "PROCESSING" {
		if time.Now().After(deadline) {
			return st, fmt.Errorf("no decision within 60s")
		}
		time.Sleep(5 * time.Millisecond)
		body, err = call(ctx, c, "GET", lendingURL+"/v1/loan-applications/"+app.ApplicationID, session.AccessToken, "", nil, 200)
		if err != nil {
			return st, fmt.Errorf("poll: %w", err)
		}
		_ = json.Unmarshal(body, &app)
	}
	st.decision = time.Since(t0)
	if app.Status != "OFFER_READY" || app.Offer == nil {
		return st, fmt.Errorf("unexpected decision %s", app.Status)
	}

	// T2 -> T3: accept; the response is 201 only when the ledger has posted.
	t2 := time.Now()
	body, err = call(ctx, c, "POST", lendingURL+"/v1/loan-offers/"+app.Offer.OfferID+"/accept", session.AccessToken, uuid.NewString(), map[string]any{
		"disclosure_hash": app.Offer.DisclosureHash, "pin": pin, "auto_debit_authorised": true,
		"destination": map[string]string{"type": "DEPOSIT_ACCOUNT"}}, 201)
	if err != nil {
		return st, fmt.Errorf("accept: %w", err)
	}
	st.disbursement = time.Since(t2)
	var loan struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(body, &loan)
	if loan.Status != "ACTIVE" {
		return st, fmt.Errorf("loan status %s after accept", loan.Status)
	}
	st.system = st.decision + st.disbursement
	return st, nil
}

func call(ctx context.Context, c *http.Client, method, url, token, idemKey string, payload any, want int) ([]byte, error) {
	var reader io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		return body, fmt.Errorf("%s %s: HTTP %d: %s", method, url, resp.StatusCode, body)
	}
	return body, nil
}
