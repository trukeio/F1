package repodir

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Repository state files that an import takes; everything else in a
// copied directory or archive (cache.db, lock, tmp/, editor leftovers) is
// derived, local or unknown and is skipped.
var stateFiles = map[string]bool{ConfigFile: true, RefsFile: true, LogFile: true, LocksFile: true}

// isStateFile reports whether rel (slash-separated, relative to the
// repository root) is part of the repository state.
func isStateFile(rel string) bool {
	if stateFiles[rel] {
		return true
	}
	dir, name := path.Split(rel)
	return dir == PacksDir+"/" && strings.HasSuffix(name, ".pack") && !strings.HasPrefix(name, ".")
}

// Imported describes a repository directory produced by ExtractZip or
// CopyDir.
type Imported struct {
	Root    string   // the archive root directory name, or the source directory name
	Meta    Meta     // f1repo.toml of the imported repository
	Files   int      // state files written
	Skipped []string // entries that are not repository state
}

// ExtractZip extracts a repository archive into dest, which must not exist.
// All entries must be below one root directory. Only repository state
// files are extracted; the format version is checked before anything is
// written. Symbolic links, absolute paths and ".." are rejected.
func ExtractZip(zipPath, dest string) (Imported, error) {
	var imp Imported
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return imp, fmt.Errorf("repodir: %w", err)
	}
	defer zr.Close()

	files := map[string]*zip.File{}
	for _, f := range zr.File {
		name := f.Name
		if strings.Contains(name, `\`) || path.IsAbs(name) {
			return imp, fmt.Errorf("repodir: archive entry %q has an invalid path", name)
		}
		clean := path.Clean(name)
		if clean == ".." || strings.HasPrefix(clean, "../") || clean == "." {
			return imp, fmt.Errorf("repodir: archive entry %q escapes the archive root", name)
		}
		root, rel, _ := strings.Cut(clean, "/")
		if imp.Root == "" {
			imp.Root = root
		} else if root != imp.Root {
			return imp, fmt.Errorf("repodir: archive has more than one root directory (%q and %q)", imp.Root, root)
		}
		mode := f.Mode()
		if mode.IsDir() || rel == "" {
			continue
		}
		if !mode.IsRegular() {
			return imp, fmt.Errorf("repodir: archive entry %q is not a regular file", name)
		}
		if !isStateFile(rel) {
			imp.Skipped = append(imp.Skipped, rel)
			continue
		}
		if _, dup := files[rel]; dup {
			return imp, fmt.Errorf("repodir: archive has %q twice", rel)
		}
		files[rel] = f
	}
	cfg, ok := files[ConfigFile]
	if !ok {
		return imp, fmt.Errorf("repodir: archive has no %s/%s; it is not an F1 repository", imp.Root, ConfigFile)
	}
	data, err := readZipFile(cfg, 1<<20)
	if err != nil {
		return imp, err
	}
	if imp.Meta, err = ParseMeta(data); err != nil {
		return imp, fmt.Errorf("repodir: archive %s: %w", zipPath, err)
	}
	if _, ok := files[RefsFile]; !ok {
		return imp, fmt.Errorf("repodir: archive has no %s", RefsFile)
	}

	if err := makeRepoDirs(dest); err != nil {
		return imp, err
	}
	rels := make([]string, 0, len(files))
	for rel := range files {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		rc, err := files[rel].Open()
		if err != nil {
			return imp, fmt.Errorf("repodir: %s: %w", rel, err)
		}
		err = writeFile(filepath.Join(dest, filepath.FromSlash(rel)), rc, int64(files[rel].UncompressedSize64))
		rc.Close()
		if err != nil {
			return imp, err
		}
		imp.Files++
	}
	return imp, finishImport(dest)
}

// CopyDir copies the repository state files of the repository directory src
// into dest, which must not exist.
func CopyDir(src, dest string) (Imported, error) {
	imp := Imported{Root: filepath.Base(filepath.Clean(src))}
	var err error
	if imp.Meta, err = ReadMeta(src); err != nil {
		return imp, err
	}
	var rels []string
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == TmpDir {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("repodir: %s is a symbolic link", p)
		}
		if isStateFile(rel) {
			rels = append(rels, rel)
		} else {
			imp.Skipped = append(imp.Skipped, rel)
		}
		return nil
	})
	if err != nil {
		return imp, err
	}
	if err := makeRepoDirs(dest); err != nil {
		return imp, err
	}
	for _, rel := range rels {
		f, err := os.Open(filepath.Join(src, filepath.FromSlash(rel)))
		if err != nil {
			return imp, fmt.Errorf("repodir: %w", err)
		}
		st, err := f.Stat()
		if err == nil {
			err = writeFile(filepath.Join(dest, filepath.FromSlash(rel)), f, st.Size())
		}
		f.Close()
		if err != nil {
			return imp, err
		}
		imp.Files++
	}
	return imp, finishImport(dest)
}

func makeRepoDirs(dest string) error {
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("repodir: %s already exists", dest)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("repodir: %w", err)
	}
	for _, d := range []string{dest, filepath.Join(dest, PacksDir), filepath.Join(dest, TmpDir)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("repodir: %w", err)
		}
	}
	return nil
}

// finishImport creates missing optional files and fsyncs the directories.
func finishImport(dest string) error {
	for _, name := range []string{LogFile, LocksFile, LockFile} {
		p := filepath.Join(dest, name)
		if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
			if err := writeAtomic(dest, name, nil); err != nil {
				return err
			}
		}
	}
	if err := SyncDir(filepath.Join(dest, PacksDir)); err != nil {
		return err
	}
	return SyncDir(dest)
}

// writeFile writes exactly size bytes from r to path and fsyncs it.
func writeFile(path string, r io.Reader, size int64) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("repodir: %w", err)
	}
	n, err := io.Copy(f, io.LimitReader(r, size+1))
	if err == nil && n != size {
		err = fmt.Errorf("repodir: %s: got %d bytes, expected %d", path, n, size)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func readZipFile(f *zip.File, limit int64) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("repodir: %s: %w", f.Name, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, fmt.Errorf("repodir: %s: %w", f.Name, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("repodir: %s is too large", f.Name)
	}
	return data, nil
}
