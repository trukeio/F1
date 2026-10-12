package backend

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFS(t *testing.T) {
	ctx := context.Background()
	b, err := NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("0123456789abcdef")
	if err := b.Put(ctx, "packs/a.pack", bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if err := b.Put(ctx, "packs/b.pack", strings.NewReader("bb"), -1); err != nil {
		t.Fatal(err)
	}
	if err := b.Put(ctx, "other/c", strings.NewReader("c"), 1); err != nil {
		t.Fatal(err)
	}

	read := func(off, n int64) string {
		t.Helper()
		rc, err := b.Get(ctx, "packs/a.pack", off, n)
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		got, err := io.ReadAll(rc)
		if err != nil {
			t.Fatal(err)
		}
		return string(got)
	}
	if got := read(0, -1); got != string(data) {
		t.Errorf("full read = %q", got)
	}
	if got := read(4, 3); got != "456" {
		t.Errorf("range read = %q", got)
	}
	if got := read(10, -1); got != "abcdef" {
		t.Errorf("tail read = %q", got)
	}
	if got := read(20, -1); got != "" {
		t.Errorf("read past end = %q", got)
	}

	var keys []string
	if err := b.List(ctx, "packs/", func(k string, size int64) error {
		keys = append(keys, k)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(keys, ",") != "packs/a.pack,packs/b.pack" {
		t.Errorf("List = %v", keys)
	}

	if err := b.Delete(ctx, "packs/a.pack"); err != nil {
		t.Fatal(err)
	}
	if err := b.Delete(ctx, "packs/a.pack"); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	if _, err := b.Get(ctx, "packs/a.pack", 0, -1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after delete: %v", err)
	}
}

func TestFSPutSizeMismatchLeavesNothing(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	b, err := NewFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Put(ctx, "x", strings.NewReader("abc"), 5); err == nil {
		t.Fatal("size mismatch accepted")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("leftover files: %v", entries)
	}
}

func TestFSTempFilesHidden(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	b, err := NewFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, tmpPrefix+"zz"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	n := 0
	if err := b.List(ctx, "", func(string, int64) error { n++; return nil }); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("List returned %d keys, want 0", n)
	}
}

func TestValidKey(t *testing.T) {
	for _, k := range []string{"", "/a", "a/", "a/../b", "./a", "a//b", "..", tmpPrefix + "x", "a/" + tmpPrefix + "x"} {
		if ValidKey(k) == nil {
			t.Errorf("ValidKey(%q) accepted", k)
		}
	}
	for _, k := range []string{"a", "packs/01J.pack", "a.b/c"} {
		if err := ValidKey(k); err != nil {
			t.Errorf("ValidKey(%q): %v", k, err)
		}
	}
}

func TestReaderAt(t *testing.T) {
	ctx := context.Background()
	b, err := NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Put(ctx, "k", strings.NewReader("hello world"), -1); err != nil {
		t.Fatal(err)
	}
	r := NewReaderAt(ctx, b, "k")
	p := make([]byte, 5)
	if n, err := r.ReadAt(p, 6); err != nil || string(p[:n]) != "world" {
		t.Fatalf("ReadAt = %q, %v", p[:n], err)
	}
	if n, err := r.ReadAt(p, 8); !errors.Is(err, io.EOF) || string(p[:n]) != "rld" {
		t.Fatalf("short ReadAt = %q, %v", p[:n], err)
	}
}
