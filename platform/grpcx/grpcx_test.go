package grpcx_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	"bankplatform.internal/platform/devcert"
	"bankplatform.internal/platform/grpcx"
)

// echoServer is a minimal service used to observe interceptor behaviour. It
// is registered by hand so the test needs no generated code of its own.
type echoServer struct {
	block   chan struct{} // when non-nil, handlers wait on it
	started chan struct{}
	callers chan string
}

var echoDesc = grpc.ServiceDesc{
	ServiceName: "test.Echo",
	HandlerType: (*any)(nil),
	Methods: []grpc.MethodDesc{
		{MethodName: "Call", Handler: func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
			in := new(healthpb.HealthCheckRequest)
			if err := dec(in); err != nil {
				return nil, err
			}
			h := func(ctx context.Context, req any) (any, error) {
				s := srv.(*echoServer)
				if in.GetService() == "panic" {
					panic("boom")
				}
				s.callers <- grpcx.Caller(ctx)
				if s.block != nil {
					s.started <- struct{}{}
					<-s.block
				}
				return &healthpb.HealthCheckResponse{}, nil
			}
			return interceptor(ctx, in, &grpc.UnaryServerInfo{Server: srv, FullMethod: "/test.Echo/Call"}, h)
		}},
	},
}

func start(t *testing.T, maxConcurrent int, es *echoServer) (addr string, ca *devcert.Authority) {
	t.Helper()
	ca, err := devcert.NewAuthority()
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ca.Issue(grpcx.WorkloadURI("server"), "server.test")
	if err != nil {
		t.Fatal(err)
	}
	srv, _, err := grpcx.NewServer(grpcx.ServerConfig{
		TLS: ca.ServerConfig(leaf), Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), MaxConcurrent: maxConcurrent,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv.RegisterService(&echoDesc, es)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String(), ca
}

func call(ctx context.Context, conn *grpc.ClientConn, service string) error {
	return conn.Invoke(ctx, "/test.Echo/Call", &healthpb.HealthCheckRequest{Service: service}, &healthpb.HealthCheckResponse{})
}

func TestCallerIdentityComesFromTheClientCertificate(t *testing.T) {
	es := &echoServer{callers: make(chan string, 1)}
	addr, ca := start(t, 8, es)

	leaf, _ := ca.Issue(grpcx.WorkloadURI("lending"))
	conn, err := grpcx.Dial(grpcx.ClientConfig{Target: addr, TLS: ca.ClientConfig(leaf, "server.test")})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := call(context.Background(), conn, ""); err != nil {
		t.Fatal(err)
	}
	if got := <-es.callers; got != "lending" {
		t.Fatalf("caller = %q, want lending", got)
	}
}

func TestClientsWithoutATrustedCertificateAreRefused(t *testing.T) {
	es := &echoServer{callers: make(chan string, 1)}
	addr, ca := start(t, 8, es)

	// A certificate from a different CA: the TLS handshake must fail.
	otherCA, _ := devcert.NewAuthority()
	rogue, _ := otherCA.Issue(grpcx.WorkloadURI("lending"))
	cfg := ca.ClientConfig(rogue, "server.test")
	conn, err := grpcx.Dial(grpcx.ClientConfig{Target: addr, TLS: cfg, DefaultTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := call(context.Background(), conn, ""); status.Code(err) != codes.Unavailable && status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("rogue client: want connection failure, got %v", err)
	}
	select {
	case c := <-es.callers:
		t.Fatalf("handler ran for an untrusted client (caller %q)", c)
	default:
	}
}

func TestServerRequiresMutualTLSConfiguration(t *testing.T) {
	if _, _, err := grpcx.NewServer(grpcx.ServerConfig{Logger: slog.Default()}); err == nil {
		t.Fatal("NewServer must refuse to start without mutual TLS")
	}
}

func TestPanicBecomesInternalAndServerSurvives(t *testing.T) {
	es := &echoServer{callers: make(chan string, 2)}
	addr, ca := start(t, 8, es)
	leaf, _ := ca.Issue(grpcx.WorkloadURI("lending"))
	conn, _ := grpcx.Dial(grpcx.ClientConfig{Target: addr, TLS: ca.ClientConfig(leaf, "server.test")})
	defer conn.Close()

	if err := call(context.Background(), conn, "panic"); status.Code(err) != codes.Internal {
		t.Fatalf("want INTERNAL, got %v", err)
	}
	if err := call(context.Background(), conn, ""); err != nil {
		t.Fatalf("server did not survive the panic: %v", err)
	}
}

func TestConcurrencyLimitShedsLoad(t *testing.T) {
	es := &echoServer{callers: make(chan string, 16), block: make(chan struct{}), started: make(chan struct{}, 16)}
	addr, ca := start(t, 2, es)
	var once sync.Once
	unblock := func() { once.Do(func() { close(es.block) }) }
	defer unblock() // never leave handlers blocked if an assertion fails
	leaf, _ := ca.Issue(grpcx.WorkloadURI("lending"))
	conn, _ := grpcx.Dial(grpcx.ClientConfig{Target: addr, TLS: ca.ClientConfig(leaf, "server.test")})
	defer conn.Close()

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); _ = call(context.Background(), conn, "") }()
	}
	<-es.started
	<-es.started

	// Both slots are busy: the third call is rejected at once, not queued.
	begin := time.Now()
	err := call(context.Background(), conn, "")
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("want RESOURCE_EXHAUSTED, got %v", err)
	}
	if time.Since(begin) > time.Second {
		t.Fatalf("rejection took %v; it should be immediate", time.Since(begin))
	}
	unblock()
	wg.Wait()
}

