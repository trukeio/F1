// Package cache manages a repository's cache.db: the pack list, the object
// index (hash → pack, offset) and the digest index. Everything in it is
// derivable from packs/ (D7, D21); the pack store rebuilds it when it is
// missing, corrupt or out of date.
package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	_ "modernc.org/sqlite" // SQLite driver "sqlite"

	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
	"github.com/trukeio/f1/internal/pack"
)

// SchemaVersion is stored in PRAGMA user_version. Caches of versions 1 and
// 2 are upgraded in place; a cache with any other version is discarded and
// rebuilt.
const SchemaVersion = 3

// ErrSchema is returned by Open for a cache with another schema version.
var ErrSchema = errors.New("cache: schema version mismatch")

const schema = `
CREATE TABLE packs (
	id    INTEGER PRIMARY KEY,
	name  TEXT NOT NULL UNIQUE,
	size  INTEGER NOT NULL,
	count INTEGER NOT NULL
);
CREATE TABLE objects (
	hash    BLOB PRIMARY KEY,
	pack_id INTEGER NOT NULL REFERENCES packs(id),
	offset  INTEGER NOT NULL,
	stored  INTEGER NOT NULL,
	size    INTEGER NOT NULL,
	type    INTEGER NOT NULL,
	flags   INTEGER NOT NULL
) WITHOUT ROWID;
CREATE INDEX objects_pack ON objects(pack_id);
CREATE TABLE digests (
	file    BLOB PRIMARY KEY,
	sha1git BLOB NOT NULL,
	sha256  BLOB NOT NULL,
	md5     BLOB NOT NULL
) WITHOUT ROWID;
CREATE INDEX digests_sha1git ON digests(sha1git);
CREATE INDEX digests_sha256 ON digests(sha256);
CREATE TABLE pending_deletes (
	name  TEXT PRIMARY KEY,
	after INTEGER NOT NULL
);
` + uploadsSchema + gitMapSchema

// gitMapSchema maps F1 objects to the Git objects they export as (version
// 3). Blobs are not in it: their Git ids are in digests. Export is
// deterministic, so the table is recomputed on demand after a rebuild.
const gitMapSchema = `
CREATE TABLE git_map (
	f1   BLOB NOT NULL,
	kind INTEGER NOT NULL,            -- 1 commit, 2 tree, 3 tag, 4 GitRaw tree bytes
	sha1 BLOB NOT NULL,
	PRIMARY KEY (f1, kind)
) WITHOUT ROWID;
CREATE INDEX git_map_sha1 ON git_map(sha1);
`

// uploadsSchema holds open S3 multipart uploads (version 2). Upload state
// is the one thing in cache.db that packs cannot restore: after a rebuild,
// open uploads are gone and clients restart them (doc/plan.md, "Multipart
// internals").
const uploadsSchema = `
CREATE TABLE uploads (
	id        TEXT PRIMARY KEY,
	ref       TEXT NOT NULL,
	key       TEXT NOT NULL,
	principal TEXT NOT NULL,
	created   INTEGER NOT NULL,       -- Unix ns
	attrs     TEXT NOT NULL           -- JSON: content type, metadata, checksum settings
) WITHOUT ROWID;
CREATE INDEX uploads_key ON uploads(ref, key, id);
CREATE TABLE completed_uploads (    -- completed uploads, so that a repeated completion succeeds
	id       TEXT PRIMARY KEY,
	ref      TEXT NOT NULL,
	key      TEXT NOT NULL,
	result   TEXT NOT NULL,           -- JSON: ETag, version id, checksum of the completion
	created  INTEGER NOT NULL         -- Unix ns of the completion
) WITHOUT ROWID;
CREATE TABLE upload_parts (
	upload   TEXT NOT NULL,
	number   INTEGER NOT NULL,
	file     BLOB NOT NULL,
	size     INTEGER NOT NULL,
	etag     TEXT NOT NULL,           -- hex MD5 of the part
	checksum BLOB,                    -- the upload's checksum algorithm over the part
	created  INTEGER NOT NULL,        -- Unix ns
	PRIMARY KEY (upload, number)
) WITHOUT ROWID;
`

// DB is an open cache.db.
type DB struct {
	db     *sql.DB
	lookup *sql.Stmt
	has    *sql.Stmt
}

// Open opens or creates the cache at path. It returns ErrSchema for a cache
// written with another schema version; the caller removes and rebuilds it.
func Open(path string) (*DB, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("cache: %w", err)
	}
	c := &DB{db: db}
	if err := c.init(); err != nil {
		db.Close()
		return nil, err
	}
	return c, nil
}

