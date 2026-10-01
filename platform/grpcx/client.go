package grpcx

import (
	"context"
	"crypto/tls"
	"errors"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

// ClientConfig configures Dial.
type ClientConfig struct {
	Target string      // host:port
	TLS    *tls.Config // required; carries this workload's client certificate
	// DefaultTimeout is applied to calls whose context has no deadline, so no
	// call can wait forever. Callers should still set their own deadlines.
	DefaultTimeout time.Duration // default 5s
	MaxRecvBytes   int           // default 1 MiB
}

// Dial returns a client connection with mutual TLS, tracing, a default
// deadline, and no automatic retries. gRPC's built-in retry is left off on
// purpose: a retry is only safe when the RPC's idempotency contract says so,
// and that decision belongs at the call site (see RetryIdempotent).
func Dial(cfg ClientConfig) (*grpc.ClientConn, error) {
	if cfg.TLS == nil {
		return nil, errors.New("grpcx: client TLS config is required")
	}
	if cfg.DefaultTimeout <= 0 {
		cfg.DefaultTimeout = 5 * time.Second
	}
	if cfg.MaxRecvBytes <= 0 {
		cfg.MaxRecvBytes = 1 << 20
	}
	return grpc.NewClient(cfg.Target,
		grpc.WithTransportCredentials(credentials.NewTLS(cfg.TLS)),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
		grpc.WithDisableRetry(),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(cfg.MaxRecvBytes)),
		grpc.WithChainUnaryInterceptor(defaultTimeout(cfg.DefaultTimeout)),
	)
}

func defaultTimeout(d time.Duration) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if _, ok := ctx.Deadline(); !ok {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, d)
			defer cancel()
		}
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

// RetryPolicy bounds RetryIdempotent.
type RetryPolicy struct {
	Attempts    int           // total attempts, including the first
	BaseBackoff time.Duration // doubled after each failed attempt
	PerAttempt  time.Duration // deadline for each attempt
	// OnRetry is called before each retry so the retry is visible in logs
	// and metrics. It must not be nil: this platform has no silent retries.
	OnRetry func(attempt int, err error)
}

// RetryIdempotent runs call up to p.Attempts times, retrying only transient
// transport-level outcomes (UNAVAILABLE, ABORTED, DEADLINE_EXCEEDED,
// RESOURCE_EXHAUSTED).
//
// It must only wrap RPCs that are idempotent by contract, called with the
// same idempotency reference on every attempt. For the ledger that means the
// same PostingRef or HoldRef: a retry after an ambiguous failure then returns
// the original result instead of moving money twice.
func RetryIdempotent(ctx context.Context, p RetryPolicy, call func(ctx context.Context) error) error {
	if p.Attempts <= 0 || p.OnRetry == nil || p.PerAttempt <= 0 {
		return errors.New("grpcx: RetryPolicy requires Attempts, PerAttempt and OnRetry")
	}
	var err error
	backoff := p.BaseBackoff
	for attempt := 1; attempt <= p.Attempts; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, p.PerAttempt)
		err = call(attemptCtx)
		cancel()
		if err == nil || !Transient(err) || attempt == p.Attempts {
			return err
		}
		if ctx.Err() != nil {
			return err
		}
		p.OnRetry(attempt, err)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(backoff):
		}
		backoff *= 2
	}
	return err
}

// Transient reports whether err is a gRPC status that may succeed if the
// same idempotent request is sent again.
func Transient(err error) bool {
	switch status.Code(err) {
	case codes.Unavailable, codes.Aborted, codes.DeadlineExceeded, codes.ResourceExhausted:
		return true
	default:
		return false
	}
}
