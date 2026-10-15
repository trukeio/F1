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
	"time"

	"github.com/BurntSushi/toml"

	"github.com/trukeio/f1/internal/auth"
)

// Config is the f1d server configuration.
type Config struct {
	Server Server `toml:"server"`
	Log    Log    `toml:"log"`
	Auth   Auth   `toml:"auth"`
}

// Auth configures authentication (D17, ADR-0005).
type Auth struct {
	// OIDC lists the trusted OIDC providers (normally one: the enterprise
	// identity provider that G1 also trusts).
	OIDC []auth.OIDCConfig `toml:"oidc"`
	// S3GroupTTL is how long an external principal's groups, recorded at its
	// last token use, apply to its S3 key requests. Default "24h".
	S3GroupTTL string `toml:"s3_group_ttl"`
}

// GroupTTL returns the parsed S3GroupTTL.
func (a Auth) GroupTTL() time.Duration {
	d, _ := time.ParseDuration(a.S3GroupTTL)
	return d
}

// Server holds listener and storage settings.
type Server struct {
	// DataDir holds server.db and repos/. A relative path is resolved
	// against the directory of the configuration file.
	DataDir string `toml:"data_dir"`
	// Listen is the address of the main listener (native API, Git).
	Listen string `toml:"listen"`
	// S3 configures the S3 API listener.
	S3 S3 `toml:"s3"`
	// GC configures garbage collection.
	GC GC `toml:"gc"`
}

// GC configures garbage collection (doc/plan.md, "Retention and garbage
// collection").
type GC struct {
	// Interval between collections of every repository; "0" disables
	// them (f1d gc still runs one). Default "24h".
	Interval string `toml:"interval"`
	// Grace protects objects written this recently even if unreachable
	// (uploads not yet committed). Default "24h".
	Grace string `toml:"grace"`
}

// IntervalDuration returns the parsed Interval.
func (g GC) IntervalDuration() time.Duration {
	d, _ := time.ParseDuration(g.Interval)
	return d
}

// GraceDuration returns the parsed Grace.
func (g GC) GraceDuration() time.Duration {
	d, _ := time.ParseDuration(g.Grace)
	return d
}

// S3 configures the S3 compatibility API (doc/plan.md, "S3 compatibility
// API").
type S3 struct {
	// Listen is the address of the S3 listener; empty disables the S3 API.
	Listen string `toml:"listen"`
	// Domain enables virtual-host addressing: <bucket>.<domain>.
	Domain string `toml:"domain"`
	// Region is the region clients sign for, and GetBucketLocation's answer.
	Region string `toml:"region"`
	// CreateBucket allows server administrators to create and delete
	// namespaces with CreateBucket and DeleteBucket.
	CreateBucket bool `toml:"create_bucket"`
	// UploadExpiry is how long an open multipart upload is kept. Default
	// "168h" (7 days).
	UploadExpiry string `toml:"upload_expiry"`
	// Aliases expose a namespace and ref under another bucket name, for
	// example a branch.
	Aliases []S3Alias `toml:"alias"`
}

// S3Alias maps a bucket name to a namespace and ref.
type S3Alias struct {
	Bucket    string `toml:"bucket"`
	Namespace string `toml:"namespace"`
	Ref       string `toml:"ref"`
}

// Expiry returns the parsed UploadExpiry.
func (s S3) Expiry() time.Duration {
	d, _ := time.ParseDuration(s.UploadExpiry)
	return d
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
			S3: S3{
				Listen:       "127.0.0.1:9000",
				Region:       "us-east-1",
				CreateBucket: true,
				UploadExpiry: "168h",
			},
			GC: GC{Interval: "24h", Grace: "24h"},
		},
		Log: Log{
			Level:  "info",
			Format: "text",
		},
		Auth: Auth{S3GroupTTL: "24h"},
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
	if c.Server.S3.Listen != "" {
		if _, _, err := net.SplitHostPort(c.Server.S3.Listen); err != nil {
			errs = append(errs, fmt.Errorf("server.s3.listen: %w", err))
		}
	}
	if c.Server.S3.Region == "" {
		errs = append(errs, errors.New("server.s3.region must not be empty"))
	}
	if d, err := time.ParseDuration(c.Server.S3.UploadExpiry); err != nil || d <= 0 {
		errs = append(errs, fmt.Errorf("server.s3.upload_expiry: %q is not a positive duration", c.Server.S3.UploadExpiry))
	}
	if d, err := time.ParseDuration(c.Server.GC.Interval); err != nil || d < 0 {
		errs = append(errs, fmt.Errorf("server.gc.interval: %q is not a duration (0 disables)", c.Server.GC.Interval))
	}
	if d, err := time.ParseDuration(c.Server.GC.Grace); err != nil || d < 0 {
		errs = append(errs, fmt.Errorf("server.gc.grace: %q is not a duration", c.Server.GC.Grace))
	}
	buckets := map[string]bool{}
	for _, a := range c.Server.S3.Aliases {
		if a.Bucket == "" || a.Namespace == "" {
			errs = append(errs, errors.New("server.s3.alias: bucket and namespace are required"))
		}
		if buckets[a.Bucket] {
			errs = append(errs, fmt.Errorf("server.s3.alias: duplicate bucket %q", a.Bucket))
		}
		buckets[a.Bucket] = true
	}
	if _, err := c.Log.SlogLevel(); err != nil {
		errs = append(errs, err)
	}
	switch c.Log.Format {
	case "text", "json":
	default:
		errs = append(errs, fmt.Errorf("log.format: %q is not one of text, json", c.Log.Format))
	}
	if d, err := time.ParseDuration(c.Auth.S3GroupTTL); err != nil || d <= 0 {
		errs = append(errs, fmt.Errorf("auth.s3_group_ttl: %q is not a positive duration", c.Auth.S3GroupTTL))
	}
	names := map[string]bool{}
	for i := range c.Auth.OIDC {
		o := c.Auth.OIDC[i]
		if err := o.Validate(); err != nil {
			errs = append(errs, err)
		}
		if names[o.Name] {
			errs = append(errs, fmt.Errorf("auth.oidc: duplicate name %q", o.Name))
		}
		names[o.Name] = true
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
