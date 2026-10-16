package cache

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/trukeio/f1/internal/hash"
)

// graphSchema holds the commit graph index (version 4, ADR-0015): data
// derived from each commit and its parents, computed on demand by the
// history queries.
const graphSchema = `
CREATE TABLE commit_graph (
	commit_hash BLOB PRIMARY KEY,     -- 32 bytes
	generation  INTEGER NOT NULL,     -- 1 + max generation of the stored parents
	time        INTEGER NOT NULL,     -- committer time (Unix s)
	parents     BLOB NOT NULL,        -- concatenated 32-byte hashes, in order
	flags       INTEGER NOT NULL,     -- 1: a parent was not stored when the row was computed
	paths       BLOB                  -- changed-path filter (see repo); NULL until computed
) WITHOUT ROWID;
`

// GraphRow is a commit_graph row.
type GraphRow struct {
	Commit     hash.Hash
	Generation uint64
	Time       int64
	Parents    []hash.Hash
	Flags      int
	Paths      []byte // nil: not computed
}

// GraphRows calls fn for every commit_graph row.
func (c *DB) GraphRows(ctx context.Context, fn func(GraphRow)) error {
	rows, err := c.db.QueryContext(ctx, `SELECT commit_hash, generation, time, parents, flags, paths FROM commit_graph`)
	if err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r GraphRow
		var h, parents, paths []byte
		if err := rows.Scan(&h, &r.Generation, &r.Time, &parents, &r.Flags, &paths); err != nil {
			return fmt.Errorf("cache: %w", err)
		}
		if len(h) != hash.Size || len(parents)%hash.Size != 0 {
			continue // not written by this code; recomputed on demand
		}
		copy(r.Commit[:], h)
		for i := 0; i < len(parents); i += hash.Size {
			r.Parents = append(r.Parents, hash.Hash(parents[i:i+hash.Size]))
		}
		if paths != nil {
			r.Paths = append([]byte{}, paths...)
		}
		fn(r)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	return nil
}

// PutGraphRows inserts or replaces commit_graph rows in one transaction.
func (c *DB) PutGraphRows(ctx context.Context, rows []GraphRow) error {
	if len(rows) == 0 {
		return nil
	}
	return c.tx(ctx, func(tx *sql.Tx) error {
		st, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO commit_graph(commit_hash, generation, time, parents, flags, paths) VALUES (?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer st.Close()
		for _, r := range rows {
			parents := make([]byte, 0, len(r.Parents)*hash.Size)
			for _, p := range r.Parents {
				parents = append(parents, p[:]...)
			}
			var paths any
			if r.Paths != nil {
				paths = r.Paths
			}
			if _, err := st.ExecContext(ctx, r.Commit[:], r.Generation, r.Time, parents, r.Flags, paths); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteGraphRows deletes the rows of commits.
func (c *DB) DeleteGraphRows(ctx context.Context, commits []hash.Hash) error {
	if len(commits) == 0 {
		return nil
	}
	return c.tx(ctx, func(tx *sql.Tx) error {
		for _, h := range commits {
			if _, err := tx.ExecContext(ctx, `DELETE FROM commit_graph WHERE commit_hash = ?`, h[:]); err != nil {
				return err
			}
		}
		return nil
	})
}

// ClearGraph deletes every commit_graph row.
func (c *DB) ClearGraph(ctx context.Context) error {
	if _, err := c.db.ExecContext(ctx, `DELETE FROM commit_graph`); err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	return nil
}