func TestDefaultDeadlineIsApplied(t *testing.T) {
	es := &echoServer{callers: make(chan string, 4), block: make(chan struct{}), started: make(chan struct{}, 4)}
	addr, ca := start(t, 8, es)
	defer close(es.block)
	leaf, _ := ca.Issue(grpcx.WorkloadURI("lending"))
	conn, _ := grpcx.Dial(grpcx.ClientConfig{Target: addr, TLS: ca.ClientConfig(leaf, "server.test"), DefaultTimeout: 150 * time.Millisecond})
	defer conn.Close()

	// No deadline on the context: the client must still give up.
	if err := call(context.Background(), conn, ""); status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("want DEADLINE_EXCEEDED, got %v", err)
	}
}

func TestRetryIdempotentRetriesOnlyTransientErrors(t *testing.T) {
	var retried atomic.Int64
	policy := grpcx.RetryPolicy{Attempts: 3, BaseBackoff: time.Millisecond, PerAttempt: time.Second,
		OnRetry: func(int, error) { retried.Add(1) }}

	calls := 0
	err := grpcx.RetryIdempotent(context.Background(), policy, func(context.Context) error {
		calls++
		if calls < 3 {
			return status.Error(codes.Unavailable, "down")
		}
		return nil
	})
	if err != nil || calls != 3 || retried.Load() != 2 {
		t.Fatalf("err=%v calls=%d retried=%d", err, calls, retried.Load())
	}

	calls = 0
	err = grpcx.RetryIdempotent(context.Background(), policy, func(context.Context) error {
		calls++
		return status.Error(codes.FailedPrecondition, "insufficient funds")
	})
	if status.Code(err) != codes.FailedPrecondition || calls != 1 {
		t.Fatalf("a business rejection must not be retried: err=%v calls=%d", err, calls)
	}

	calls = 0
	err = grpcx.RetryIdempotent(context.Background(), policy, func(context.Context) error {
		calls++
		return status.Error(codes.Unavailable, "down")
	})
	if status.Code(err) != codes.Unavailable || calls != 3 {
		t.Fatalf("attempts must be bounded: err=%v calls=%d", err, calls)
	}

	if err := grpcx.RetryIdempotent(context.Background(), grpcx.RetryPolicy{Attempts: 3, PerAttempt: time.Second}, func(context.Context) error { return nil }); err == nil {
		t.Fatal("a policy without OnRetry must be refused: retries may not be silent")
	}
}
