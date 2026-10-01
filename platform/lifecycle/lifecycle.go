// Package lifecycle runs a service's long-lived tasks (servers, relays,
// workers) under one cancellable context and shuts them down together.
//
// Resource ownership is explicit: main builds every dependency, hands each
// task exactly what it needs, and this package guarantees that when one task
// fails or the process is asked to stop, every task is told to stop and is
// waited for (up to a bound) before the process exits.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
)

// Task is a named long-running function. It must return promptly once its
// context is cancelled.
type Task struct {
	Name string
	Run  func(ctx context.Context) error
}

// Run starts all tasks and blocks until SIGINT/SIGTERM, parent cancellation,
// or the first task error. It then cancels every task and waits up to
// shutdownTimeout for them to return.
func Run(parent context.Context, log *slog.Logger, shutdownTimeout time.Duration, tasks ...Task) error {
	ctx, stop := signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	for _, task := range tasks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := task.Run(ctx)
			if err != nil && !errors.Is(err, context.Canceled) {
				log.Error("task stopped with error", "task", task.Name, "error", err.Error())
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("%s: %w", task.Name, err)
				}
				mu.Unlock()
				cancel() // one task failing stops the service; the orchestrator restarts it
			}
		}()
	}

	<-ctx.Done()
	log.Info("shutting down", "timeout", shutdownTimeout.String())

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(shutdownTimeout):
		log.Error("shutdown timed out; exiting with tasks still running")
		mu.Lock()
		if firstErr == nil {
			firstErr = errors.New("shutdown timed out")
		}
		mu.Unlock()
	}
	mu.Lock()
	defer mu.Unlock()
	return firstErr
}

// GRPCServer returns a task that serves srv on addr and stops gracefully:
// in-flight calls finish, new ones are refused, and after grace the server
// is stopped hard.
func GRPCServer(name, addr string, srv *grpc.Server, grace time.Duration) Task {
	return Task{Name: name, Run: func(ctx context.Context) error {
		lis, err := net.Listen("tcp", addr)
		if err != nil {
			return err
		}
		errc := make(chan error, 1)
		go func() { errc <- srv.Serve(lis) }()
		select {
		case err := <-errc:
			return err
		case <-ctx.Done():
			stopped := make(chan struct{})
			go func() { srv.GracefulStop(); close(stopped) }()
			select {
			case <-stopped:
			case <-time.After(grace):
				srv.Stop()
			}
			return nil
		}
	}}
}

// HTTPServer returns a task that serves handler on addr with conservative
// timeouts and drains in-flight requests on shutdown.
func HTTPServer(name, addr string, handler http.Handler, grace time.Duration) Task {
	return Task{Name: name, Run: func(ctx context.Context) error {
		srv := &http.Server{
			Addr:              addr,
			Handler:           handler,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       15 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       60 * time.Second,
			MaxHeaderBytes:    16 << 10,
		}
		errc := make(chan error, 1)
		go func() { errc <- srv.ListenAndServe() }()
		select {
		case err := <-errc:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
			defer cancel()
			return srv.Shutdown(shutdownCtx)
		}
	}}
}

// Every returns a task that calls fn immediately and then at each interval
// until cancelled. Errors are logged, not fatal: a failed tick is retried at
// the next one.
func Every(name string, interval time.Duration, log *slog.Logger, fn func(ctx context.Context) error) Task {
	return Task{Name: name, Run: func(ctx context.Context) error {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			if err := fn(ctx); err != nil && ctx.Err() == nil {
				log.ErrorContext(ctx, "periodic task failed", "task", name, "error", err.Error())
			}
			select {
			case <-ctx.Done():
				return nil
			case <-t.C:
			}
		}
	}}
}

// AdminMux returns the operational endpoints every service exposes on its
// admin port: Prometheus metrics, liveness, and readiness (which runs the
// given checks, for example a database ping).
func AdminMux(metrics http.Handler, ready func(ctx context.Context) error) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", metrics)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := ready(ctx); err != nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return mux
}
