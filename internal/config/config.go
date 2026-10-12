// Package config loads and validates the f1d server configuration (TOML, D14).
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is the f1d server configuration.
type Config struct {
	Server Server `toml:"server"`
	Log    Log    `toml:"log"`
}

// Server holds listener and storage settings.
type Server struct {
	// DataDir holds server.db and repos/. A relative path is resolved
	// against the directory of the configuration file.
	DataDir string `toml:"data_dir"`
	// Listen is the address of the main listener (native API, Git).
	Listen string `toml:"listen"`
}

// Log holds logging settings.
type Log struct {
	Level  string `toml:"level"`  // debug, info, warn, error
	Format string `toml:"format"` // text, json
}

// Default returns the configuration used for keys absent from the file.
func Default() Config {
	return Config{
		Server: Server{
			DataDir: "data",
			Listen:  "127.0.0.1:8080",
		},
		Log: Log{
			Level:  "info",
			Format: "text",
		},
	}
}

// Load reads the configuration file at path, applies defaults for missing
// keys, resolves relative paths and validates the result. Unknown keys are
// an error, so that typos do not silently fall back to defaults.
func Load(path string) (Config, error) {
	cfg := Default()
	md, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return Config{}, fmt.Errorf("config %s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		sort.Strings(keys)
		return Config{}, fmt.Errorf("config %s: unknown keys: %s", path, strings.Join(keys, ", "))
	}
	if cfg.Server.DataDir != "" && !filepath.IsAbs(cfg.Server.DataDir) {
		cfg.Server.DataDir = filepath.Join(filepath.Dir(path), cfg.Server.DataDir)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

// Validate checks the configuration for errors.
func (c Config) Validate() error {
	var errs []error
	if c.Server.DataDir == "" {
		errs = append(errs, errors.New("server.data_dir must not be empty"))
	}
	if _, _, err := net.SplitHostPort(c.Server.Listen); err != nil {
		errs = append(errs, fmt.Errorf("server.listen: %w", err))
	}
	if _, err := c.Log.SlogLevel(); err != nil {
		errs = append(errs, err)
	}
	switch c.Log.Format {
	case "text", "json":
	default:
		errs = append(errs, fmt.Errorf("log.format: %q is not one of text, json", c.Log.Format))
	}
	return errors.Join(errs...)
}

// SlogLevel returns the configured log level.
func (l Log) SlogLevel() (slog.Level, error) {
	switch l.Level {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("log.level: %q is not one of debug, info, warn, error", l.Level)
}

// EnsureDataDir creates the data directory if it does not exist.
func (c Config) EnsureDataDir() error {
	if err := os.MkdirAll(c.Server.DataDir, 0o700); err != nil {
		return fmt.Errorf("data directory: %w", err)
	}
	return nil
}
