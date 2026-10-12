// Package testutil holds helpers shared by tests: deterministic test data and
// golden files.
//
// Golden files live in testdata/golden/ at the repository root. They are
// regenerated only by an explicit "go test -run Golden -update" in the
// package that owns them; changing them is a format change and requires an
// ADR.
package testutil

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"testing"

	"lukechampine.com/blake3"
)

var update = flag.Bool("update", false, "rewrite golden files")

// Bytes returns n deterministic pseudo-random bytes derived from seed (BLAKE3
// XOF of the seed).
func Bytes(seed string, n int) []byte {
	h := blake3.New(32, nil)
	h.Write([]byte(seed))
	b := make([]byte, n)
	if _, err := h.XOF().Read(b); err != nil {
		panic(err)
	}
	return b
}

// Reader returns an endless stream of the same bytes Bytes(seed, n) starts
// with, for large test inputs that should not live in memory.
func Reader(seed string) io.Reader {
	h := blake3.New(32, nil)
	h.Write([]byte(seed))
	return h.XOF()
}

// RepoRoot returns the repository root (the directory holding go.mod).
func RepoRoot(t testing.TB) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

// Golden compares got, encoded as indented JSON, with
// testdata/golden/<name>.json. With -update it rewrites the file instead.
func Golden(t testing.TB, name string, got any) {
	t.Helper()
	data, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	path := filepath.Join(RepoRoot(t), "testdata", "golden", name+".json")
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("golden file %s: %v (run with -update to create it)", path, err)
	}
	if !bytes.Equal(data, want) {
		t.Errorf("golden mismatch for %s.\nThis is an on-disk format change. If intended, write an ADR and run: go test -run Golden -update\ngot:\n%s", name, data)
	}
}
