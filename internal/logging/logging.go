// Package logging sets up the process-wide slog logger.
package logging

import (
	"io"
	"log/slog"

	"github.com/trukeio/f1/internal/config"
)

// New returns a logger that writes to w as configured.
func New(w io.Writer, cfg config.Log) (*slog.Logger, error) {
	level, err := cfg.SlogLevel()
	if err != nil {
		return nil, err
	}
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if cfg.Format == "json" {
		h = slog.NewJSONHandler(w, opts)
	} else {
		h = slog.NewTextHandler(w, opts)
	}
	return slog.New(h), nil
}
