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

	"github.com/trukeio/f1/internal/api/git"
	"github.com/trukeio/f1/internal/api/native"
	"github.com/trukeio/f1/internal/api/s3"
	"github.com/trukeio/f1/internal/auth"
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

// Addrs are the bound listener addresses of a running server.
type Addrs struct {
	Main net.Addr // native API (and later Git)
	S3   net.Addr // nil if the S3 API is disabled
}

// Run serves until ctx is cancelled, then shuts down gracefully. If ready is
// not nil, it is called with the bound listener addresses once the server
// accepts connections.
func Run(ctx context.Context, cfg config.Config, log *slog.Logger, ready func(Addrs)) error {
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
	var s3ln net.Listener
	if cfg.Server.S3.Listen != "" {
		if s3ln, err = net.Listen("tcp", cfg.Server.S3.Listen); err != nil {
			ln.Close()
			return fmt.Errorf("listen %s (S3): %w", cfg.Server.S3.Listen, err)
		}
	}

	chain := &auth.Chain{Verifiers: []auth.Verifier{&auth.LocalTokens{DB: db}}, Log: log}
	for _, oc := range cfg.Auth.OIDC {
		v, err := auth.NewOIDC(oc, db, nil)
		if err != nil {
			return err
		}
		chain.Verifiers = append(chain.Verifiers, v)
	}
	acl := &auth.Authorizer{DB: db}
	nativeAPI := &native.Server{Repos: mgr, Auth: chain, ACL: acl, DB: db, Log: log}

	newServer := func(h http.Handler) *http.Server {
		return &http.Server{
			Handler:           h,
			ReadHeaderTimeout: 10 * time.Second,
			ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		}
	}
	gitAPI := &git.Server{Repos: mgr, Auth: chain, ACL: acl, Log: log}
	srv := newServer(newMux(nativeAPI.Handler(), gitAPI))
	admin := newServer(adminHandler(mgr, log))
	servers := []*http.Server{srv, admin}

	errc := make(chan error, 3)
	go func() { errc <- srv.Serve(ln) }()
	go func() { errc <- admin.Serve(adminLn) }()
	addrs := Addrs{Main: ln.Addr()}

	bgCtx, stopBackground := context.WithCancel(ctx)
	defer stopBackground()
	if s3ln != nil {
		s3cfg := cfg.Server.S3
		aliases := map[string]s3.Alias{}
		for _, a := range s3cfg.Aliases {
			aliases[a.Bucket] = s3.Alias{Namespace: a.Namespace, Ref: a.Ref}
		}
		s3API := &s3.Server{Repos: mgr, DB: db, ACL: acl, GroupTTL: cfg.Auth.GroupTTL(), Log: log, Config: s3.Config{
			Region: s3cfg.Region, Domain: s3cfg.Domain, CreateBucket: s3cfg.CreateBucket, UploadExpiry: s3cfg.Expiry(), Aliases: aliases,
		}}
		s3srv := newServer(s3API)
		servers = append(servers, s3srv)
		go func() { errc <- s3srv.Serve(s3ln) }()
		go s3API.Run(bgCtx)
		addrs.S3 = s3ln.Addr()
	}

	log.Info("server started", "addr", ln.Addr().String(), "s3_addr", addrString(addrs.S3), "data_dir", cfg.Server.DataDir, "repositories", len(mgr.List()))
	if ready != nil {
		ready(addrs)
	}

	var serveErr error
	select {
	case serveErr = <-errc:
	case <-ctx.Done():
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
	defer cancel()
	stopBackground()
	var errs []error
	for _, hs := range servers {
		errs = append(errs, hs.Shutdown(shutdownCtx))
	}
	err = errors.Join(errs...)
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", serveErr)
	}
	if err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	log.Info("server stopped")
	return nil
}

func addrString(a net.Addr) string {
	if a == nil {
		return ""
	}
	return a.String()
}

func newMux(nativeAPI, gitAPI http.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/api/v1/", nativeAPI)
	mux.Handle(git.Prefix, gitAPI)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}
