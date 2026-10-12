package repo

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"time"

	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/repodir"
)

// ArchiveInfo describes an archive written by Archive.
type ArchiveInfo struct {
	Refs  map[string]hash.Hash // the refs captured in the archive
	Packs int
	Bytes int64 // pack bytes
}

// Archive writes the repository as a ZIP to w while writes continue: it
// holds pack deletion, reads refs and refs.log together (so they agree),
// then lists the live packs. Every object reachable from the captured refs
// is in a pack sealed before the refs were written, so it is in the
// listing; packs sealed later are not included and not needed (doc/plan.md,
// "Copy and archive procedures").
//
// Layout: every entry is below root/. Text files are deflated; pack files
// are stored, as they are already compressed. ZIP64 is used automatically
// for large archives. cache.db, lock and tmp/ are not included.
func (r *Repo) Archive(ctx context.Context, w io.Writer, root string) (ArchiveInfo, error) {
	var info ArchiveInfo
	release := r.store.Hold()
	defer release()

	texts := map[string][]byte{}
	r.refMu.Lock()
	for _, name := range []string{repodir.ConfigFile, repodir.RefsFile, repodir.LogFile, repodir.LocksFile} {
		b, err := os.ReadFile(r.dir.Join(name))
		if err != nil {
			r.refMu.Unlock()
			return info, fmt.Errorf("repo: archive: %w", err)
		}
		texts[name] = b
	}
	info.Refs = make(map[string]hash.Hash, len(r.refs))
	for k, v := range r.refs {
		info.Refs[k] = v
	}
	r.refMu.Unlock()

	packs, err := r.store.LivePacks(ctx)
	if err != nil {
		return info, err
	}

	zw := zip.NewWriter(w)
	now := time.Now()
	for _, name := range []string{repodir.ConfigFile, repodir.RefsFile, repodir.LogFile, repodir.LocksFile} {
		fw, err := zw.CreateHeader(&zip.FileHeader{Name: path.Join(root, name), Method: zip.Deflate, Modified: now})
		if err != nil {
			return info, err
		}
		if _, err := fw.Write(texts[name]); err != nil {
			return info, err
		}
	}
	if _, err := zw.CreateHeader(&zip.FileHeader{Name: path.Join(root, repodir.PacksDir) + "/", Modified: now}); err != nil {
		return info, err
	}
	for _, p := range packs {
		if err := ctx.Err(); err != nil {
			return info, err
		}
		fw, err := zw.CreateHeader(&zip.FileHeader{
			Name:               path.Join(root, p.Key),
			Method:             zip.Store,
			Modified:           now,
			UncompressedSize64: uint64(p.Size),
		})
		if err != nil {
			return info, err
		}
		rc, err := r.store.Backend().Get(ctx, p.Key, 0, -1)
		if err != nil {
			return info, err
		}
		n, err := io.Copy(fw, rc)
		rc.Close()
		if err != nil {
			return info, fmt.Errorf("repo: archive %s: %w", p.Key, err)
		}
		if n != p.Size {
			return info, fmt.Errorf("repo: archive %s: read %d bytes, expected %d", p.Key, n, p.Size)
		}
		info.Packs++
		info.Bytes += n
	}
	return info, zw.Close()
}
