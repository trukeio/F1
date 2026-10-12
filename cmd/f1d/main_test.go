package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trukeio/f1/internal/policy"
	"github.com/trukeio/f1/internal/repo"
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
