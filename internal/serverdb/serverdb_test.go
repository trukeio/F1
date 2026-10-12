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