func (c *DB) init() error {
	var version int
	if err := c.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	switch version {
	case 0:
		var tables int
		if err := c.db.QueryRow("SELECT count(*) FROM sqlite_master").Scan(&tables); err != nil {
			return fmt.Errorf("cache: %w", err)
		}
		if tables != 0 {
			return ErrSchema
		}
		if _, err := c.db.Exec(schema + fmt.Sprintf("PRAGMA user_version = %d;", SchemaVersion)); err != nil {
			return fmt.Errorf("cache: create schema: %w", err)
		}
	case 1, 2:
		upgrade := gitMapSchema
		if version == 1 {
			upgrade = uploadsSchema + upgrade
		}
		if _, err := c.db.Exec(upgrade + fmt.Sprintf("PRAGMA user_version = %d;", SchemaVersion)); err != nil {
			return fmt.Errorf("cache: upgrade schema: %w", err)
		}
	case SchemaVersion:
	default:
		return ErrSchema
	}
	var err error
	if c.lookup, err = c.db.Prepare(`SELECT p.name, o.offset, o.stored, o.size, o.type, o.flags
		FROM objects o JOIN packs p ON p.id = o.pack_id WHERE o.hash = ?`); err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	if c.has, err = c.db.Prepare(`SELECT 1 FROM objects WHERE hash = ?`); err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	return nil
}

// Remove deletes the cache files at path (database, WAL and shared memory).
func Remove(path string) error {
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("cache: %w", err)
		}
	}
	return nil
}

// Checkpoint copies the WAL into the main database file and truncates the
// WAL, so that cache.db alone is a consistent snapshot (for frozen copies).
func (c *DB) Checkpoint(ctx context.Context) error {
	if _, err := c.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("cache: checkpoint: %w", err)
	}
	return nil
}

// Close closes the database.
func (c *DB) Close() error { return c.db.Close() }

// PackInfo describes an indexed pack.
type PackInfo struct {
	Name  string
	Size  int64
	Count int
}

