// Package web is f1web, the web viewer of an F1 installation (ADR-0018).
// It is a client of the native API: every call carries the signed-in
// user's token, so F1 applies the user's ACLs.
package web

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the f1web configuration (TOML, D14).
type Config struct {
	// Listen is the address of the web listener. Default "127.0.0.1:8090".
	Listen string `toml:"listen"`
	// PublicURL is the URL browsers use to reach f1web, without a
	// trailing slash, for example "https://f1.example.com". The OIDC
	// redirect URI is PublicURL + "/auth/callback". An https URL makes
	// the session cookie Secure.
	PublicURL string `toml:"public_url"`
	// F1URL is the URL of the F1 server's main listener.
	F1URL string `toml:"f1_url"`
	// SessionIdle signs a user out after this long without a request.
	// Default "8h".
	SessionIdle string `toml:"session_idle"`
	// TokenLogin lets users sign in by pasting an F1 token, as local
	// users must. Default true.
	TokenLogin bool `toml:"token_login"`
	// OIDC configures sign-in through the identity provider; nil disables
	// it.
	OIDC *OIDCConfig `toml:"oidc"`
	Log  Log         `toml:"log"`
}

// OIDCConfig is f1web's client registration at the OIDC provider that F1
// trusts (ADR-0005). Its access tokens must carry F1's audience.
type OIDCConfig struct {
	// Name labels the sign-in button. Default "single sign-on".
	Name string `toml:"name"`
	// Issuer is the provider's issuer URL; endpoints are discovered.
	Issuer       string `toml:"issuer"`
	ClientID     string `toml:"client_id"`
	ClientSecret string `toml:"client_secret"`
	// Scopes are requested at sign-in. Default ["openid"]. Entra ID needs
	// the F1 API scope (for example "api://f1/.default") and
	// "offline_access" for refresh tokens.
	Scopes []string `toml:"scopes"`
}

// Log configures logging.
type Log struct {
	// Level is "debug", "info", "warn" or "error". Default "info".
	Level string `toml:"level"`
	// Format is "text" or "json". Default "text".
	Format string `toml:"format"`
}

// DefaultConfig returns the defaults.
func DefaultConfig() Config {
	return Config{Listen: "127.0.0.1:8090", SessionIdle: "8h", TokenLogin: true,
		Log: Log{Level: "info", Format: "text"}}
}

// LoadConfig reads and validates a configuration file.
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	md, err := toml.Decode(string(data), &cfg)
	if err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	if u := md.Undecoded(); len(u) > 0 {
		return cfg, fmt.Errorf("%s: unknown setting %s", path, u[0])
	}
	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Validate checks the configuration and fills in derived defaults.
func (c *Config) Validate() error {
	c.PublicURL = strings.TrimSuffix(c.PublicURL, "/")
	c.F1URL = strings.TrimSuffix(c.F1URL, "/")
	if err := checkURL("public_url", c.PublicURL); err != nil {
		return err
	}
	if err := checkURL("f1_url", c.F1URL); err != nil {
		return err
	}
	if _, err := c.idle(); err != nil {
		return err
	}
	if _, err := c.Log.slogLevel(); err != nil {
		return err
	}
	if c.Log.Format != "text" && c.Log.Format != "json" {
		return fmt.Errorf("log.format must be text or json, not %q", c.Log.Format)
	}
	if o := c.OIDC; o != nil {
		if err := checkURL("oidc.issuer", o.Issuer); err != nil {
			return err
		}
		if o.ClientID == "" {
			return errors.New("oidc.client_id is required")
		}
		if o.Name == "" {
			o.Name = "single sign-on"
		}
		if len(o.Scopes) == 0 {
			o.Scopes = []string{"openid"}
		}
	}
	if c.OIDC == nil && !c.TokenLogin {
		return errors.New("no way to sign in: configure [oidc] or set token_login = true")
	}
	return nil
}

func (c *Config) idle() (time.Duration, error) {
	d, err := time.ParseDuration(c.SessionIdle)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("session_idle: invalid duration %q", c.SessionIdle)
	}
	return d, nil
}

// secure reports whether cookies get the Secure attribute.
func (c *Config) secure() bool { return strings.HasPrefix(c.PublicURL, "https://") }

func checkURL(name, s string) error {
	if s == "" {
		return fmt.Errorf("%s is required", name)
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%s: not an http(s) URL: %q", name, s)
	}
	return nil
}

func (l Log) slogLevel() (slog.Level, error) {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(l.Level)); err != nil {
		return 0, fmt.Errorf("log.level: %w", err)
	}
	return lv, nil
}

// NewLogger returns a logger that writes to w as configured.
func NewLogger(w io.Writer, l Log) *slog.Logger {
	lv, _ := l.slogLevel()
	opts := &slog.HandlerOptions{Level: lv}
	if l.Format == "json" {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}
