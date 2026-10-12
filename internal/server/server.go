// Package server runs the f1d HTTP listeners.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/trukeio/f1/internal/config"
	"github.com/trukeio/f1/internal/repo"
	"github.com/trukeio/f1/internal/serverdb"
)

// Names inside the data directory.
const (
	ServerDB = "server.db"
	ReposDir = "repos"
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
	if err := os.MkdirAll(filepath.Join(cfg.Server.DataDir, ReposDir), 0o700); err != nil {
		return fmt.Errorf("data directory: %w", err)
	}
	// The admin socket also guards the data directory against a second
	// server, so it is created before anything else is opened.
	adminLn, err := listenAdmin(filepath.Join(cfg.Server.DataDir, AdminSocket))
	if err != nil {
		return err
	}
	defer os.Remove(filepath.Join(cfg.Server.DataDir, AdminSocket))
	defer adminLn.Close()

	db, err := serverdb.Open(ctx, filepath.Join(cfg.Server.DataDir, ServerDB))
	if err != nil {
		return err
	}
	defer db.Close()

	mgr, err := OpenManager(ctx, cfg.Server.DataDir, db, log, repo.Options{})
	if err != nil {
		return err
	}
	defer func() {
		// Graceful shutdown seals open packs.
		if err := mgr.Close(context.Background()); err != nil {
			log.Error("closing repositories", "err", err)
		}
	}()

	ln, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.Server.Listen, err)
	}

	srv := &http.Server{
		Handler:           newMux(),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	admin := &http.Server{
		Handler:           adminHandler(mgr, log),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	errc := make(chan error, 2)
	go func() { errc <- srv.Serve(ln) }()
	go func() { errc <- admin.Serve(adminLn) }()

	log.Info("server started", "addr", ln.Addr().String(), "data_dir", cfg.Server.DataDir, "repositories", len(mgr.List()))
	if ready != nil {
		ready(ln.Addr())
	}

	var serveErr error
	select {
	case serveErr = <-errc:
	case <-ctx.Done():
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
	defer cancel()
	err = errors.Join(srv.Shutdown(shutdownCtx), admin.Shutdown(shutdownCtx))
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", serveErr)
	}
	if err != nil {
		return fmt.Errorf("shutdown: %w", err)
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
