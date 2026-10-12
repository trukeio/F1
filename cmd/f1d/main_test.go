package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trukeio/f1/internal/policy"
	"github.com/trukeio/f1/internal/repo"
	"github.com/trukeio/f1/internal/server"
)

func TestRunUsage(t *testing.T) {
	tests := []struct {
		args     []string
		wantCode int
		wantOut  string
		wantErr  string
	}{
		{nil, 2, "", "Usage: f1d"},
		{[]string{"help"}, 0, "Usage: f1d", ""},
		{[]string{"version"}, 0, "f1d dev", ""},
		{[]string{"bogus"}, 2, "", `unknown command "bogus"`},
		{[]string{"serve", "extra"}, 2, "", "unexpected arguments"},
		{[]string{"serve", "--config", "/nonexistent/f1d.toml"}, 1, "", "f1d serve: config"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), tt.args, &stdout, &stderr)
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d (stderr: %s)", code, tt.wantCode, stderr.String())
			}
			if !strings.Contains(stdout.String(), tt.wantOut) {
				t.Errorf("stdout = %q, want containing %q", stdout.String(), tt.wantOut)
			}
			if !strings.Contains(stderr.String(), tt.wantErr) {
				t.Errorf("stderr = %q, want containing %q", stderr.String(), tt.wantErr)
			}
		})
	}
}

func TestFsckCommand(t *testing.T) {
	data := t.TempDir()
	if err := repo.Create(filepath.Join(data, "repos", "ns"), policy.Default()); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(data, "f1d.toml")
	if err := os.WriteFile(cfg, []byte("[server]\ndata_dir = \".\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"fsck", "--dir", filepath.Join(data, "repos", "ns")},
		{"fsck", "--config", cfg, "ns"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), args, &stdout, &stderr); code != 0 {
			t.Fatalf("%v: exit %d: %s%s", args, code, stdout.String(), stderr.String())
		}
		if !strings.Contains(stdout.String(), "fsck: ok") {
			t.Fatalf("%v: output %q", args, stdout.String())
		}
	}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"fsck", "--dir", filepath.Join(data, "missing")}, &stdout, &stderr); code != 1 {
		t.Fatalf("fsck of a missing directory: exit %d", code)
	}
	if code := run(context.Background(), []string{"fsck"}, &stdout, &stderr); code != 2 {
		t.Fatalf("fsck without arguments: exit %d", code)
	}
}

func runOK(t *testing.T, args ...string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), args, &stdout, &stderr); code != 0 {
		t.Fatalf("f1d %v: exit %d: %s%s", args, code, stdout.String(), stderr.String())
	}
	return stdout.String()
}

func runFail(t *testing.T, want string, args ...string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, &stdout, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), want) {
		t.Fatalf("f1d %v: exit %d, stderr %q, want failure containing %q", args, code, stderr.String(), want)
	}
}

func TestRepoCommands(t *testing.T) {
	data := t.TempDir()
	cfg := filepath.Join(data, "f1d.toml")
	if err := os.WriteFile(cfg, []byte("[server]\ndata_dir = \".\"\nlisten = \"127.0.0.1:0\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := repo.Create(filepath.Join(data, "repos", "ns"), policy.Default()); err != nil {
		t.Fatal(err)
	}
	zipPath := filepath.Join(t.TempDir(), "ns.zip")

	// Offline: no server is running.
	runOK(t, "repo", "attach", "--config", cfg, "ns")
	if out := runOK(t, "repo", "list", "--config", cfg); !strings.Contains(out, "ns\t") {
		t.Fatalf("list: %q", out)
	}
	runOK(t, "repo", "archive", "--config", cfg, "ns", zipPath)
	runOK(t, "repo", "detach", "--config", cfg, "ns")
	runOK(t, "repo", "import", "--config", cfg, "--name", "ns2", "--new-uuid", zipPath)
	runOK(t, "repo", "rebuild-cache", "--config", cfg, "ns2")
	runFail(t, "no server is running", "repo", "freeze", "--config", cfg, "ns2")
	runFail(t, "already exists", "repo", "import", "--config", cfg, "--name", "ns2", zipPath)

	// Online: the same commands go through the admin socket.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- run(ctx, []string{"serve", "--config", cfg}, io.Discard, io.Discard) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, ok := server.DialAdmin(data); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	runOK(t, "repo", "freeze", "--config", cfg, "ns2")
	if out := runOK(t, "repo", "list", "--config", cfg); !strings.Contains(out, "(frozen)") {
		t.Fatalf("list while frozen: %q", out)
	}
	runOK(t, "repo", "unfreeze", "--config", cfg, "ns2")
	runOK(t, "repo", "archive", "--config", cfg, "ns2", filepath.Join(t.TempDir(), "online.zip"))
	runFail(t, "in use", "repo", "rebuild-cache", "--config", cfg, "ns2")
	runFail(t, "in use", "fsck", "--config", cfg, "ns2")
	runOK(t, "repo", "import", "--config", cfg, "--name", "ns3", zipPath)
	if out := runOK(t, "repo", "list", "--config", cfg); !strings.Contains(out, "ns3") {
		t.Fatalf("imported repository not attached on the running server: %q", out)
	}
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("serve exited %d", code)
	}
}
