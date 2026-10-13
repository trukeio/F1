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
	GitRawRoot GitKind = 5 // the F1 tree a GitRaw tree describes
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

// GitRow is a git_map row.
type GitRow struct {
	F1   hash.Hash
	Kind GitKind
}

// GitObjects returns every F1 object recorded for the Git id sha.
func (c *DB) GitObjects(ctx context.Context, sha [20]byte) ([]GitRow, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT f1, kind FROM git_map WHERE sha1 = ?`, sha[:])
	if err != nil {
		return nil, fmt.Errorf("cache: %w", err)
	}
	defer rows.Close()
	var out []GitRow
	for rows.Next() {
		var b []byte
		var r GitRow
		if err := rows.Scan(&b, &r.Kind); err != nil {
			return nil, fmt.Errorf("cache: %w", err)
		}
		copy(r.F1[:], b)
		out = append(out, r)
	}
	return out, rows.Err()
}
