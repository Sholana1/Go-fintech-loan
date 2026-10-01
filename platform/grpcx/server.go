// Package grpcx builds gRPC servers and clients with the platform's security
// and resilience defaults, so every internal hop behaves the same way.
//
// Server side:
//   - Mutual TLS is mandatory. There is no insecure mode, in any environment;
//     local development and tests use certificates from platform/devcert.
//   - The caller's workload identity is taken from the verified client
//     certificate (SPIFFE-style URI SAN) and placed in the context. Services
//     authorise on that identity.
//   - Panics are converted to INTERNAL and logged.
//   - Concurrency is bounded: beyond MaxConcurrent in-flight calls the server
//     answers RESOURCE_EXHAUSTED instead of queueing without limit.
//   - Message size is capped.
//   - OpenTelemetry stats handler, health service, graceful stop.
//
// Client side: see client.go.
package grpcx

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net/url"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
)

// TrustDomain is the SPIFFE trust domain of platform workloads.
const TrustDomain = "bankplatform.internal"

// WorkloadURI returns the URI SAN that identifies a service.
func WorkloadURI(service string) *url.URL {
	return &url.URL{Scheme: "spiffe", Host: TrustDomain, Path: "/svc/" + service}
}

type callerKey struct{}

// Caller returns the authenticated workload identity of the peer (for example
// "lending"), or "" if the context did not pass through the server
// interceptor.
func Caller(ctx context.Context) string {
	s, _ := ctx.Value(callerKey{}).(string)
	return s
}

// ServerConfig configures NewServer.
type ServerConfig struct {
	TLS           *tls.Config // required; must demand and verify client certificates
	Logger        *slog.Logger
	MaxRecvBytes  int // default 1 MiB
	MaxConcurrent int // default 256
}

// NewServer returns a gRPC server with the platform interceptors installed
// and the standard health service registered.
func NewServer(cfg ServerConfig) (*grpc.Server, *health.Server, error) {
	if cfg.TLS == nil || cfg.TLS.ClientAuth != tls.RequireAndVerifyClientCert {
		return nil, nil, errors.New("grpcx: server TLS config must require and verify client certificates")
	}
	if cfg.MaxRecvBytes <= 0 {
		cfg.MaxRecvBytes = 1 << 20
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 256
	}
	slots := make(chan struct{}, cfg.MaxConcurrent)

	srv := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(cfg.TLS)),
		grpc.MaxRecvMsgSize(cfg.MaxRecvBytes),
		// Per-connection stream cap. It is set above the global limit so
		// that overload is answered by the limiter below with an explicit
		// RESOURCE_EXHAUSTED; a lower HTTP/2 cap would make clients queue
		// silently until their deadline instead.
		grpc.MaxConcurrentStreams(uint32(cfg.MaxConcurrent)*4),
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: 30 * time.Second, Timeout: 10 * time.Second}),
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		grpc.ChainUnaryInterceptor(
			recoverInterceptor(cfg.Logger),
			callerInterceptor(),
			limitInterceptor(slots),
			logInterceptor(cfg.Logger),
		),
	)
	hs := health.NewServer()
	healthpb.RegisterHealthServer(srv, hs)
	return srv, hs, nil
}
