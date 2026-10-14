// Package client implements the F1 native client's working copy
// (doc/plan.md, "Native client"; ADR-0010): clone with sparse patterns,
// status, local commits, push, pull with per-path merge, locks and
// branches. cmd/f1 is its command-line front end.
//
// The working copy keeps its metadata in .f1/:
//
//	config.json  server, namespace, ref and sparse patterns
//	state.json   base commit, local commits, added paths, conflicts
//	index.json   path → entry, local content hash and stat data
//	objects/     one pack per local commit (content not yet pushed)
//	tmp/         downloads and merge results in progress
//
// Content is read through the path-addressed native API (listings, diffs,
// file reads at a commit), so a sparse working copy fetches only metadata
// and files inside its patterns. Pushing uses the object endpoints.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/trukeio/f1/internal/policy"
	"github.com/trukeio/f1/pkg/f1client"
)

// MetaDir is the name of the working copy's metadata directory.
const MetaDir = ".f1"

// ErrNotWorkingCopy is returned by Open outside a working copy.
var ErrNotWorkingCopy = errors.New("not an f1 working copy (no .f1 directory found)")

// Config is .f1/config.json.
type Config struct {
	Server    string `json:"server"`
	Namespace string `json:"namespace"`
	// Ref is the branch the working copy follows ("main"), or a commit
	// hash for a detached working copy, which cannot push.
	Ref string `json:"ref"`
	// Sparse lists the directories that are checked out; empty means
	// everything.
	Sparse []string `json:"sparse,omitempty"`
}

// Entry is a file's value in a commit.
type Entry struct {
	Kind string `json:"kind"` // file, exec, symlink, lfs
	File string `json:"file"` // File hash
	Size int64  `json:"size"`
	// Local is the File hash of the content as chunked by this client, if
	// known. It equals File unless the server chunked differently (S3
	// multipart uploads), and is what the working copy compares.
	Local string `json:"local,omitempty"`
}

// localHash returns the hash that identifies the entry's content locally.
func (e *Entry) localHash() string {
	if e.Local != "" {
		return e.Local
	}
	return e.File
}

// sameContent reports whether two entries (nil: absent) have equal
// content and kind.
func sameContent(a, b *Entry) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return normKind(a.Kind) == normKind(b.Kind) && (a.File == b.File || a.localHash() == b.localHash())
}

// IndexEntry is the value of a path in the working copy's HEAD (the base
// commit plus local commits), with the stat data of the file that holds it.
type IndexEntry struct {
	Entry
	// Mtime is the file's modification time (ns) when it was known to hold
	// the entry's content; 0 forces the content to be hashed again.
	Mtime int64  `json:"mtime"`
	Ino   uint64 `json:"ino,omitempty"`
}

// PathChange is one change of a local commit. Old is the value before the
// commit, New the value after it; nil means absent.
type PathChange struct {
	Path string `json:"path"`
	Old  *Entry `json:"old,omitempty"`
	New  *Entry `json:"new,omitempty"`
}

// LocalCommit is a commit that has not been pushed.
type LocalCommit struct {
	ID      string       `json:"id"`
	Message string       `json:"message"`
	Time    time.Time    `json:"time"`
	Changes []PathChange `json:"changes"`
}

// State is .f1/state.json.
type State struct {
	// Base is the server commit the working copy is based on ("" for a ref
	// with no commit yet).
	Base    string        `json:"base"`
	Commits []LocalCommit `json:"commits,omitempty"`
	// Added lists untracked paths scheduled for the next commit.
	Added []string `json:"added,omitempty"`
	// Conflicts lists paths with unresolved merge conflicts.
	Conflicts []string `json:"conflicts,omitempty"`
	// Message holds the messages of local commits that a pull turned back
	// into working changes, as a suggestion for the next commit.
	Message string `json:"message,omitempty"`
}

// Options configure a working copy's connection.
type Options struct {
	Token string       // bearer token
	HTTP  *http.Client // nil: http.DefaultClient
	Out   io.Writer    // progress and messages; nil discards them
	// Workers is the number of parallel downloads (default 4).
	Workers int
}

