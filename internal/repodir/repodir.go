// Package repodir manages a repository directory: f1repo.toml, the refs and
// refs.log files, the locks file, the repository lock and tmp/.
//
// Layout (doc/plan.md, "Repository directory"):
//
//	f1repo.toml  namespace config, format version, repository UUID
//	refs         current refs, one "<hash-hex> <refname>" line per ref, sorted
//	refs.log     append-only log of every ref update
//	locks        current file locks
//	lock         empty file; flock held by whoever has the repository open
//	packs/       immutable pack files
//	cache.db     rebuildable cache
//	tmp/         unsealed packs and in-progress uploads; never copied
package repodir

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/policy"
)

// FormatVersion is the repository format this code reads and writes.
const FormatVersion = 1

// File and directory names inside a repository directory.
const (
	ConfigFile = "f1repo.toml"
	RefsFile   = "refs"
	LogFile    = "refs.log"
	LocksFile  = "locks"
	LockFile   = "lock"
	PacksDir   = "packs"
	CacheFile  = "cache.db"
	TmpDir     = "tmp"
)

// ErrLocked is returned when another process holds the repository open.
var ErrLocked = errors.New("repodir: repository is in use by another process")

// Meta is the content of f1repo.toml.
type Meta struct {
	Format int    `toml:"format"`
	UUID   string `toml:"uuid"`
	policy.Config
}

// Dir is an open repository directory. It holds the repository lock until
// Close.
type Dir struct {
	path string
	lock *os.File
	meta Meta
}

// Create initializes a new repository directory at path, which must not
// exist or be empty.
func Create(path string, cfg policy.Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if entries, err := os.ReadDir(path); err == nil && len(entries) > 0 {
		return fmt.Errorf("repodir: %s is not empty", path)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("repodir: %w", err)
	}
	for _, d := range []string{path, filepath.Join(path, PacksDir), filepath.Join(path, TmpDir)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("repodir: %w", err)
		}
	}
	meta := Meta{Format: FormatVersion, UUID: newUUID(), Config: cfg}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(meta); err != nil {
		return fmt.Errorf("repodir: %w", err)
	}
	for name, content := range map[string][]byte{ConfigFile: buf.Bytes(), RefsFile: nil, LogFile: nil, LocksFile: nil, LockFile: nil} {
		if err := writeAtomic(path, name, content); err != nil {
			return err
		}
	}
	return nil
}

// Open locks and opens the repository directory at path. It refuses
// repositories with a newer format, and empties tmp/.
func Open(path string) (*Dir, error) {
	meta, err := ReadMeta(path)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(path, LockFile), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("repodir: %w", err)
	}
	if err := lockFile(f); err != nil {
		f.Close()
		if errors.Is(err, ErrLocked) {
			return nil, fmt.Errorf("%w: %s", ErrLocked, path)
		}
		return nil, fmt.Errorf("repodir: lock: %w", err)
	}
	d := &Dir{path: path, lock: f, meta: meta}
	if err := d.cleanTmp(); err != nil {
		d.Close()
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(path, PacksDir), 0o700); err != nil {
		d.Close()
		return nil, fmt.Errorf("repodir: %w", err)
	}
	return d, nil
}

// ReadMeta reads and validates f1repo.toml without locking the repository.
func ReadMeta(path string) (Meta, error) {
	data, err := os.ReadFile(filepath.Join(path, ConfigFile))
	if err != nil {
		return Meta{}, fmt.Errorf("repodir: %w", err)
	}
	meta, err := ParseMeta(data)
	if err != nil {
		return Meta{}, fmt.Errorf("repodir: %s: %w", path, err)
	}
	return meta, nil
}

