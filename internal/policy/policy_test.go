package policy

import "testing"

func TestPresetsValid(t *testing.T) {
	for _, name := range []string{"s3", "svn", "git"} {
		c, err := Preset(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Validate(); err != nil {
			t.Errorf("preset %s: %v", name, err)
		}
	}
	if _, err := Preset("ftp"); err == nil {
		t.Error("unknown preset accepted")
	}
}

func TestValidate(t *testing.T) {
	bad := []func(*Config){
		func(c *Config) { c.Layout = "deep" },
		func(c *Config) { c.Retention = "last:0" },
		func(c *Config) { c.Retention = "forever" },
		func(c *Config) { c.Locks = "required:" },
		func(c *Config) { c.Locks = "required:[" },
		func(c *Config) { c.DefaultRef = "a..b" },
		func(c *Config) { c.Git = true; c.Layout = LayoutFlat },   // D12
		func(c *Config) { c.Git = true; c.Retention = "days:30" }, // D12
	}
	for i, mutate := range bad {
		c := Default()
		mutate(&c)
		if c.Validate() == nil {
			t.Errorf("case %d accepted: %+v", i, c)
		}
	}
	c := Default()
	c.Retention, c.Locks = "last:5", "required:*.psd, *.brd"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if r := c.RetentionPolicy(); r.Kind != "last" || r.N != 5 || !r.Truncating() {
		t.Fatalf("retention = %+v", r)
	}
	m, _ := ParseLocks(c.Locks)
	if len(m.Required) != 2 || m.Required[1] != "*.brd" {
		t.Fatalf("locks = %+v", m)
	}
}

func TestRefNames(t *testing.T) {
	for _, ok := range []string{"heads/main", "heads/feature/x", "tags/v1.0", "notes/commits"} {
		if err := ValidRefName(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "main", "heads/", "/heads/x", "heads//x", "heads/x.lock", "heads/.x", "heads/a..b", "heads/a b", "heads/a~1", "heads/a:b", "heads/x.", "heads/a@{1}"} {
		if ValidRefName(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for in, want := range map[string]string{"main": "heads/main", "feature/x": "heads/feature/x", "tags/v1": "tags/v1", "heads/dev": "heads/dev"} {
		if got := FullRefName(in); got != want {
			t.Errorf("FullRefName(%q) = %q, want %q", in, got, want)
		}
	}
}