// WC is an open working copy.
type WC struct {
	Root   string
	Config Config
	State  State
	Index  map[string]*IndexEntry

	api     *f1client.Client
	out     io.Writer
	workers int
	objects *objectStore

	ns        *f1client.Namespace // fetched on first use
	principal string
	locked    bool
}

// lock takes the working copy's lock file, so that two f1 processes do not
// change it at once.
func (wc *WC) lock() error {
	f, err := os.OpenFile(wc.meta("lock"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("another f1 process is using this working copy (if none is, remove %s)", wc.meta("lock"))
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(f, "%d\n", os.Getpid())
	wc.locked = true
	return f.Close()
}

func (wc *WC) meta(name ...string) string {
	return filepath.Join(append([]string{wc.Root, MetaDir}, name...)...)
}

func newWC(root string, cfg Config, opts Options) *WC {
	wc := &WC{Root: root, Config: cfg, Index: map[string]*IndexEntry{}, out: opts.Out, workers: opts.Workers}
	if wc.out == nil {
		wc.out = io.Discard
	}
	if wc.workers <= 0 {
		wc.workers = 4
	}
	wc.api = f1client.New(cfg.Server, opts.Token)
	wc.api.HTTP = opts.HTTP
	return wc
}

// Open opens the working copy that contains dir.
func Open(dir string, opts Options) (*WC, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	for {
		if st, err := os.Stat(filepath.Join(root, MetaDir)); err == nil && st.IsDir() {
			break
		}
		parent := filepath.Dir(root)
		if parent == root {
			return nil, ErrNotWorkingCopy
		}
		root = parent
	}
	var cfg Config
	if err := readJSON(filepath.Join(root, MetaDir, "config.json"), &cfg); err != nil {
		return nil, err
	}
	wc := newWC(root, cfg, opts)
	if err := wc.lock(); err != nil {
		return nil, err
	}
	if err := wc.load(); err != nil {
		wc.Close()
		return nil, err
	}
	return wc, nil
}

func (wc *WC) load() error {
	if err := readJSON(wc.meta("state.json"), &wc.State); err != nil {
		return err
	}
	if err := readJSON(wc.meta("index.json"), &wc.Index); err != nil {
		return err
	}
	if wc.Index == nil {
		wc.Index = map[string]*IndexEntry{}
	}
	var err error
	if wc.objects, err = openObjects(wc.meta("objects")); err != nil {
		return err
	}
	return os.RemoveAll(wc.meta("tmp"))
}

// Close releases the working copy's files and its lock.
func (wc *WC) Close() error {
	if wc.objects != nil {
		wc.objects.Close()
	}
	if wc.locked {
		wc.locked = false
		return os.Remove(wc.meta("lock"))
	}
	return nil
}

// save writes the state and the index, each atomically.
func (wc *WC) save() error {
	if err := writeJSON(wc.meta("index.json"), wc.Index); err != nil {
		return err
	}
	return writeJSON(wc.meta("state.json"), wc.State)
}

func (wc *WC) saveConfig() error {
	return writeJSON(wc.meta("config.json"), wc.Config)
}

func readJSON(name string, v any) error {
	b, err := os.ReadFile(name)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// writeJSON replaces a file atomically and durably.
func writeJSON(name string, v any) error {
	b, err := json.MarshalIndent(v, "", "\t")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(name), filepath.Base(name)+".tmp-*")
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		os.Remove(f.Name())
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(f.Name())
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return err
	}
	if err := os.Rename(f.Name(), name); err != nil {
		os.Remove(f.Name())
		return err
	}
	return syncDir(filepath.Dir(name))
}

// namespace returns the namespace's description (cached).
func (wc *WC) namespace(ctx context.Context) (*f1client.Namespace, error) {
	if wc.ns != nil {
		return wc.ns, nil
	}
	list, err := wc.api.Namespaces(ctx)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].Name == wc.Config.Namespace {
			wc.ns = &list[i]
			return wc.ns, nil
		}
	}
	return nil, fmt.Errorf("namespace %s not found", wc.Config.Namespace)
}

// me returns the caller's principal name (cached).
func (wc *WC) me(ctx context.Context) (string, error) {
	if wc.principal != "" {
		return wc.principal, nil
	}
	m, err := wc.api.Me(ctx)
	if err != nil {
		return "", err
	}
	wc.principal = m.Principal
	return wc.principal, nil
}

