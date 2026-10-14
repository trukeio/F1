package client

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckPath(t *testing.T) {
	for p, ok := range map[string]bool{
		"a.txt": true, "d/e/f.bin": true, ".hidden": true, "d/.f1": true, "x/..y": true,
		"": false, "/abs": false, "a//b": false, "a/./b": false, "../up": false, "a/..": false,
		".f1": false, ".F1/state.json": false, "a\\b": false, "nul\x00": false, "a/": false,
	} {
		if err := checkPath(p); (err == nil) != ok {
			t.Errorf("checkPath(%q) = %v", p, err)
		}
	}
}

func TestSparse(t *testing.T) {
	wc := &WC{Config: Config{Sparse: []string{"d1", "x/y"}}}
	for p, want := range map[string]bool{
		"d1/a": true, "d1": true, "d10/a": false, "x/y/z": true, "x/z": false, "top": false,
	} {
		if got := wc.inSparse(p); got != want {
			t.Errorf("inSparse(%q) = %v", p, got)
		}
	}
	for d, want := range map[string]bool{"x": true, "d1": true, "d1/sub": true, "d2": false, "x/w": false} {
		if got := wc.sparseDir(d); got != want {
			t.Errorf("sparseDir(%q) = %v", d, got)
		}
	}
	if got := wc.prefixes(); len(got) != 2 || got[0] != "d1/" || got[1] != "x/y/" {
		t.Errorf("prefixes = %v", got)
	}
	for in, want := range map[string]string{"d1/": "d1", "/d1": "d1", "./a/b/": "a/b", ".": ""} {
		if got, err := cleanPattern(in); err != nil || got != want {
			t.Errorf("cleanPattern(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := cleanPattern(".f1"); err == nil {
		t.Error("the metadata directory is not a pattern")
	}
}

// Parent directories are never followed through symbolic links.
func TestMkParents(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	wc := &WC{Root: root}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skip("no symlinks")
	}
	if err := wc.mkParents("link/x.txt"); err == nil {
		t.Fatal("wrote through a symbolic link")
	}
	if err := os.WriteFile(filepath.Join(root, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := wc.mkParents("file/x.txt"); err == nil {
		t.Fatal("a file as a directory")
	}
	if err := wc.mkParents("a/b/c.txt"); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(filepath.Join(root, "a", "b")); err != nil || !st.IsDir() {
		t.Fatal("parents not created")
	}
}

func TestSameContent(t *testing.T) {
	a := &Entry{Kind: "file", File: "f1"}
	if !sameContent(nil, nil) || sameContent(a, nil) || sameContent(nil, a) {
		t.Fatal("absent entries")
	}
	// Different chunking, same content (S3 multipart File vs local).
	if !sameContent(&Entry{Kind: "lfs", File: "srv", Local: "f1"}, a) {
		t.Fatal("local hash and LFS kind")
	}
	if sameContent(&Entry{Kind: "exec", File: "f1"}, a) {
		t.Fatal("kind change")
	}
}