// Packs returns every indexed pack, ordered by name.
func (c *DB) Packs(ctx context.Context) ([]PackInfo, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT name, size, count FROM packs ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("cache: %w", err)
	}
	defer rows.Close()
	var out []PackInfo
	for rows.Next() {
		var p PackInfo
		if err := rows.Scan(&p.Name, &p.Size, &p.Count); err != nil {
			return nil, fmt.Errorf("cache: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// NewPack is a sealed pack to index.
type NewPack struct {
	Name    string
	Size    int64
	Entries []pack.IndexEntry
	Digests []object.DigestRecord
}

// AddPacks indexes sealed packs in one transaction. An object already
// indexed in another pack keeps its existing location.
func (c *DB) AddPacks(ctx context.Context, packs ...NewPack) error {
	return c.tx(ctx, func(tx *sql.Tx) error {
		return addPacks(ctx, tx, packs, false)
	})
}

// ReplacePacks indexes new packs and removes old ones in one transaction.
// Objects of the new packs take over their index rows (a consolidation or
// GC rewrite copied them); every object of the old packs must be in the new
// ones. The old pack files are scheduled for deletion after grace.
func (c *DB) ReplacePacks(ctx context.Context, old []string, packs []NewPack, grace time.Duration) error {
	after := time.Now().Add(grace).UnixNano()
	return c.tx(ctx, func(tx *sql.Tx) error {
		if err := addPacks(ctx, tx, packs, true); err != nil {
			return err
		}
		for _, name := range old {
			var left int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM objects o JOIN packs p ON p.id = o.pack_id WHERE p.name = ?`, name).Scan(&left); err != nil {
				return fmt.Errorf("cache: %w", err)
			}
			if left != 0 {
				return fmt.Errorf("cache: replacing pack %s would drop %d objects", name, left)
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM packs WHERE name = ?`, name); err != nil {
				return fmt.Errorf("cache: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO pending_deletes(name, after) VALUES (?, ?)`, name, after); err != nil {
				return fmt.Errorf("cache: %w", err)
			}
		}
		return nil
	})
}

func addPacks(ctx context.Context, tx *sql.Tx, packs []NewPack, replace bool) error {
	verb := "INSERT OR IGNORE"
	if replace {
		verb = "INSERT OR REPLACE"
	}
	insObj, err := tx.PrepareContext(ctx, verb+` INTO objects(hash, pack_id, offset, stored, size, type, flags) VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	defer insObj.Close()
	insDigest, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO digests(file, sha1git, sha256, md5) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	defer insDigest.Close()
	for _, p := range packs {
		res, err := tx.ExecContext(ctx, `INSERT INTO packs(name, size, count) VALUES (?, ?, ?)`, p.Name, p.Size, len(p.Entries))
		if err != nil {
			return fmt.Errorf("cache: add pack %s: %w", p.Name, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("cache: %w", err)
		}
		for _, e := range p.Entries {
			if _, err := insObj.ExecContext(ctx, e.Hash[:], id, int64(e.Offset), e.Stored, e.Size, int(e.Type), int(e.Flags)); err != nil {
				return fmt.Errorf("cache: %w", err)
			}
		}
		for _, d := range p.Digests {
			if _, err := insDigest.ExecContext(ctx, d.File[:], d.SHA1Git[:], d.SHA256[:], d.MD5[:]); err != nil {
				return fmt.Errorf("cache: %w", err)
			}
		}
	}
	return nil
}

func (c *DB) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("cache: commit: %w", err)
	}
	return nil
}

// Location is where an object is stored.
type Location struct {
	Pack  string
	Entry pack.IndexEntry
}

// Lookup returns the location of h.
func (c *DB) Lookup(ctx context.Context, h hash.Hash) (Location, bool, error) {
	var loc Location
	var off int64
	var typ, flags int
	err := c.lookup.QueryRowContext(ctx, h[:]).Scan(&loc.Pack, &off, &loc.Entry.Stored, &loc.Entry.Size, &typ, &flags)
	if errors.Is(err, sql.ErrNoRows) {
		return Location{}, false, nil
	}
	if err != nil {
		return Location{}, false, fmt.Errorf("cache: %w", err)
	}
	loc.Entry.Hash = h
	loc.Entry.Offset = uint64(off)
	loc.Entry.Type = object.Type(typ)
	loc.Entry.Flags = uint8(flags)
	return loc, true, nil
}

// Has reports whether h is indexed.
func (c *DB) Has(ctx context.Context, h hash.Hash) (bool, error) {
	var one int
	err := c.has.QueryRowContext(ctx, h[:]).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("cache: %w", err)
	}
	return true, nil
}

// Digest returns the digest record of a File.
func (c *DB) Digest(ctx context.Context, file hash.Hash) (object.DigestRecord, bool, error) {
	d := object.DigestRecord{File: file}
	var sha1, sha256, md5 []byte
	err := c.db.QueryRowContext(ctx, `SELECT sha1git, sha256, md5 FROM digests WHERE file = ?`, file[:]).Scan(&sha1, &sha256, &md5)
	if errors.Is(err, sql.ErrNoRows) {
		return d, false, nil
	}
	if err != nil {
		return d, false, fmt.Errorf("cache: %w", err)
	}
	copy(d.SHA1Git[:], sha1)
	copy(d.SHA256[:], sha256)
	copy(d.MD5[:], md5)
	return d, true, nil
}

// FileBySHA256 returns a File whose content has the given SHA-256 (LFS oid).
func (c *DB) FileBySHA256(ctx context.Context, sum [32]byte) (hash.Hash, bool, error) {
	return c.fileBy(ctx, `SELECT file FROM digests WHERE sha256 = ? LIMIT 1`, sum[:])
}

// FileBySHA1Git returns a File whose Git blob SHA-1 is sum.
func (c *DB) FileBySHA1Git(ctx context.Context, sum [20]byte) (hash.Hash, bool, error) {
	return c.fileBy(ctx, `SELECT file FROM digests WHERE sha1git = ? LIMIT 1`, sum[:])
}

func (c *DB) fileBy(ctx context.Context, query string, key []byte) (hash.Hash, bool, error) {
	var b []byte
	err := c.db.QueryRowContext(ctx, query, key).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return hash.Zero, false, nil
	}
	if err != nil {
		return hash.Zero, false, fmt.Errorf("cache: %w", err)
	}
	var h hash.Hash
	copy(h[:], b)
	return h, true, nil
}

// ObjectsInPack returns the index entries of a pack, ordered by offset.
func (c *DB) ObjectsInPack(ctx context.Context, name string) ([]pack.IndexEntry, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT o.hash, o.offset, o.stored, o.size, o.type, o.flags
		FROM objects o JOIN packs p ON p.id = o.pack_id WHERE p.name = ? ORDER BY o.offset`, name)
	if err != nil {
		return nil, fmt.Errorf("cache: %w", err)
	}
	defer rows.Close()
	var out []pack.IndexEntry
	for rows.Next() {
		var e pack.IndexEntry
		var hb []byte
		var off int64
		var typ, flags int
		if err := rows.Scan(&hb, &off, &e.Stored, &e.Size, &typ, &flags); err != nil {
			return nil, fmt.Errorf("cache: %w", err)
		}
		copy(e.Hash[:], hb)
		e.Offset, e.Type, e.Flags = uint64(off), object.Type(typ), uint8(flags)
		out = append(out, e)
	}
	return out, rows.Err()
}

// CountObjects returns the number of indexed objects.
func (c *DB) CountObjects(ctx context.Context) (int, error) {
	var n int
	err := c.db.QueryRowContext(ctx, `SELECT count(*) FROM objects`).Scan(&n)
	return n, err
}

// DuePendingDeletes returns pack files whose deletion grace period has
// passed.
func (c *DB) DuePendingDeletes(ctx context.Context, now time.Time) ([]string, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT name FROM pending_deletes WHERE after <= ? ORDER BY name`, now.UnixNano())
	if err != nil {
		return nil, fmt.Errorf("cache: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// PendingDeletes returns every pack file scheduled for deletion.
func (c *DB) PendingDeletes(ctx context.Context) (map[string]bool, error) {
	names, err := c.DuePendingDeletes(ctx, time.Unix(0, 1<<62))
	if err != nil {
		return nil, err
	}
	m := map[string]bool{}
	for _, n := range names {
		m[n] = true
	}
	return m, nil
}

// DonePendingDelete removes a pack file from the pending deletes.
func (c *DB) DonePendingDelete(ctx context.Context, name string) error {
	_, err := c.db.ExecContext(ctx, `DELETE FROM pending_deletes WHERE name = ?`, name)
	return err
}
