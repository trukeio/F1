package server

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/trukeio/f1/internal/config"
)

func TestRunStartsAndStops(t *testing.T) {
	cfg := config.Default()
	cfg.Server.DataDir = filepath.Join(t.TempDir(), "data")
	cfg.Server.Listen = "127.0.0.1:0"
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	ctx, cancel := context.WithCancel(context.Background())
	addrc := make(chan net.Addr, 1)
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, log, func(a net.Addr) { addrc <- a }) }()

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
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := Run(context.Background(), cfg, log, nil); err == nil {
		t.Fatal("expected error when the address is in use")
	}
}