// ParseMeta parses and validates the content of f1repo.toml. A format newer
// than FormatVersion is refused with a message naming both versions.
func ParseMeta(data []byte) (Meta, error) {
	meta := Meta{Config: policy.Default()}
	md, err := toml.Decode(string(data), &meta)
	if err != nil {
		return Meta{}, fmt.Errorf("%s: %w", ConfigFile, err)
	}
	if meta.Format > FormatVersion {
		return Meta{}, fmt.Errorf("%s has format %d; this server supports format %d, upgrade F1 to open it", ConfigFile, meta.Format, FormatVersion)
	}
	if meta.Format < 1 {
		return Meta{}, fmt.Errorf("%s: missing or invalid format", ConfigFile)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return Meta{}, fmt.Errorf("%s: unknown keys %v", ConfigFile, undecoded)
	}
	if meta.UUID == "" {
		return Meta{}, fmt.Errorf("%s: missing uuid", ConfigFile)
	}
	if err := meta.Validate(); err != nil {
		return Meta{}, fmt.Errorf("%s: %w", ConfigFile, err)
	}
	return meta, nil
}

// WriteMeta replaces f1repo.toml atomically.
func WriteMeta(path string, meta Meta) error {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(meta); err != nil {
		return fmt.Errorf("repodir: %w", err)
	}
	return writeAtomic(path, ConfigFile, buf.Bytes())
}

// NewUUID returns a random repository UUID.
func NewUUID() string { return newUUID() }

// Close releases the repository lock.
func (d *Dir) Close() error {
	if d.lock == nil {
		return nil
	}
	unlockFile(d.lock)
	err := d.lock.Close()
	d.lock = nil
	return err
}

// Path returns the repository directory.
func (d *Dir) Path() string { return d.path }

// Meta returns the repository metadata.
func (d *Dir) Meta() Meta { return d.meta }

// Join returns a path inside the repository directory.
func (d *Dir) Join(elem ...string) string {
	return filepath.Join(append([]string{d.path}, elem...)...)
}

func (d *Dir) cleanTmp() error {
	tmp := d.Join(TmpDir)
	if err := os.RemoveAll(tmp); err != nil {
		return fmt.Errorf("repodir: clean tmp: %w", err)
	}
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return fmt.Errorf("repodir: %w", err)
	}
	return nil
}

// ReadRefs reads the refs file.
func (d *Dir) ReadRefs() (map[string]hash.Hash, error) {
	return readRefs(d.Join(RefsFile))
}

func readRefs(path string) (map[string]hash.Hash, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("repodir: %w", err)
	}
	refs := map[string]hash.Hash{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; sc.Scan(); n++ {
		hexHash, name, ok := strings.Cut(sc.Text(), " ")
		if !ok {
			return nil, fmt.Errorf("repodir: refs line %d: malformed", n)
		}
		h, err := hash.Parse(hexHash)
		if err != nil {
			return nil, fmt.Errorf("repodir: refs line %d: %w", n, err)
		}
		if err := policy.ValidRefName(name); err != nil {
			return nil, fmt.Errorf("repodir: refs line %d: %w", n, err)
		}
		if _, dup := refs[name]; dup {
			return nil, fmt.Errorf("repodir: refs line %d: duplicate ref %s", n, name)
		}
		refs[name] = h
	}
	return refs, sc.Err()
}

// WriteRefs replaces the refs file atomically: write refs.tmp, fsync,
// rename, fsync the directory.
func (d *Dir) WriteRefs(refs map[string]hash.Hash) error {
	names := make([]string, 0, len(refs))
	for name := range refs {
		names = append(names, name)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	for _, name := range names {
		fmt.Fprintf(&buf, "%s %s\n", refs[name], name)
	}
	return writeAtomic(d.path, RefsFile, buf.Bytes())
}

// writeAtomic writes dir/name through a temporary file, fsyncs it, renames
// it into place and fsyncs dir.
func writeAtomic(dir, name string, content []byte) error {
	tmp := filepath.Join(dir, name+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("repodir: %w", err)
	}
	if _, err := f.Write(content); err != nil {
		f.Close()
		return fmt.Errorf("repodir: write %s: %w", name, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("repodir: sync %s: %w", name, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("repodir: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return fmt.Errorf("repodir: %w", err)
	}
	return SyncDir(dir)
}

// SyncDir fsyncs a directory.
func SyncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("repodir: %w", err)
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("repodir: sync %s: %w", dir, err)
	}
	return nil
}

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40 // version 4
	b[8] = b[8]&0x3f | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
