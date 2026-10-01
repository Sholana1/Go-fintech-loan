package grpcx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

func recoverInterceptor(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				log.ErrorContext(ctx, "panic in gRPC handler", "method", info.FullMethod, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
				err = status.Error(codes.Internal, "internal error")
			}
		}()
		return handler(ctx, req)
	}
}

// callerInterceptor extracts the workload identity from the verified client
// certificate. Health checks are exempt so load balancers can probe.
func callerInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if strings.HasPrefix(info.FullMethod, "/grpc.health.v1.Health/") {
			return handler(ctx, req)
		}
		name, err := callerFromPeer(ctx)
		if err != nil {
			return nil, status.Error(codes.Unauthenticated, err.Error())
		}
		return handler(context.WithValue(ctx, callerKey{}, name), req)
	}
}

func callerFromPeer(ctx context.Context) (string, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return "", errors.New("no peer information")
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(tlsInfo.State.VerifiedChains) == 0 || len(tlsInfo.State.VerifiedChains[0]) == 0 {
		return "", errors.New("client certificate required")
	}
	leaf := tlsInfo.State.VerifiedChains[0][0]
	for _, u := range leaf.URIs {
		if u.Scheme == "spiffe" && u.Host == TrustDomain && strings.HasPrefix(u.Path, "/svc/") {
			if name := strings.TrimPrefix(u.Path, "/svc/"); name != "" && !strings.Contains(name, "/") {
				return name, nil
			}
		}
	}
	return "", errors.New("client certificate carries no workload identity")
}

func limitInterceptor(slots chan struct{}) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
			return handler(ctx, req)
		default:
			// Shed load immediately; the caller's retry policy and
			// idempotency keys make this safe.
			return nil, status.Error(codes.ResourceExhausted, "server at concurrency limit")
		}
	}
}

func logInterceptor(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		code := status.Code(err)
		attrs := []any{"method", info.FullMethod, "caller", Caller(ctx), "code", code.String(), "duration_ms", time.Since(start).Milliseconds()}
		switch code {
		case codes.OK:
			log.DebugContext(ctx, "grpc call", attrs...)
		case codes.Internal, codes.Unknown, codes.DataLoss, codes.Unavailable:
			log.ErrorContext(ctx, "grpc call failed", append(attrs, "error", err.Error())...)
		default:
			log.InfoContext(ctx, "grpc call rejected", append(attrs, "error", err.Error())...)
		}
		return resp, err
	}
}
