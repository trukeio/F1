package repo

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/trukeio/f1/internal/repodir"
)

// BackupInfo describes a backup written by Backup.
type BackupInfo struct {
	Packs   int   // live packs
	Copied  int   // packs copied (not yet in the target)
	Bytes   int64 // bytes copied
	Removed int   // packs removed from the target (no longer in the repository)
}

// Backup brings a backup directory up to date with the repository while
// writes continue (doc/plan.md, "Operations"): it holds pack deletion,
// reads f1repo.toml, refs, refs.log and locks together, lists the live
// packs, copies the packs the target lacks, then replaces the target's
// text files, and finally removes target packs that are no longer live.
// An interrupted backup is consistent: the target's refs are replaced only
// after every pack they need is there. cache.db is not backed up; the
// target is a repository directory that "f1d repo import" restores.
func (r *Repo) Backup(ctx context.Context, dst string) (BackupInfo, error) {
	var info BackupInfo
	release := r.store.Hold()
	defer release()

	texts := map[string][]byte{}
	r.refMu.Lock()
	for _, name := range backupFiles {
		b, err := os.ReadFile(r.dir.Join(name))
		if err != nil {
			r.refMu.Unlock()
			return info, fmt.Errorf("repo: backup: %w", err)
		}
		texts[name] = b
	}
	r.refMu.Unlock()
	packs, err := r.store.LivePacks(ctx)
	if err != nil {
		return info, err
	}
	info.Packs = len(packs)

	packsDir := filepath.Join(dst, repodir.PacksDir)
	if err := os.MkdirAll(packsDir, 0o700); err != nil {
		return info, fmt.Errorf("repo: backup: %w", err)
	}
	live := map[string]bool{}
	for _, p := range packs {
		if err := ctx.Err(); err != nil {
			return info, err
		}
		name := filepath.Base(p.Key)
		live[name] = true
		target := filepath.Join(packsDir, name)
		if st, err := os.Stat(target); err == nil && st.Size() == p.Size {
			continue // packs are immutable
		}
		rc, err := r.store.Backend().Get(ctx, p.Key, 0, -1)
		if err != nil {
			return info, err
		}
		n, err := writeFileAtomic(target, rc)
		rc.Close()
		if err != nil {
			return info, fmt.Errorf("repo: backup %s: %w", p.Key, err)
		}
		if n != p.Size {
			os.Remove(target)
			return info, fmt.Errorf("repo: backup %s: read %d bytes, expected %d", p.Key, n, p.Size)
		}
		info.Copied++
		info.Bytes += n
	}
	if err := syncDir(packsDir); err != nil {
		return info, err
	}
	for _, name := range backupFiles {
		if _, err := writeFileAtomic(filepath.Join(dst, name), strings.NewReader(string(texts[name]))); err != nil {
			return info, fmt.Errorf("repo: backup: %w", err)
		}
	}
	if err := syncDir(dst); err != nil {
		return info, err
	}
	entries, err := os.ReadDir(packsDir)
	if err != nil {
		return info, fmt.Errorf("repo: backup: %w", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".pack") && !live[e.Name()] {
			if err := os.Remove(filepath.Join(packsDir, e.Name())); err != nil {
				return info, fmt.Errorf("repo: backup: %w", err)
			}
			info.Removed++
		}
	}
	return info, syncDir(packsDir)
}

// backupFiles are the text files of a repository directory that a backup
// copies (with packs/).
var backupFiles = []string{repodir.ConfigFile, repodir.RefsFile, repodir.LogFile, repodir.LocksFile}

// writeFileAtomic writes r to path through a temporary file, fsyncs it and
// renames it into place.
func writeFileAtomic(path string, r io.Reader) (int64, error) {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, r)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return n, err
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
