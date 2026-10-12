package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
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
