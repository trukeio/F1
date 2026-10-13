package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/trukeio/f1/internal/hash"
)

// GitKind is the kind of a git_map row.
type GitKind int

// git_map kinds.
const (
	GitCommit  GitKind = 1
	GitTree    GitKind = 2 // a canonical export of an F1 tree
	GitTag     GitKind = 3
	GitRawTree GitKind = 4 // a GitRaw object's exact tree bytes
)

// GitSHA returns the Git id an F1 object exports as, if it is recorded.
func (c *DB) GitSHA(ctx context.Context, f1 hash.Hash, kind GitKind) ([20]byte, bool, error) {
	var sha [20]byte
	var b []byte
	err := c.db.QueryRowContext(ctx, `SELECT sha1 FROM git_map WHERE f1 = ? AND kind = ?`, f1[:], int(kind)).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return sha, false, nil
	}
	if err != nil {
		return sha, false, fmt.Errorf("cache: %w", err)
	}
	copy(sha[:], b)
	return sha, len(b) == 20, nil
}

// PutGitSHA records the Git id of an F1 object.
func (c *DB) PutGitSHA(ctx context.Context, f1 hash.Hash, kind GitKind, sha [20]byte) error {
	_, err := c.db.ExecContext(ctx, `INSERT OR REPLACE INTO git_map(f1, kind, sha1) VALUES (?, ?, ?)`, f1[:], int(kind), sha[:])
	if err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	return nil
}

// GitObject returns an F1 object that exports as the Git id sha, if one is
// recorded (several F1 objects may map to one Git object).
func (c *DB) GitObject(ctx context.Context, sha [20]byte) (hash.Hash, GitKind, bool, error) {
	var h hash.Hash
	var b []byte
	var kind int
	err := c.db.QueryRowContext(ctx, `SELECT f1, kind FROM git_map WHERE sha1 = ? LIMIT 1`, sha[:]).Scan(&b, &kind)
	if errors.Is(err, sql.ErrNoRows) {
		return h, 0, false, nil
	}
	if err != nil {
		return h, 0, false, fmt.Errorf("cache: %w", err)
	}
	copy(h[:], b)
	return h, GitKind(kind), true, nil
}
