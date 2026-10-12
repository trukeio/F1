package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "f1d.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadDefaults(t *testing.T) {
	path := writeConfig(t, "")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(filepath.Dir(path), "data"); cfg.Server.DataDir != want {
		t.Errorf("DataDir = %q, want %q", cfg.Server.DataDir, want)
	}
	if cfg.Server.Listen != "127.0.0.1:8080" {
		t.Errorf("Listen = %q", cfg.Server.Listen)
	}
	if cfg.Log.Level != "info" || cfg.Log.Format != "text" {
		t.Errorf("Log = %+v", cfg.Log)
	}
}

func TestLoadValues(t *testing.T) {
	path := writeConfig(t, `
[server]
data_dir = "/var/lib/f1"
listen = ":9999"

[log]
level = "debug"
format = "json"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Server: Server{DataDir: "/var/lib/f1", Listen: ":9999"},
		Log:    Log{Level: "debug", Format: "json"},
	}
	if cfg != want {
		t.Errorf("got %+v, want %+v", cfg, want)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{"syntax", "[server", "config"},
		{"unknown key", "[server]\nlisten_addr = \":1\"", "unknown keys: server.listen_addr"},
		{"unknown section", "[s3]\nlisten = \":1\"", "unknown keys: s3, s3.listen"},
		{"bad listen", "[server]\nlisten = \"localhost\"", "server.listen"},
		{"bad level", "[log]\nlevel = \"verbose\"", "log.level"},
		{"bad format", "[log]\nformat = \"xml\"", "log.format"},
		{"empty data dir", "[server]\ndata_dir = \"\"", "server.data_dir"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tt.content))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "missing.toml")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestExampleConfig(t *testing.T) {
	if _, err := Load("../../example.toml"); err != nil {
		t.Fatal(err)
	}
}