// detached reports whether the working copy follows a commit, not a
// branch.
func (wc *WC) detached() bool { return isHash(wc.Config.Ref) }

func isHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// head returns the commit the ref points to ("" if the branch has none).
func (wc *WC) head(ctx context.Context) (string, error) {
	if wc.detached() {
		return wc.Config.Ref, nil
	}
	refs, err := wc.api.Refs(ctx, wc.Config.Namespace)
	if err != nil {
		return "", err
	}
	want := policy.FullRefName(wc.Config.Ref)
	for _, r := range refs {
		if r.Name == want {
			return r.Commit, nil
		}
	}
	return "", nil
}

// --- paths ---

// cleanPattern normalizes a sparse pattern (a directory path).
func cleanPattern(p string) (string, error) {
	p = strings.Trim(path.Clean("/"+filepath.ToSlash(p)), "/")
	if p == "" {
		return "", nil
	}
	if err := checkPath(p); err != nil {
		return "", err
	}
	return p, nil
}

// checkPath refuses paths that cannot be written safely inside the
// working copy: absolute paths, "." and ".." components, empty components,
// and the metadata directory. A server could send them.
func checkPath(p string) error {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return fmt.Errorf("unsafe path %q", p)
	}
	for i, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." || (i == 0 && strings.EqualFold(seg, MetaDir)) || strings.ContainsRune(seg, 0) {
			return fmt.Errorf("unsafe path %q", p)
		}
	}
	return nil
}

// inSparse reports whether a path is inside the sparse patterns.
func (wc *WC) inSparse(p string) bool {
	if len(wc.Config.Sparse) == 0 {
		return true
	}
	for _, s := range wc.Config.Sparse {
		if s == "" || p == s || strings.HasPrefix(p, s+"/") {
			return true
		}
	}
	return false
}

// prefixes returns the listing and diff prefixes of the sparse patterns.
func (wc *WC) prefixes() []string {
	if len(wc.Config.Sparse) == 0 || slices.Contains(wc.Config.Sparse, "") {
		return []string{""}
	}
	out := make([]string, len(wc.Config.Sparse))
	for i, s := range wc.Config.Sparse {
		out[i] = s + "/"
	}
	return out
}

// abs returns the filesystem path of a working copy path.
func (wc *WC) abs(p string) string { return filepath.Join(wc.Root, filepath.FromSlash(p)) }

// Rel converts a filesystem path (absolute or relative to the current
// directory) to a working copy path ("" for the root).
func (wc *WC) Rel(name string) (string, error) {
	a, err := filepath.Abs(name)
	if err != nil {
		return "", err
	}
	r, err := filepath.Rel(wc.Root, a)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is outside the working copy", name)
	}
	if r == "." {
		return "", nil
	}
	r = filepath.ToSlash(r)
	if err := checkPath(r); err != nil {
		return "", err
	}
	return r, nil
}

// mkParents creates the parent directories of p, refusing to go through
// a symbolic link or a file: a checked-out symlink must not redirect a
// later write outside the working copy.
func (wc *WC) mkParents(p string) error {
	dir := wc.Root
	segs := strings.Split(p, "/")
	for _, seg := range segs[:len(segs)-1] {
		dir = filepath.Join(dir, seg)
		st, err := os.Lstat(dir)
		switch {
		case errors.Is(err, os.ErrNotExist):
			if err := os.Mkdir(dir, 0o777); err != nil {
				return err
			}
		case err != nil:
			return err
		case st.Mode()&os.ModeSymlink != 0 || !st.IsDir():
			return fmt.Errorf("cannot write %s: %s is not a directory", p, dir)
		}
	}
	return nil
}

// removeEmptyParents removes the empty directories above p.
func (wc *WC) removeEmptyParents(p string) {
	for dir := path.Dir(p); dir != "." && dir != "/"; dir = path.Dir(dir) {
		if os.Remove(wc.abs(dir)) != nil {
			return
		}
	}
}

func (wc *WC) printf(format string, args ...any) {
	fmt.Fprintf(wc.out, format, args...)
}
