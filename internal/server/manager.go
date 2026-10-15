package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/trukeio/f1/internal/gc"
	"github.com/trukeio/f1/internal/policy"
	"github.com/trukeio/f1/internal/repo"
	"github.com/trukeio/f1/internal/repodir"
	"github.com/trukeio/f1/internal/serverdb"
)

// Errors returned by the manager.
var (
	ErrNoRepo     = errors.New("server: no such repository")
	ErrRepoExists = errors.New("server: repository already exists")
)

// Manager owns the open repositories of a running server: one repo.Repo per
// registered directory under <data_dir>/repos.
type Manager struct {
	reposDir string
	db       *serverdb.DB
	log      *slog.Logger
	opts     repo.Options

	mu    sync.Mutex
	repos map[string]*repo.Repo
}

// OpenManager opens every registered repository. Repository directories
// found in repos/ that are not registered (placed there while the server was
// stopped) are registered. Registered repositories without a directory, and
// duplicate UUIDs, are logged. A repository that fails to open is logged
// and left closed, so that one broken repository does not stop the server.
func OpenManager(ctx context.Context, dataDir string, db *serverdb.DB, log *slog.Logger, opts repo.Options) (*Manager, error) {
	if opts.Logger == nil {
		opts.Logger = log
	}
	m := &Manager{reposDir: filepath.Join(dataDir, ReposDir), db: db, log: log, opts: opts, repos: map[string]*repo.Repo{}}
	registered, err := db.Repositories(ctx)
	if err != nil {
		return nil, err
	}
	isRegistered := map[string]bool{}
	for _, r := range registered {
		isRegistered[r.Name] = true
	}
	entries, err := os.ReadDir(m.reposDir)
	if err != nil {
		return nil, fmt.Errorf("server: %w", err)
	}
	onDisk := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		if err := policy.ValidNamespace(name); err != nil {
			log.Warn("ignoring directory in repos/", "dir", name, "err", err)
			continue
		}
		meta, err := repodir.ReadMeta(filepath.Join(m.reposDir, name))
		if err != nil {
			log.Error("cannot read repository", "namespace", name, "err", err)
			continue
		}
		onDisk[name] = true
		if !isRegistered[name] {
			if err := db.Register(ctx, name, meta.UUID); err != nil {
				return nil, err
			}
			log.Info("registered repository found in repos/", "namespace", name, "uuid", meta.UUID)
		}
		if err := m.open(ctx, name); err != nil {
			log.Error("cannot open repository", "namespace", name, "err", err)
		}
	}
	for _, r := range registered {
		if !onDisk[r.Name] {
			log.Warn("registered repository has no directory", "namespace", r.Name)
		}
	}
	m.warnDuplicateUUIDs()
	return m, nil
}

func (m *Manager) dir(ns string) string { return filepath.Join(m.reposDir, ns) }

func (m *Manager) open(ctx context.Context, ns string) error {
	r, rep, err := repo.Open(ctx, m.dir(ns), m.opts)
	if err != nil {
		return err
	}
	if rep.Store.Rebuilt || len(rep.Store.Indexed) > 0 || rep.Recovery.RolledForward > 0 || rep.Recovery.TruncatedBytes > 0 {
		m.log.Info("repository recovered on open", "namespace", ns,
			"cache_rebuilt", rep.Store.Rebuilt, "packs_indexed", len(rep.Store.Indexed),
			"refs_rolled_forward", rep.Recovery.RolledForward, "log_bytes_truncated", rep.Recovery.TruncatedBytes)
	}
	m.mu.Lock()
	m.repos[ns] = r
	m.mu.Unlock()
	return nil
}

func (m *Manager) warnDuplicateUUIDs() {
	m.mu.Lock()
	defer m.mu.Unlock()
	byUUID := map[string][]string{}
	for ns, r := range m.repos {
		byUUID[r.Meta().UUID] = append(byUUID[r.Meta().UUID], ns)
	}
	for uuid, names := range byUUID {
		if len(names) > 1 {
			sort.Strings(names)
			m.log.Warn("repositories share a UUID (copied under another name); use import --new-uuid to give a copy its own", "uuid", uuid, "namespaces", names)
		}
	}
}

// Get returns an open repository.
func (m *Manager) Get(ns string) (*repo.Repo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.repos[ns]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoRepo, ns)
	}
	return r, nil
}

// RepoStatus describes an open repository.
type RepoStatus struct {
	Name   string `json:"name"`
	UUID   string `json:"uuid"`
	Frozen bool   `json:"frozen"`
}

