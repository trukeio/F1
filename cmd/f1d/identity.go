package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/trukeio/f1/internal/config"
	"github.com/trukeio/f1/internal/policy"
	"github.com/trukeio/f1/internal/repo"
	"github.com/trukeio/f1/internal/repodir"
	"github.com/trukeio/f1/internal/server"
	"github.com/trukeio/f1/internal/serverdb"
)

const identityUsage = `Usage:
  f1d user add <name> [--group <group>]...     create a local user
  f1d user list
  f1d user groups <name> [<group>...]          replace a local user's groups
  f1d user disable|enable <name>
  f1d token create <user> [--expires 720h] [--description <text>]
  f1d token list [<user>]
  f1d token revoke <id>
  f1d s3key create <user>
  f1d acl set <namespace|*> <principal> <prefix> <perms>   perms: read,write,lock,admin or "none"
  f1d acl rm <namespace|*> <principal> <prefix>
  f1d acl list <namespace|*>
  f1d ns create <name> [--preset s3|svn|git]   (f1d repo list lists namespaces)

Users and groups may be given as "alice" or "user:alice", "eng" or
"group:eng", except in acl commands, where a principal must be written
in full (user:<name> or group:<name>). Every command takes --config f1d.toml.
`

// multi is a repeatable string flag.
type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(s string) error { *m = append(*m, s); return nil }

func userName(s string) string {
	if strings.HasPrefix(s, "user:") {
		return s
	}
	return "user:" + s
}

func groupName(s string) string {
	if strings.HasPrefix(s, "group:") {
		return s
	}
	return "group:" + s
}

// principalName requires an explicit user: or group: prefix: in an ACL a
// bare name would be ambiguous.
func principalName(s string) (string, error) {
	if err := serverdb.ValidPrincipal(s); err != nil {
		return "", fmt.Errorf("%v (ACL principals are written user:<name> or group:<name>)", err)
	}
	return s, nil
}

// identityCmd runs user, token, s3key, acl and ns commands. They work on
// server.db directly, whether or not a server is running (SQLite allows
// concurrent access); "ns create" goes through a running server.
func identityCmd(ctx context.Context, group string, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, identityUsage)
		return 2
	}
	sub := args[0]
	fs := flag.NewFlagSet(group+" "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, identityUsage) }
	configPath := fs.String("config", "f1d.toml", "path to the configuration file")
	var groups multi
	fs.Var(&groups, "group", "user add: a group of the user (repeatable)")
	expires := fs.Duration("expires", 0, "token create: lifetime (0: no expiry)")
	description := fs.String("description", "", "token create: description")
	preset := fs.String("preset", "", "ns create: s3, svn or git")
	if err := parseFlags(fs, args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	a := fs.Args()
	fail := func(err error) int {
		fmt.Fprintf(stderr, "f1d %s %s: %v\n", group, sub, err)
		return 1
	}
	usage := func() int {
		fs.Usage()
		return 2
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return fail(err)
	}
	if err := os.MkdirAll(cfg.Server.DataDir, 0o700); err != nil {
		return fail(err)
	}

	if group == "ns" {
		return nsCmd(ctx, cfg, sub, a, *preset, stdout, fail, usage)
	}

	db, err := serverdb.Open(ctx, filepath.Join(cfg.Server.DataDir, server.ServerDB))
	if err != nil {
		return fail(err)
	}
	defer db.Close()

	switch group + " " + sub {
	case "user add":
		if len(a) != 1 {
			return usage()
		}
		var gs []string
		for _, g := range groups {
			gs = append(gs, groupName(g))
		}
		if err := db.AddUser(ctx, userName(a[0]), gs); err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "%s created\n", userName(a[0]))
	case "user list":
		ps, err := db.Principals(ctx)
		if err != nil {
			return fail(err)
		}
		for _, p := range ps {
			state := ""
			if p.Disabled {
				state = "\tdisabled"
			}
			gs, _ := db.LocalGroups(ctx, p.Name)
			fmt.Fprintf(stdout, "%s\t%s\t%s%s\n", p.Name, p.Issuer, strings.Join(gs, ","), state)
		}
	case "user groups":
		if len(a) < 1 {
			return usage()
		}
		var gs []string
		for _, g := range a[1:] {
			gs = append(gs, groupName(g))
		}
		if err := db.SetUserGroups(ctx, userName(a[0]), gs); err != nil {
			return fail(err)
		}
	case "user disable", "user enable":
		if len(a) != 1 {
			return usage()
		}
		if err := db.SetDisabled(ctx, userName(a[0]), sub == "disable"); err != nil {
			return fail(err)
		}
	case "token create":
		if len(a) != 1 {
			return usage()
		}
		t, secret, err := db.CreateToken(ctx, userName(a[0]), *description, *expires)
		if err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "%s\n", secret)
		exp := "never"
		if !t.Expires.IsZero() {
			exp = t.Expires.Format(time.RFC3339)
		}
		fmt.Fprintf(stderr, "token %s for %s, expires %s; it is shown only once\n", t.ID, t.Principal, exp)
	case "token list":
		user := ""
		if len(a) == 1 {
			user = userName(a[0])
		}
		toks, err := db.Tokens(ctx, user)
		if err != nil {
			return fail(err)
		}
		for _, t := range toks {
			state := "active"
			if t.Revoked {
				state = "revoked"
			} else if !t.Expires.IsZero() && time.Now().After(t.Expires) {
				state = "expired"
			}
			fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\n", t.ID, t.Principal, state, t.Description)
		}
	case "token revoke":
		if len(a) != 1 {
			return usage()
		}
		if err := db.RevokeToken(ctx, "", a[0]); err != nil {
			return fail(err)
		}
	case "s3key create":
		if len(a) != 1 {
			return usage()
		}
		k, err := db.CreateS3Key(ctx, userName(a[0]))
		if err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "access_key = %s\nsecret_key = %s\n", k.AccessKey, k.Secret)
	case "acl set":
		if len(a) != 4 {
			return usage()
		}
		var perms []string
		if a[3] != "none" && a[3] != "" {
			perms = strings.Split(a[3], ",")
		}
		if err := checkACLNamespace(a[0]); err != nil {
			return fail(err)
		}
		p, err := principalName(a[1])
		if err != nil {
			return fail(err)
		}
		if err := db.SetACL(ctx, serverdb.ACLEntry{Namespace: a[0], Principal: p, Prefix: a[2], Perms: perms}); err != nil {
			return fail(err)
		}
	case "acl rm":
		if len(a) != 3 {
			return usage()
		}
		p, err := principalName(a[1])
		if err != nil {
			return fail(err)
		}
		if err := db.RemoveACL(ctx, a[0], p, a[2]); err != nil {
			return fail(err)
		}
	case "acl list":
		if len(a) != 1 {
			return usage()
		}
		entries, err := db.ACL(ctx, a[0])
		if err != nil {
			return fail(err)
		}
		for _, e := range entries {
			perms := strings.Join(e.Perms, ",")
			if perms == "" {
				perms = "none"
			}
			fmt.Fprintf(stdout, "%s\t%q\t%s\n", e.Principal, e.Prefix, perms)
		}
	default:
		fmt.Fprintf(stderr, "f1d: unknown command %q\n\n%s", group+" "+sub, identityUsage)
		return 2
	}
	return 0
}

