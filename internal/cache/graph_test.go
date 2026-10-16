package cache

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trukeio/f1/internal/hash"
)

// A version 3 cache is upgraded in place with the commit_graph table, and
// rows round-trip.
func TestGraphUpgradeAndRows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cache.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	v3 := strings.TrimSuffix(schema, graphSchema)
	if _, err := raw.Exec(v3 + "PRAGMA user_version = 3;"); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	c, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var version int
	if err := c.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("version = %d, %v", version, err)
	}
	a, b := hash.Plain([]byte("a")), hash.Plain([]byte("b"))
	rows := []GraphRow{
		{Commit: a, Generation: 1, Time: 10, Flags: 1},
		{Commit: b, Generation: 2, Time: 20, Parents: []hash.Hash{a}, Paths: []byte{1, 0xff}},
	}
	if err := c.PutGraphRows(ctx, rows); err != nil {
		t.Fatal(err)
	}
	got := map[hash.Hash]GraphRow{}
	if err := c.GraphRows(ctx, func(r GraphRow) { got[r.Commit] = r }); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got[a]) != fmt.Sprint(rows[0]) || fmt.Sprint(got[b]) != fmt.Sprint(rows[1]) {
		t.Fatalf("rows = %v", got)
	}
	if got[a].Paths != nil {
		t.Fatal("a row without a filter came back with one")
	}
	if err := c.DeleteGraphRows(ctx, []hash.Hash{a}); err != nil {
		t.Fatal(err)
	}
	n := 0
	c.GraphRows(ctx, func(GraphRow) { n++ })
	if n != 1 {
		t.Fatalf("%d rows after delete", n)
	}
	if err := c.ClearGraph(ctx); err != nil {
		t.Fatal(err)
	}
	n = 0
	c.GraphRows(ctx, func(GraphRow) { n++ })
	if n != 0 {
		t.Fatalf("%d rows after clear", n)
	}
}
