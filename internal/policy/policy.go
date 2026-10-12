// Package policy defines the namespace configuration and its validation.
package policy

import (
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
)

// Layout values.
const (
	LayoutTree = "tree"
	LayoutFlat = "flat"
)

// Config is a namespace configuration (the namespace keys of f1repo.toml).
type Config struct {
	Layout     string `toml:"layout"`
	Retention  string `toml:"retention"`
	Autocommit bool   `toml:"autocommit"`
	Branches   bool   `toml:"branches"`
	Locks      string `toml:"locks"`
	Git        bool   `toml:"git"`
	DefaultRef string `toml:"default_ref"`
}

// Default returns the defaults for keys absent from a configuration: the Git
// style preset without Git exposure.
func Default() Config {
	return Config{
		Layout:     LayoutTree,
		Retention:  "all",
		Branches:   true,
		Locks:      "exclusive",
		DefaultRef: "main",
	}
}

// Preset returns a named preset: "s3", "svn" or "git".
func Preset(name string) (Config, error) {
	c := Default()
	switch name {
	case "s3":
		c.Layout = LayoutFlat
		c.Retention = "none"
		c.Autocommit = true
		c.Branches = false
		c.Git = false
	case "svn":
		c.Locks = "required:*.sch,*.brd,*.docx"
	case "git":
		c.Git = true
	default:
		return Config{}, fmt.Errorf("policy: unknown preset %q", name)
	}
	return c, nil
}

// Retention is a parsed retention policy.
type Retention struct {
	Kind string // "all", "last", "days" or "none"
	N    int    // for "last" and "days"
}

// Truncating reports whether the policy drops history.
func (r Retention) Truncating() bool { return r.Kind != "all" }

// ParseRetention parses "all", "none", "last:N" or "days:N".
func ParseRetention(s string) (Retention, error) {
	switch s {
	case "all", "none":
		return Retention{Kind: s}, nil
	}
	kind, num, ok := strings.Cut(s, ":")
	if ok && (kind == "last" || kind == "days") {
		n, err := strconv.Atoi(num)
		if err == nil && n > 0 {
			return Retention{Kind: kind, N: n}, nil
		}
	}
	return Retention{}, fmt.Errorf("retention: %q is not one of all, none, last:N, days:N", s)
}

// LockMode is a parsed lock policy.
type LockMode struct {
	Mode     string   // "off", "exclusive" or "required"
	Required []string // glob patterns for "required"
}

// ParseLocks parses "off", "exclusive" or "required:<glob>,<glob>".
func ParseLocks(s string) (LockMode, error) {
	switch s {
	case "off", "exclusive":
		return LockMode{Mode: s}, nil
	}
	if globs, ok := strings.CutPrefix(s, "required:"); ok && globs != "" {
		m := LockMode{Mode: "required"}
		for _, g := range strings.Split(globs, ",") {
			g = strings.TrimSpace(g)
			if g == "" {
				return LockMode{}, fmt.Errorf("locks: empty glob in %q", s)
			}
			if _, err := path.Match(g, ""); err != nil {
				return LockMode{}, fmt.Errorf("locks: bad glob %q: %w", g, err)
			}
			m.Required = append(m.Required, g)
		}
		return m, nil
	}
	return LockMode{}, fmt.Errorf("locks: %q is not one of off, exclusive, required:<globs>", s)
}

// Validate checks the configuration, including D12 (Git namespaces need
// layout = tree and retention = all).
func (c Config) Validate() error {
	var errs []error
	if c.Layout != LayoutTree && c.Layout != LayoutFlat {
		errs = append(errs, fmt.Errorf("layout: %q is not one of tree, flat", c.Layout))
	}
	r, err := ParseRetention(c.Retention)
	if err != nil {
		errs = append(errs, err)
	}
	if _, err := ParseLocks(c.Locks); err != nil {
		errs = append(errs, err)
	}
	if err := ValidRefName("heads/" + c.DefaultRef); err != nil {
		errs = append(errs, fmt.Errorf("default_ref: %w", err))
	}
	if c.Git && (c.Layout != LayoutTree || (err == nil && r.Truncating())) {
		errs = append(errs, errors.New("git = true requires layout = tree and retention = all (D12)"))
	}
	return errors.Join(errs...)
}

// RetentionPolicy returns the parsed retention policy of a valid config.
func (c Config) RetentionPolicy() Retention {
	r, _ := ParseRetention(c.Retention)
	return r
}

// ValidRefName checks a full ref name such as "heads/main" or "tags/v1"
// against Git's ref name rules (git check-ref-format), restricted so that a
// name never contains whitespace.
func ValidRefName(name string) error {
	bad := func(why string) error { return fmt.Errorf("invalid ref name %q: %s", name, why) }
	if name == "" || name == "@" {
		return bad("empty")
	}
	if !strings.Contains(name, "/") {
		return bad("must have a category such as heads/ or tags/")
	}
	if strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.Contains(name, "//") {
		return bad("empty component")
	}
	if strings.HasSuffix(name, ".") || strings.Contains(name, "..") || strings.Contains(name, "@{") {
		return bad(`contains "..", "@{" or ends with "."`)
	}
	for _, comp := range strings.Split(name, "/") {
		if strings.HasPrefix(comp, ".") || strings.HasSuffix(comp, ".lock") {
			return bad(`component starts with "." or ends with ".lock"`)
		}
	}
	for _, r := range name {
		if r <= ' ' || r == 0x7f || strings.ContainsRune(`~^:?*[\`, r) {
			return bad("contains a forbidden character")
		}
	}
	return nil
}

// refCategories are the ref name prefixes that FullRefName leaves alone.
var refCategories = []string{"heads/", "tags/", "remotes/", "notes/"}

// FullRefName expands a short branch name: "main" → "heads/main",
// "feature/x" → "heads/feature/x". Names that start with a category
// ("heads/", "tags/", "remotes/", "notes/") are returned unchanged.
func FullRefName(name string) string {
	for _, c := range refCategories {
		if strings.HasPrefix(name, c) {
			return name
		}
	}
	return "heads/" + name
}
