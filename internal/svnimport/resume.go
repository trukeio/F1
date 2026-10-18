package svnimport

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
)

var (
	revisionTrailer = regexp.MustCompile(`(?m)^SVN-Revision: (\d+)$`)
	uuidTrailer     = regexp.MustCompile(`(?m)^SVN-UUID: (\S+)$`)
)

// trailers returns the SVN revision and UUID a commit message records, or
// ok == false.
func trailers(msg []byte) (rev int, uuid string, ok bool) {
	m := revisionTrailer.FindSubmatch(msg)
	if m == nil {
		return 0, "", false
	}
	rev, err := strconv.Atoi(string(m[1]))
	if err != nil {
		return 0, "", false
	}
	if u := uuidTrailer.FindSubmatch(msg); u != nil {
		uuid = string(u[1])
	}
	return rev, uuid, true
}

// MarkerRef records an import's progress: it points at a commit with an
// empty tree whose message gives the last revision imported, the SVN
// repository's UUID and the layout. It moves with the other refs at every
// checkpoint, so it always matches them. Delete it once the migration is
// over.
const MarkerRef = "notes/svn-import"

// String is the layout as the marker records it.
func (l layout) String() string {
	if l.root {
		return "root"
	}
	return fmt.Sprintf("trunk=%s branches=%s tags=%s", l.trunk, strings.Join(l.branches, ","), strings.Join(l.tags, ","))
}

var layoutTrailer = regexp.MustCompile(`(?m)^SVN-Layout: (.*)$`)

// resume prepares an incremental import: the namespace must hold an
// earlier import (its marker), with the same layout, and every other ref
// must end in a commit of that import.
func (im *importer) resume() error {
	marker, ok := im.committed[MarkerRef]
	if !ok {
		return fmt.Errorf("svnimport: the namespace is not empty and holds no SVN import (no %s ref)", MarkerRef)
	}
	c, err := im.rp.ReadCommit(im.ctx, marker)
	if err != nil {
		return err
	}
	rev, uuid, ok := trailers(c.Message)
	if !ok {
		return fmt.Errorf("svnimport: %s does not record an SVN revision", MarkerRef)
	}
	if m := layoutTrailer.FindSubmatch(c.Message); m == nil || string(m[1]) != im.lay.String() {
		was := "unknown"
		if m != nil {
			was = string(m[1])
		}
		return fmt.Errorf("svnimport: the earlier import used layout %q, not %q: pass the same --trunk, --branches, --tags or --root", was, im.lay.String())
	}
	names := make([]string, 0, len(im.committed))
	for n := range im.committed {
		if n != MarkerRef {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		c, err := im.rp.ReadCommit(im.ctx, im.committed[n])
		if err != nil {
			return err
		}
		if _, _, ok := trailers(c.Message); !ok {
			return fmt.Errorf("svnimport: ref %s does not end in an imported SVN revision (commits made in F1 since an import cannot be continued)", n)
		}
	}
	im.base, im.baseUUID, im.resumed = rev, uuid, true
	im.rep.Continued = rev
	return nil
}

// marker writes the marker commit for the last revision imported.
func (im *importer) marker() (hash.Hash, error) {
	root, err := im.tree(hash.Zero)
	if err != nil {
		return hash.Zero, err
	}
	msg := fmt.Sprintf("SVN import\n\nSVN-Revision: %d\nSVN-UUID: %s\nSVN-Layout: %s\n", im.rev, im.dr.UUID, im.lay)
	p := object.Person{Name: []byte("svn-import"), Time: im.lastTime.Unix(), TZ: "+0000"}
	return im.rp.Put(im.ctx, &object.Commit{Tree: root, Author: p, Committer: p, Message: []byte(msg)})
}

// checkContinuation checks that an incremental dump continues the
// namespace: the same repository, from the next revision.
func (im *importer) checkContinuation(first int) error {
	if im.dr.UUID != "" && im.baseUUID != "" && im.dr.UUID != im.baseUUID {
		return fmt.Errorf("svnimport: the dump is of SVN repository %s, the namespace holds %s", im.dr.UUID, im.baseUUID)
	}
	if first != im.base+1 {
		return fmt.Errorf("svnimport: the namespace holds revisions up to r%d, so the dump must start at r%d, not r%d (svnadmin dump -r %d:HEAD --incremental)",
			im.base, im.base+1, first, im.base+1)
	}
	return nil
}

// historyAt returns the commit that held the branch or tag of c at rev,
// from earlier imports. A branch deleted after rev is kept under
// svn-deleted/, named after the deleting revision; otherwise the live ref
// holds it.
func (im *importer) historyAt(c class, rev int) (hash.Hash, error) {
	name := c.refName(im.defaultBranch)
	cat, short, _ := strings.Cut(name, "/")
	prefix := cat + "/svn-deleted/" + short + "-r"
	best, bestRev := "", 0
	for n := range im.committed {
		k, err := strconv.Atoi(strings.TrimPrefix(n, prefix))
		if !strings.HasPrefix(n, prefix) || err != nil || k <= rev {
			continue
		}
		if best == "" || k < bestRev {
			best, bestRev = n, k
		}
	}
	if best == "" {
		best = name
	}
	marks, err := im.refHistory(best)
	if err != nil {
		return hash.Zero, err
	}
	h, _ := markAt(marks, rev)
	return h, nil
}

// refHistory returns the commits on a ref's first-parent history with the
// SVN revisions they record, oldest first.
func (im *importer) refHistory(name string) ([]mark, error) {
	if marks, ok := im.history[name]; ok {
		return marks, nil
	}
	var marks []mark
	for h := im.committed[name]; !h.IsZero(); {
		c, err := im.rp.ReadCommit(im.ctx, h)
		if err != nil {
			return nil, err
		}
		rev, _, ok := trailers(c.Message)
		if !ok {
			break // history from before the import
		}
		marks = append(marks, mark{rev: rev, commit: h})
		if len(c.Parents) == 0 {
			break
		}
		h = c.Parents[0]
	}
	for i, j := 0, len(marks)-1; i < j; i, j = i+1, j-1 {
		marks[i], marks[j] = marks[j], marks[i]
	}
	im.history[name] = marks
	return marks, nil
}
