// Package serverdb manages server.db: the server-level database for the
// repository registry and, from phase 3 on, identities, tokens, S3 keys
// and ACLs. Unlike cache.db it is a source of truth, so it is written with
// full durability.
package serverdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // SQLite driver "sqlite"
)

// migrations are applied in order; migration i brings the schema to version
// i+1. Never edit a released migration; append a new one.
var migrations = []string{
	`CREATE TABLE repositories (
		name       TEXT PRIMARY KEY,
		uuid       TEXT NOT NULL,
		created_at INTEGER NOT NULL
	);`,
}

// DB is an open server.db.
type DB struct {
	db *sql.DB
}

// Open opens or creates server.db at path and applies pending migrations.
// It refuses a database written by a newer F1 version.
func Open(ctx context.Context, path string) (*DB, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(FULL)&_pragma=foreign_keys(ON)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("serverdb: %w", err)
	}
	d := &DB{db: db}
	if err := d.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return d, nil
}

// Close closes the database.
func (d *DB) Close() error { return d.db.Close() }

// Version returns the schema version.
func (d *DB) Version(ctx context.Context) (int, error) {
	var v int
	err := d.db.QueryRowContext(ctx, `SELECT coalesce(max(version), 0) FROM schema_migrations`).Scan(&v)
	return v, err
}

func (d *DB) migrate(ctx context.Context) error {
	if _, err := d.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("serverdb: %w", err)
	}
	current, err := d.Version(ctx)
	if err != nil {
		return fmt.Errorf("serverdb: %w", err)
	}
	if current > len(migrations) {
		return fmt.Errorf("serverdb: schema version %d is newer than this server supports (%d); upgrade F1", current, len(migrations))
	}
	for v := current; v < len(migrations); v++ {
		tx, err := d.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("serverdb: %w", err)
		}
		if _, err := tx.ExecContext(ctx, migrations[v]); err != nil {
			tx.Rollback()
			return fmt.Errorf("serverdb: migration %d: %w", v+1, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)`, v+1, time.Now().Unix()); err != nil {
			tx.Rollback()
			return fmt.Errorf("serverdb: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("serverdb: migration %d: %w", v+1, err)
		}
	}
	return nil
}

// Repository is a registered repository.
type Repository struct {
	Name    string
	UUID    string
	Created time.Time
}

// ErrExists is returned when registering a name that is taken.
var ErrExists = errors.New("serverdb: repository already registered")

// ErrNotFound is returned for an unknown repository.
var ErrNotFound = errors.New("serverdb: repository not registered")

// Register adds a repository to the registry.
func (d *DB) Register(ctx context.Context, name, uuid string) error {
	var n int
	if err := d.db.QueryRowContext(ctx, `SELECT count(*) FROM repositories WHERE name = ?`, name).Scan(&n); err != nil {
		return fmt.Errorf("serverdb: %w", err)
	}
	if n > 0 {
		return fmt.Errorf("%w: %s", ErrExists, name)
	}
	_, err := d.db.ExecContext(ctx, `INSERT INTO repositories(name, uuid, created_at) VALUES (?, ?, ?)`, name, uuid, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("serverdb: %w", err)
	}
	return nil
}

// Unregister removes a repository from the registry.
func (d *DB) Unregister(ctx context.Context, name string) error {
	res, err := d.db.ExecContext(ctx, `DELETE FROM repositories WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("serverdb: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return nil
}

// Repositories lists the registry in name order.
func (d *DB) Repositories(ctx context.Context) ([]Repository, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT name, uuid, created_at FROM repositories ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("serverdb: %w", err)
	}
	defer rows.Close()
	var out []Repository
	for rows.Next() {
		var r Repository
		var created int64
		if err := rows.Scan(&r.Name, &r.UUID, &created); err != nil {
			return nil, fmt.Errorf("serverdb: %w", err)
		}
		r.Created = time.Unix(created, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}
