// Package gc holds repository maintenance: fsck now, garbage collection in
// phase 8.
package gc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/trukeio/f1/internal/cache"
	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
	"github.com/trukeio/f1/internal/pack"
	"github.com/trukeio/f1/internal/repodir"
)

// Severity of a finding.
type Severity string

const (
	// Error means data is missing or corrupt.
	Error Severity = "error"
	// Warning means derived state (cache.db, refs recovery) is out of date
	// and will be repaired when the repository is opened.
	Warning Severity = "warning"
)

// Finding is one problem found by fsck.
type Finding struct {
	Severity Severity
	Message  string
}

func (f Finding) String() string { return string(f.Severity) + ": " + f.Message }

// Report is the result of fsck.
type Report struct {
	Packs     int
	Objects   int
	Refs      int
	Reachable int
	Findings  []Finding
}

// OK reports whether fsck found no errors (warnings are allowed).
func (r *Report) OK() bool {
	for _, f := range r.Findings {
		if f.Severity == Error {
			return false
		}
	}
	return true
}

// Clean reports whether fsck found nothing at all.
func (r *Report) Clean() bool { return len(r.Findings) == 0 }

func (r *Report) add(s Severity, format string, args ...any) {
	r.Findings = append(r.Findings, Finding{s, fmt.Sprintf(format, args...)})
}

// maxFindings bounds repeated findings, so reports of badly damaged
// repositories stay readable.
const maxFindings = 1000

// CheckDir runs fsck on the repository directory at path. It takes the
// repository lock, so it refuses to run while a server has the repository
// open. It does not repair anything: it opens cache.db read-only in effect
// (no rebuild) and does not roll refs forward.
func CheckDir(ctx context.Context, path string) (*Report, error) {
	dir, err := repodir.Open(path)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	return check(ctx, dir)
}

type objInfo struct {
	typ  object.Type
	pack string
}

func check(ctx context.Context, dir *repodir.Dir) (*Report, error) {
	rep := &Report{}
	meta := dir.Meta()
	truncating := meta.RetentionPolicy().Truncating()

	// Refs and refs.log.
	refs, err := dir.ReadRefs()
	if err != nil {
		rep.add(Error, "refs: %v", err)
		refs = map[string]hash.Hash{}
	}
	rep.Refs = len(refs)
	checkLog(dir, refs, rep)

	// Packs: checksums, payload hashes, and the set of stored objects.
	stored := map[hash.Hash]objInfo{}
	packNames, err := packFiles(dir)
	if err != nil {
		return nil, err
	}
	pending := map[string]bool{}
	cachePath := dir.Join(repodir.CacheFile)
	var db *cache.DB
	if _, err := os.Stat(cachePath); err != nil {
		rep.add(Warning, "cache.db is missing; it is rebuilt when the repository is opened")
	} else if db, err = cache.Open(cachePath); err != nil {
		rep.add(Warning, "cache.db cannot be used (%v); it is rebuilt when the repository is opened", err)
		db = nil
	} else {
		defer db.Close()
		if pending, err = db.PendingDeletes(ctx); err != nil {
			return nil, err
		}
	}
	footers := map[string][]pack.IndexEntry{}
	for _, name := range packNames {
		rep.Packs++
		entries, err := verifyPack(dir, name)
		if err != nil {
			rep.add(Error, "pack %s: %v", name, err)
			continue
		}
		footers[name] = entries
		if pending[name] {
			continue // replaced; its objects live in other packs
		}
		for _, e := range entries {
			if _, ok := stored[e.Hash]; !ok {
				stored[e.Hash] = objInfo{typ: e.Type, pack: name}
			}
		}
	}
	rep.Objects = len(stored)

	if db != nil {
		checkCache(ctx, db, footers, pending, stored, rep)
	}

	// D9 for every stored object, then reachability from the refs.
	objs := map[hash.Hash]object.Object{}
	readObjects(dir, footers, pending, func(h hash.Hash, o object.Object) { objs[h] = o }, rep)
	checkD9(objs, stored, truncating, rep)
	checkReachable(objs, refs, stored, truncating, rep)
	return rep, nil
}

func packFiles(dir *repodir.Dir) ([]string, error) {
	entries, err := os.ReadDir(dir.Join(repodir.PacksDir))
	if err != nil {
		return nil, fmt.Errorf("fsck: %w", err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".pack") && !e.IsDir() {
			names = append(names, strings.TrimSuffix(e.Name(), ".pack"))
		}
	}
	sort.Strings(names)
	return names, nil
}

