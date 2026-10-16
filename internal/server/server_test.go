package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/trukeio/f1/internal/config"
)

func TestRunStartsAndStops(t *testing.T) {
	cfg := config.Default()
	cfg.Server.DataDir = filepath.Join(t.TempDir(), "data")
	cfg.Server.Listen = "127.0.0.1:0"
	cfg.Server.S3.Listen = "127.0.0.1:0"
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	ctx, cancel := context.WithCancel(context.Background())
	addrc := make(chan net.Addr, 1)
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, log, func(a Addrs) { addrc <- a.Main }) }()

	var addr net.Addr
	select {
	case addr = <-addrc:
	case err := <-done:
		t.Fatalf("Run returned early: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("server did not become ready")
	}

	resp, err := http.Get("http://" + addr.String() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok\n" {
		t.Fatalf("healthz: %d %q", resp.StatusCode, body)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("server did not stop")
	}
	for _, name := range []string{ServerDB, ReposDir} {
		if _, err := os.Stat(filepath.Join(cfg.Server.DataDir, name)); err != nil {
			t.Errorf("%s not created: %v", name, err)
		}
	}
}

func TestRunListenError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	cfg := config.Default()
	cfg.Server.DataDir = t.TempDir()
	cfg.Server.Listen = ln.Addr().String()
	cfg.Server.S3.Listen = ""
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := Run(context.Background(), cfg, log, nil); err == nil {
		t.Fatal("expected error when the address is in use")
	}
}

// A data directory whose admin socket path is longer than a socket address
// holds still works: the server binds, admin commands reach it, a second
// server is refused, and the socket is removed on exit.
func TestLongDataDirPath(t *testing.T) {
	dir := t.TempDir()
	for len(dir) < 150 {
		dir = filepath.Join(dir, "a-rather-long-directory-name")
	}
	cfg := config.Default()
	cfg.Server.DataDir = dir
	cfg.Server.Listen = "127.0.0.1:0"
	cfg.Server.S3.Listen = ""
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, log, func(Addrs) { ready <- struct{}{} }) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("Run: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("server did not start")
	}
	sock := filepath.Join(dir, AdminSocket)
	if fi, err := os.Stat(sock); err != nil || fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o600 {
		t.Fatalf("admin socket: %v, %v", fi, err)
	}
	ac, ok := DialAdmin(dir)
	if !ok {
		t.Fatal("DialAdmin failed")
	}
	if _, err := ac.List(ctx); err != nil {
		t.Fatalf("admin call: %v", err)
	}
	if err := Run(ctx, cfg, log, nil); !errors.Is(err, ErrServerRunning) {
		t.Fatalf("second server: %v", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatalf("socket left behind: %v", err)
	}
	if _, ok := DialAdmin(dir); ok {
		t.Fatal("DialAdmin succeeded with no server")
	}
}