func checkACLNamespace(ns string) error {
	if ns == serverdb.ServerNamespace {
		return nil
	}
	return policy.ValidNamespace(ns)
}

func nsCmd(ctx context.Context, cfg config.Config, sub string, a []string, preset string, stdout io.Writer, fail func(error) int, usage func() int) int {
	switch sub {
	case "create":
		if len(a) != 1 {
			return usage()
		}
		ns := a[0]
		if admin, ok := server.DialAdmin(cfg.Server.DataDir); ok {
			if err := admin.Create(ctx, ns, preset); err != nil {
				return fail(err)
			}
		} else if err := createOffline(ctx, cfg, ns, preset); err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "%s created\n", ns)
		return 0
	}
	return usage()
}

// createOffline creates and registers a namespace while no server runs.
func createOffline(ctx context.Context, cfg config.Config, ns, preset string) error {
	pc := policy.Default()
	var err error
	if preset != "" {
		if pc, err = policy.Preset(preset); err != nil {
			return err
		}
	}
	if err := policy.ValidNamespace(ns); err != nil {
		return err
	}
	path := filepath.Join(cfg.Server.DataDir, server.ReposDir, ns)
	if err := repo.Create(path, pc); err != nil {
		return err
	}
	db, err := serverdb.Open(ctx, filepath.Join(cfg.Server.DataDir, server.ServerDB))
	if err != nil {
		return err
	}
	defer db.Close()
	meta, err := repodir.ReadMeta(path)
	if err != nil {
		return err
	}
	return db.Register(ctx, ns, meta.UUID)
}

// aclFile is the TOML format of export-acl and import-acl.
type aclFile struct {
	Namespace string     `toml:"namespace"`
	ACL       []aclEntry `toml:"acl"`
}

type aclEntry struct {
	Principal string   `toml:"principal"`
	Prefix    string   `toml:"prefix"`
	Perms     []string `toml:"perms"`
}

// exportACL writes a namespace's ACL as TOML.
func exportACL(ctx context.Context, db *serverdb.DB, ns string, w io.Writer) (int, error) {
	entries, err := db.ACL(ctx, ns)
	if err != nil {
		return 0, err
	}
	f := aclFile{Namespace: ns}
	for _, e := range entries {
		perms := e.Perms
		if perms == nil {
			perms = []string{}
		}
		f.ACL = append(f.ACL, aclEntry{Principal: e.Principal, Prefix: e.Prefix, Perms: perms})
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(f); err != nil {
		return 0, err
	}
	_, err = w.Write(buf.Bytes())
	return len(f.ACL), err
}

// importACL applies an exported ACL to a namespace (which may have another
// name than the exported one). Entries for users unknown on this server
// are reported and skipped; groups are always imported, since external
// groups are only known from tokens.
func importACL(ctx context.Context, db *serverdb.DB, ns string, data []byte, report io.Writer) (int, error) {
	var f aclFile
	md, err := toml.Decode(string(data), &f)
	if err != nil {
		return 0, err
	}
	if und := md.Undecoded(); len(und) > 0 {
		return 0, fmt.Errorf("unknown keys %v", und)
	}
	n := 0
	for _, e := range f.ACL {
		if strings.HasPrefix(e.Principal, "user:") {
			if _, err := db.LookupPrincipal(ctx, e.Principal); err != nil {
				fmt.Fprintf(report, "skipped %s %q: unknown on this server\n", e.Principal, e.Prefix)
				continue
			}
		}
		if err := db.SetACL(ctx, serverdb.ACLEntry{Namespace: ns, Principal: e.Principal, Prefix: e.Prefix, Perms: e.Perms}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