func openPack(dir *repodir.Dir, name string) (*pack.Reader, *os.File, error) {
	f, err := os.Open(filepath.Join(dir.Join(repodir.PacksDir), name+".pack"))
	if err != nil {
		return nil, nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	p, err := pack.Open(f, st.Size())
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return p, f, nil
}

func verifyPack(dir *repodir.Dir, name string) ([]pack.IndexEntry, error) {
	p, f, err := openPack(dir, name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := p.Verify(); err != nil {
		return nil, err
	}
	return p.Entries(), nil
}

// checkLog compares refs with the last complete refs.log group.
func checkLog(dir *repodir.Dir, refs map[string]hash.Hash, rep *Report) {
	var last []repodir.LogRecord
	err := dir.ReadLog(func(r repodir.LogRecord) error {
		if len(last) > 0 && last[0].Tx != r.Tx {
			last = last[:0]
		}
		last = append(last, r)
		return nil
	})
	if err != nil {
		rep.add(Warning, "refs.log: %v (an incomplete trailing update is removed when the repository is opened)", err)
		return
	}
	if len(last) == 0 {
		if len(refs) > 0 {
			rep.add(Warning, "refs.log is empty but refs has %d refs", len(refs))
		}
		return
	}
	allNew := true
	for _, r := range last {
		allNew = allNew && refs[r.Ref] == r.New
	}
	if !allNew {
		rep.add(Warning, "refs does not reflect the last refs.log update %s; it is applied when the repository is opened", last[0].Tx)
	}
}

func checkCache(ctx context.Context, db *cache.DB, footers map[string][]pack.IndexEntry, pending map[string]bool, stored map[hash.Hash]objInfo, rep *Report) {
	indexed, err := db.Packs(ctx)
	if err != nil {
		rep.add(Warning, "cache.db: %v", err)
		return
	}
	isIndexed := map[string]bool{}
	for _, p := range indexed {
		isIndexed[p.Name] = true
		if _, ok := footers[p.Name]; !ok {
			rep.add(Warning, "cache.db indexes pack %s, which is missing or unreadable", p.Name)
		}
	}
	for name := range footers {
		if !isIndexed[name] && !pending[name] {
			rep.add(Warning, "pack %s is not indexed in cache.db", name)
		}
	}
	n := 0
	for h, info := range stored {
		loc, ok, err := db.Lookup(ctx, h)
		if err != nil {
			rep.add(Warning, "cache.db: %v", err)
			return
		}
		if !ok {
			if n++; n <= maxFindings {
				rep.add(Warning, "cache.db does not index %s %s", info.typ, h.Short())
			}
			continue
		}
		found := false
		for _, e := range footers[loc.Pack] {
			if e == loc.Entry {
				found = true
				break
			}
		}
		if !found {
			if n++; n <= maxFindings {
				rep.add(Warning, "cache.db location of %s does not match pack %s", h.Short(), loc.Pack)
			}
		}
	}
	if count, err := db.CountObjects(ctx); err == nil && count != len(stored) {
		rep.add(Warning, "cache.db indexes %d objects, packs hold %d", count, len(stored))
	}
}

// readObjects decodes every non-chunk object of the live packs.
func readObjects(dir *repodir.Dir, footers map[string][]pack.IndexEntry, pending map[string]bool, fn func(hash.Hash, object.Object), rep *Report) {
	names := make([]string, 0, len(footers))
	for name := range footers {
		if !pending[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		p, f, err := openPack(dir, name)
		if err != nil {
			rep.add(Error, "pack %s: %v", name, err)
			continue
		}
		for _, e := range footers[name] {
			if e.Type == object.TypeChunk {
				continue
			}
			_, payload, err := p.Get(e.Hash)
			if err != nil {
				rep.add(Error, "pack %s: %v", name, err)
				continue
			}
			o, err := object.Decode(e.Type, payload)
			if err != nil {
				rep.add(Error, "%s %s: %v", e.Type, e.Hash.Short(), err)
				continue
			}
			fn(e.Hash, o)
		}
		f.Close()
	}
}

func checkD9(objs map[hash.Hash]object.Object, stored map[hash.Hash]objInfo, truncating bool, rep *Report) {
	n := 0
	for _, h := range sortedHashes(objs) {
		o := objs[h]
		var parents map[hash.Hash]bool
		if c, ok := o.(*object.Commit); ok && truncating {
			parents = map[hash.Hash]bool{}
			for _, p := range c.Parents {
				parents[p] = true
			}
		}
		for _, ref := range object.References(o) {
			if _, ok := stored[ref]; ok || parents[ref] {
				continue
			}
			if n++; n <= maxFindings {
				rep.add(Error, "D9: %s %s references missing object %s", o.Type(), h.Short(), ref.Short())
			}
		}
	}
}

func sortedHashes(m map[hash.Hash]object.Object) []hash.Hash {
	out := make([]hash.Hash, 0, len(m))
	for h := range m {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return hash.Compare(out[i], out[j]) < 0 })
	return out
}

func checkReachable(objs map[hash.Hash]object.Object, refs map[string]hash.Hash, stored map[hash.Hash]objInfo, truncating bool, rep *Report) {
	for _, name := range sortedKeys(refs) {
		h := refs[name]
		info, ok := stored[h]
		switch {
		case !ok:
			rep.add(Error, "ref %s points to missing object %s", name, h.Short())
		case info.typ != object.TypeCommit && info.typ != object.TypeTag:
			rep.add(Error, "ref %s points to a %s", name, info.typ)
		}
	}
	// Objects referenced from refs, transitively. D9 already guarantees
	// that references exist; this counts what is reachable and catches
	// commits whose parents are missing outside truncating retention.
	seen := map[hash.Hash]bool{}
	var stack []hash.Hash
	for _, h := range refs {
		stack = append(stack, h)
	}
	for len(stack) > 0 {
		h := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[h] {
			continue
		}
		seen[h] = true
		if _, ok := stored[h]; !ok {
			continue // reported by D9 or the ref check
		}
		rep.Reachable++
		o, ok := objs[h]
		if !ok {
			continue // a chunk
		}
		refsOf := object.References(o)
		if c, isCommit := o.(*object.Commit); isCommit && truncating {
			refsOf = append([]hash.Hash{c.Tree}, presentParents(c, stored)...)
			if c.GitTreeRaw != nil {
				refsOf = append(refsOf, *c.GitTreeRaw)
			}
		}
		stack = append(stack, refsOf...)
	}
}

func presentParents(c *object.Commit, stored map[hash.Hash]objInfo) []hash.Hash {
	var out []hash.Hash
	for _, p := range c.Parents {
		if _, ok := stored[p]; ok {
			out = append(out, p)
		}
	}
	return out
}

func sortedKeys(m map[string]hash.Hash) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ErrNotClean is returned by callers that require a clean report.
var ErrNotClean = errors.New("fsck: repository has errors")
