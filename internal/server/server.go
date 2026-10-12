// Package server runs the f1d HTTP listeners.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/trukeio/f1/internal/config"
)

// ShutdownTimeout bounds how long Run waits for in-flight requests after
// its context is cancelled.
const ShutdownTimeout = 30 * time.Second

// Run serves until ctx is cancelled, then shuts down gracefully. If ready is
// not nil, it is called with the bound listener address once the server
// accepts connections.
func Run(ctx context.Context, cfg config.Config, log *slog.Logger, ready func(addr net.Addr)) error {
	if err := cfg.EnsureDataDir(); err != nil {
		return err
	}

	ln, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.Server.Listen, err)
	}

	srv := &http.Server{
		Handler:           newMux(),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	log.Info("server started", "addr", ln.Addr().String(), "data_dir", cfg.Server.DataDir)
	if ready != nil {
		ready(ln.Addr())
	}

	select {
	case err := <-errc:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	log.Info("server stopped")
	return nil
}

func newMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}
