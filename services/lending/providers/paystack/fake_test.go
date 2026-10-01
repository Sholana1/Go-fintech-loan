package paystack_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"bankplatform.internal/services/lending/providers/paystack"
)

// These tests run the adapter against a local HTTP server that answers with
// the shapes Paystack's OpenAPI specification describes. They prove the
// adapter sends what the specification asks for and interprets what it
// promises. They do NOT prove the adapter works against Paystack: that needs
// a sandbox run with real credentials.

const testKey = "sk_test_not_a_real_key"

// call is one request the fake received.
type call struct {
	Method string
	Path   string
	Query  string
	Auth   string
	Body   map[string]any
}

// fake is a scriptable stand-in for api.paystack.co.
type fake struct {
	t      *testing.T
	mu     sync.Mutex
	calls  []call
	routes map[string]http.HandlerFunc // "METHOD /path"
	srv    *httptest.Server
}

func newFake(t *testing.T) *fake {
	f := &fake{t: t, routes: map[string]http.HandlerFunc{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := call{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Auth: r.Header.Get("Authorization")}
		if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
			_ = json.Unmarshal(raw, &c.Body)
		}
		f.mu.Lock()
		f.calls = append(f.calls, c)
		h := f.routes[r.Method+" "+r.URL.Path]
		f.mu.Unlock()
		if h == nil {
			t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		h(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) on(route string, h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[route] = h
}

// reply answers with Paystack's envelope.
func reply(status int, ok bool, message string, data any) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": ok, "message": message, "data": data})
	}
}

func (f *fake) recorded() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call{}, f.calls...)
}

func (f *fake) client() *paystack.Client {
	c, err := paystack.New(paystack.Config{BaseURL: f.srv.URL, SecretKey: testKey, Currency: "NGN"})
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}
