package auth

import (
	"context"
	"strings"

	"github.com/trukeio/f1/internal/serverdb"
)

// Grants are the ACL entries of one namespace that apply to one caller,
// loaded once per request.
//
// Rules (doc/plan.md, "ACLs"; ADR-0006): an entry applies to paths that
// start with its prefix (plain string prefix, so "docs/" is a directory and
// "docs" also matches "docs.txt"). For a path, the longest applying prefix
// decides; entries for the caller and its groups with that prefix are
// combined. Empty perms deny. admin implies every permission. Admin on the
// server namespace "*" with prefix "" is admin everywhere. Default deny.
type Grants struct {
	entries     []grant
	serverAdmin bool
}

type grant struct {
	prefix string
	perms  map[string]bool
}

// Authorizer loads grants from server.db.
type Authorizer struct {
	DB *serverdb.DB
}

// For loads the caller's grants for a namespace.
func (a *Authorizer) For(ctx context.Context, c *Caller, ns string) (*Grants, error) {
	g := &Grants{}
	server, err := a.DB.ACLFor(ctx, serverdb.ServerNamespace, c.Principals())
	if err != nil {
		return nil, err
	}
	for _, e := range server {
		if e.Prefix == "" && contains(e.Perms, serverdb.PermAdmin) {
			g.serverAdmin = true
		}
	}
	if ns == serverdb.ServerNamespace {
		g.add(server)
		return g, nil
	}
	entries, err := a.DB.ACLFor(ctx, ns, c.Principals())
	if err != nil {
		return nil, err
	}
	g.add(entries)
	return g, nil
}

func (g *Grants) add(entries []serverdb.ACLEntry) {
	for _, e := range entries {
		perms := map[string]bool{}
		for _, p := range e.Perms {
			perms[p] = true
		}
		g.entries = append(g.entries, grant{prefix: e.Prefix, perms: perms})
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func (gr grant) has(perm string) bool { return gr.perms[perm] || gr.perms[serverdb.PermAdmin] }

// Allow reports whether the caller has perm on path.
func (g *Grants) Allow(path, perm string) bool {
	if g.serverAdmin {
		return true
	}
	best := -1
	allowed := false
	for _, e := range g.entries {
		if !strings.HasPrefix(path, e.prefix) {
			continue
		}
		switch {
		case len(e.prefix) > best:
			best, allowed = len(e.prefix), e.has(perm)
		case len(e.prefix) == best:
			allowed = allowed || e.has(perm)
		}
	}
	return allowed
}

// Whole reports whether the caller has perm on every path of the
// namespace: granted at the empty prefix and not narrowed by any longer
// prefix. Hash-addressed access needs whole-namespace read (D13).
func (g *Grants) Whole(perm string) bool {
	if g.serverAdmin {
		return true
	}
	if !g.Allow("", perm) {
		return false
	}
	// Entries at the same prefix combine, so check per prefix.
	byPrefix := map[string]bool{}
	for _, e := range g.entries {
		byPrefix[e.prefix] = byPrefix[e.prefix] || e.has(perm)
	}
	for _, ok := range byPrefix {
		if !ok {
			return false
		}
	}
	return true
}

// Any reports whether the caller has perm somewhere in the namespace.
func (g *Grants) Any(perm string) bool {
	if g.serverAdmin {
		return true
	}
	for _, e := range g.entries {
		if e.has(perm) {
			return true
		}
	}
	return false
}

// Under reports whether the caller has perm on some path starting with
// prefix: on the prefix itself, or through an entry for a longer prefix.
// Listings use it to show a collapsed prefix only if something below it is
// readable.
func (g *Grants) Under(prefix, perm string) bool {
	if g.Allow(prefix, perm) {
		return true
	}
	for _, e := range g.entries {
		if strings.HasPrefix(e.prefix, prefix) && e.has(perm) {
			return true
		}
	}
	return false
}

// ServerAdmin reports whether the caller is a server administrator.
func (g *Grants) ServerAdmin() bool { return g.serverAdmin }