// List returns the open repositories in name order.
func (m *Manager) List() []RepoStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]RepoStatus, 0, len(m.repos))
	for ns, r := range m.repos {
		out = append(out, RepoStatus{Name: ns, UUID: r.Meta().UUID, Frozen: r.Frozen()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Names returns the names of the open repositories in order.
func (m *Manager) Names() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.repos))
	for ns := range m.repos {
		out = append(out, ns)
	}
	sort.Strings(out)
	return out
}

// Attach registers and opens the repository directory repos/<ns>.
func (m *Manager) Attach(ctx context.Context, ns string) error {
	if err := policy.ValidNamespace(ns); err != nil {
		return err
	}
	m.mu.Lock()
	_, open := m.repos[ns]
	m.mu.Unlock()
	if open {
		return fmt.Errorf("%w: %s is already attached", ErrRepoExists, ns)
	}
	meta, err := repodir.ReadMeta(m.dir(ns))
	if err != nil {
		return err
	}
	if err := m.db.Register(ctx, ns, meta.UUID); err != nil && !errors.Is(err, serverdb.ErrExists) {
		return err
	}
	if err := m.open(ctx, ns); err != nil {
		return err
	}
	m.log.Info("repository attached", "namespace", ns, "uuid", meta.UUID)
	m.warnDuplicateUUIDs()
	return nil
}

// Create creates, registers and opens a new namespace.
func (m *Manager) Create(ctx context.Context, ns string, cfg policy.Config) (*repo.Repo, error) {
	if err := policy.ValidNamespace(ns); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	dir := m.dir(ns)
	if _, err := os.Stat(dir); err == nil {
		return nil, fmt.Errorf("%w: %s", ErrRepoExists, ns)
	}
	if err := repo.Create(dir, cfg); err != nil {
		return nil, err
	}
	if err := m.Attach(ctx, ns); err != nil {
		return nil, err
	}
	return m.Get(ns)
}

// Detach closes and unregisters a repository, leaving its directory
// consistent for moving: the open pack is sealed and tmp/ is emptied.
func (m *Manager) Detach(ctx context.Context, ns string) error {
	m.mu.Lock()
	r, ok := m.repos[ns]
	if ok {
		delete(m.repos, ns)
	}
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoRepo, ns)
	}
	if err := r.Close(ctx); err != nil {
		return err
	}
	if err := DetachOffline(ctx, m.db, m.reposDir, ns); err != nil {
		return err
	}
	m.log.Info("repository detached", "namespace", ns)
	return nil
}

// DetachOffline empties tmp/ of a closed repository and unregisters it.
func DetachOffline(ctx context.Context, db *serverdb.DB, reposDir, ns string) error {
	tmp := filepath.Join(reposDir, ns, repodir.TmpDir)
	if err := os.RemoveAll(tmp); err != nil {
		return fmt.Errorf("server: %w", err)
	}
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return fmt.Errorf("server: %w", err)
	}
	return db.Unregister(ctx, ns)
}

// DeletedPrefix starts the names of deleted repository directories in
// repos/; the manager ignores them (names starting with "." are never
// namespaces).
const DeletedPrefix = ".deleted-"

// Delete detaches a namespace and moves its directory aside to
// repos/.deleted-<ns>-<suffix>, so that its name can be reused. Nothing is
// erased: the operator removes the directory, or restores it with a rename
// and f1d repo attach (S3 DeleteBucket, which only deletes empty buckets).
func (m *Manager) Delete(ctx context.Context, ns string) error {
	if err := m.Detach(ctx, ns); err != nil {
		return err
	}
	to := filepath.Join(m.reposDir, DeletedPrefix+ns+"-"+randomSuffix())
	if err := os.Rename(m.dir(ns), to); err != nil {
		return fmt.Errorf("server: %w", err)
	}
	if err := repodir.SyncDir(m.reposDir); err != nil {
		return err
	}
	m.log.Info("repository deleted", "namespace", ns, "moved_to", to)
	return nil
}

// Freeze freezes a repository for copying.
func (m *Manager) Freeze(ctx context.Context, ns string) error {
	r, err := m.Get(ns)
	if err != nil {
		return err
	}
	return r.Freeze(ctx)
}

// Unfreeze unfreezes a repository.
func (m *Manager) Unfreeze(ns string) error {
	r, err := m.Get(ns)
	if err != nil {
		return err
	}
	r.Unfreeze()
	return nil
}

// Archive writes a repository archive to w (see repo.Repo.Archive).
func (m *Manager) Archive(ctx context.Context, ns string, w io.Writer) (repo.ArchiveInfo, error) {
	r, err := m.Get(ns)
	if err != nil {
		return repo.ArchiveInfo{}, err
	}
	return r.Archive(ctx, w, ns)
}

