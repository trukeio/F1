package serverdb

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

var ctx = context.Background()

func TestRegistry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := db.Version(ctx); err != nil || v != len(migrations) {
		t.Fatalf("version = %d, %v", v, err)
	}
	if err := db.Register(ctx, "design", "u1"); err != nil {
		t.Fatal(err)
	}
	if err := db.Register(ctx, "code", "u2"); err != nil {
		t.Fatal(err)
	}
	if err := db.Register(ctx, "code", "u3"); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate register: %v", err)
	}
	db.Close()

	// Reopening applies no migration twice and keeps the data.
	db, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repos, err := db.Repositories(ctx)
	if err != nil || len(repos) != 2 || repos[0].Name != "code" || repos[1].UUID != "u1" {
		t.Fatalf("repositories = %+v, %v", repos, err)
	}
	if err := db.Unregister(ctx, "code"); err != nil {
		t.Fatal(err)
	}
	if err := db.Unregister(ctx, "code"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second unregister: %v", err)
	}
}

func TestNewerSchemaRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES (99, 0)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := Open(ctx, path); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("open of a newer schema: %v", err)
	}
}

// Guarded ACL changes keep an enabled server administrator (ADR-0020).
func TestLastAdminGuard(t *testing.T) {
	db, err := Open(ctx, filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(db.AddUser(ctx, "user:root", nil))
	must(db.AddUser(ctx, "user:alice", []string{"group:ops"}))
	admin := func(p string) ACLEntry {
		return ACLEntry{Namespace: ServerNamespace, Principal: p, Perms: []string{PermAdmin}}
	}
	must(db.SetACL(ctx, admin("user:root")))

	// The only administrator cannot be removed or narrowed.
	if err := db.RemoveACLGuarded(ctx, ServerNamespace, "user:root", ""); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("remove the last administrator: %v", err)
	}
	if err := db.SetACLGuarded(ctx, ACLEntry{Namespace: ServerNamespace, Principal: "user:root", Perms: []string{PermRead}}); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("narrow the last administrator: %v", err)
	}
	// An unknown user or a group without enabled members does not count.
	must(db.SetACLGuarded(ctx, admin("user:ghost")))
	must(db.SetACLGuarded(ctx, admin("group:empty")))
	if err := db.RemoveACLGuarded(ctx, ServerNamespace, "user:root", ""); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("only unknown administrators left: %v", err)
	}
	// A group with an enabled local member counts, until it is disabled.
	must(db.SetACLGuarded(ctx, admin("group:ops")))
	must(db.SetDisabled(ctx, "user:alice", true))
	if err := db.RemoveACLGuarded(ctx, ServerNamespace, "user:root", ""); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("group of a disabled user: %v", err)
	}
	must(db.SetDisabled(ctx, "user:alice", false))
	// A group recorded for an external principal counts.
	must(db.RemoveACLGuarded(ctx, ServerNamespace, "group:ops", ""))
	if _, err := db.SeenExternal(ctx, "user:carol", "corp", []string{"group:admins"}); err != nil {
		t.Fatal(err)
	}
	must(db.SetACLGuarded(ctx, admin("group:admins")))
	must(db.RemoveACLGuarded(ctx, ServerNamespace, "user:root", ""))
	// A refused change leaves the ACL as it was.
	if err := db.RemoveACLGuarded(ctx, ServerNamespace, "group:admins", ""); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("remove the last group: %v", err)
	}
	if e, _ := db.ACL(ctx, ServerNamespace); len(e) != 3 {
		t.Errorf("entries after a refused change: %+v", e)
	}
	// Guarded group changes and disabling.
	must(db.SetUserGroups(ctx, "user:alice", nil))
	must(db.SetACLGuarded(ctx, admin("group:ops")))
	must(db.SetUserGroupsGuarded(ctx, "user:alice", []string{"group:ops"}))
	must(db.RemoveACLGuarded(ctx, ServerNamespace, "group:admins", ""))
	if err := db.SetUserGroupsGuarded(ctx, "user:alice", nil); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("leave the last administrators' group: %v", err)
	}
	if err := db.SetDisabledGuarded(ctx, "user:alice", true); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("disable the last administrator: %v", err)
	}
	// Without any administrator, changes that add none still go through.
	must(db.SetDisabled(ctx, "user:alice", true))
	must(db.SetACLGuarded(ctx, ACLEntry{Namespace: ServerNamespace, Principal: "user:ghost2", Perms: []string{PermRead}}))
	must(db.SetDisabledGuarded(ctx, "user:alice", false))

	// Other namespaces are not guarded.
	must(db.SetACLGuarded(ctx, ACLEntry{Namespace: "ns", Principal: "user:alice", Perms: []string{PermAdmin}}))
	must(db.RemoveACLGuarded(ctx, "ns", "user:alice", ""))
	if err := db.RemoveACLGuarded(ctx, "ns", "user:alice", ""); !errors.Is(err, ErrNoACLEntry) {
		t.Errorf("remove a missing entry: %v", err)
	}
}