// ArchiveToFile writes a repository archive to path atomically.
func ArchiveToFile(ctx context.Context, path string, archive func(io.Writer) (repo.ArchiveInfo, error)) (repo.ArchiveInfo, error) {
	tmp := path + ".tmp-" + randomSuffix()
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return repo.ArchiveInfo{}, fmt.Errorf("server: %w", err)
	}
	info, err := archive(f)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
		return repo.ArchiveInfo{}, err
	}
	return info, nil
}

// GC collects garbage in a repository (see gc.Run).
func (m *Manager) GC(ctx context.Context, ns string, opts gc.Options) (*gc.Result, error) {
	r, err := m.Get(ns)
	if err != nil {
		return nil, err
	}
	return gc.Run(ctx, r, opts)
}

// RunGC collects garbage in every repository every interval, one
// repository at a time, until ctx is done.
func (m *Manager) RunGC(ctx context.Context, interval, grace time.Duration) {
	if interval <= 0 {
		return
	}
	if grace == 0 {
		grace = -1 // an explicit zero grace period
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for _, ns := range m.Names() {
			if ctx.Err() != nil {
				return
			}
			res, err := m.GC(ctx, ns, gc.Options{Grace: grace})
			switch {
			case err != nil:
				m.log.Error("garbage collection failed", "namespace", ns, "err", err)
			case res.Skipped != "":
				m.log.Info("garbage collection skipped", "namespace", ns, "reason", res.Skipped)
			default:
				m.log.Info("garbage collection", "namespace", ns, "deleted_objects", res.DeletedObjects,
					"deleted_bytes", res.DeletedBytes, "packs_deleted", len(res.PacksDeleted), "packs_rewritten", len(res.PacksRewritten), "duration", res.Duration)
			}
		}
	}
}

// Close closes every open repository.
func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	repos := m.repos
	m.repos = map[string]*repo.Repo{}
	m.mu.Unlock()
	var errs []error
	for ns, r := range repos {
		if err := r.Close(ctx); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", ns, err))
		}
	}
	return errors.Join(errs...)
}

// ImportOptions control ImportRepo.
type ImportOptions struct {
	Name    string // namespace; default: the archive root or source directory name
	NewUUID bool   // give the imported repository a new UUID
}

// ImportResult describes an imported repository.
type ImportResult struct {
	Name    string
	UUID    string
	Files   int
	Skipped []string
	Fsck    *gc.Report
}

// ImportRepo copies a repository directory, or extracts a repository ZIP,
// into reposDir/<name>. It rebuilds cache.db, runs fsck, and moves the
// result into place only if fsck finds no errors. It does not register the
// repository; the caller registers it offline, or asks a running server to
// attach it.
func ImportRepo(ctx context.Context, reposDir, src string, opts ImportOptions) (ImportResult, error) {
	var res ImportResult
	st, err := os.Stat(src)
	if err != nil {
		return res, fmt.Errorf("server: import: %w", err)
	}
	if err := os.MkdirAll(reposDir, 0o700); err != nil {
		return res, fmt.Errorf("server: %w", err)
	}
	staging := filepath.Join(reposDir, ".import-"+randomSuffix())
	defer os.RemoveAll(staging)

	var imp repodir.Imported
	if st.IsDir() {
		imp, err = repodir.CopyDir(src, staging)
	} else {
		imp, err = repodir.ExtractZip(src, staging)
	}
	if err != nil {
		return res, err
	}
	res.Name = opts.Name
	if res.Name == "" {
		res.Name = imp.Root
	}
	if err := policy.ValidNamespace(res.Name); err != nil {
		return res, fmt.Errorf("server: import: %w (use --name)", err)
	}
	target := filepath.Join(reposDir, res.Name)
	if _, err := os.Stat(target); err == nil {
		return res, fmt.Errorf("%w: %s", ErrRepoExists, res.Name)
	}
	meta := imp.Meta
	if opts.NewUUID {
		meta.UUID = repodir.NewUUID()
		if err := repodir.WriteMeta(staging, meta); err != nil {
			return res, err
		}
	}
	res.UUID, res.Files, res.Skipped = meta.UUID, imp.Files, imp.Skipped

	// Open once: builds cache.db and applies refs.log recovery.
	r, _, err := repo.Open(ctx, staging, repo.Options{})
	if err != nil {
		return res, fmt.Errorf("server: import: %w", err)
	}
	if err := r.Close(ctx); err != nil {
		return res, err
	}
	if res.Fsck, err = gc.CheckDir(ctx, staging); err != nil {
		return res, err
	}
	if !res.Fsck.OK() {
		return res, fmt.Errorf("server: import: fsck found errors: %v", res.Fsck.Findings)
	}
	if err := os.Rename(staging, target); err != nil {
		return res, fmt.Errorf("server: import: %w", err)
	}
	return res, repodir.SyncDir(reposDir)
}

func randomSuffix() string {
	var b [6]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
