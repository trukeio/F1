package native

import (
	"bufio"
	"net/http"
	"sort"
	"strings"

	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
	"github.com/trukeio/f1/internal/pack"
	"github.com/trukeio/f1/internal/repo"
	"github.com/trukeio/f1/internal/serverdb"
	api "github.com/trukeio/f1/pkg/f1client"
)

// maxHashes bounds the hash lists of objects/missing and objects/fetch.
const maxHashes = 100000

func sortRefs(refs []api.Ref) {
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
}

func parseHashes(list []string) ([]hash.Hash, error) {
	if len(list) > maxHashes {
		return nil, badRequest("at most %d hashes per request", maxHashes)
	}
	out := make([]hash.Hash, len(list))
	for i, s := range list {
		h, err := hash.Parse(s)
		if err != nil {
			return nil, badRequest("%v", err)
		}
		out[i] = h
	}
	return out, nil
}

// Every object endpoint addresses content by hash and needs read
// permission on the whole namespace (D13).

func (s *Server) missingObjects(w http.ResponseWriter, r *http.Request) error {
	rp, _, g, err := s.ns(r)
	if err != nil {
		return err
	}
	if err := need(g.Whole(serverdb.PermRead)); err != nil {
		return err
	}
	var req api.HashList
	if err := readJSON(r, &req); err != nil {
		return err
	}
	hashes, err := parseHashes(req.Hashes)
	if err != nil {
		return err
	}
	out := api.MissingResponse{Missing: []string{}}
	for _, h := range hashes {
		ok, err := rp.Store().Has(r.Context(), h)
		if err != nil {
			return err
		}
		if !ok {
			out.Missing = append(out.Missing, h.String())
		}
	}
	return writeJSON(w, out)
}

// uploadObjects stores a pack stream. Every payload is verified against its
// hash, and every object's references must be stored first (D9: send
// objects in dependency order). The objects are durable when the response
// is sent.
//
// DigestRecords are refused: they assert a File's Git, LFS and S3 digests,
// which other protocols trust (D21), so only the server writes them. Files
// committed by POST /commits get theirs in the background.
func (s *Server) uploadObjects(w http.ResponseWriter, r *http.Request) error {
	rp, _, g, err := s.ns(r)
	if err != nil {
		return err
	}
	if err := need(g.Whole(serverdb.PermRead) && g.Any(serverdb.PermWrite)); err != nil {
		return err
	}
	stored := 0
	err = pack.Scan(r.Body, func(rec pack.Record) error {
		if rec.Type == object.TypeDigestRecord {
			return badRequest("digest records are computed by the server; do not upload them")
		}
		if _, err := rec.Payload(nil); err != nil {
			return badRequest("%v", err)
		}
		ok, err := rp.Store().Has(r.Context(), rec.Hash)
		if err != nil || ok {
			return err
		}
		if err := rp.Store().Put(r.Context(), rec); err != nil {
			return err
		}
		stored++
		return nil
	})
	if err != nil {
		return err
	}
	if err := rp.Store().Flush(r.Context()); err != nil {
		return err
	}
	return writeJSON(w, api.UploadResult{Stored: stored})
}

func (s *Server) getObject(w http.ResponseWriter, r *http.Request) error {
	rp, _, g, err := s.ns(r)
	if err != nil {
		return err
	}
	if err := need(g.Whole(serverdb.PermRead)); err != nil {
		return err
	}
	h, err := hash.Parse(r.PathValue("hash"))
	if err != nil {
		return badRequest("%v", err)
	}
	rec, err := rp.Store().Get(r.Context(), h)
	if err != nil {
		return err
	}
	// Objects are immutable (ADR-0016): a cached copy is always current
	// while the object is stored.
	etag := `"` + h.String() + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", cacheImmutable)
	if etagMatch(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return nil
	}
	payload, err := rec.Payload(nil)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("F1-Type", rec.Type.String())
	_, err = w.Write(payload)
	return err
}

// etagMatch reports whether an If-None-Match header matches etag.
func etagMatch(header, etag string) bool {
	for _, t := range strings.Split(header, ",") {
		t = strings.TrimSpace(t)
		if t == "*" || strings.TrimPrefix(t, "W/") == etag {
			return true
		}
	}
	return false
}

// fetchObjects streams the requested objects as a pack stream, in the
// requested order, in their stored (possibly compressed) form.
func (s *Server) fetchObjects(w http.ResponseWriter, r *http.Request) error {
	rp, _, g, err := s.ns(r)
	if err != nil {
		return err
	}
	if err := need(g.Whole(serverdb.PermRead)); err != nil {
		return err
	}
	var req api.HashList
	if err := readJSON(r, &req); err != nil {
		return err
	}
	hashes, err := parseHashes(req.Hashes)
	if err != nil {
		return err
	}
	// Check that every object exists before the response starts.
	for _, h := range hashes {
		ok, err := rp.Store().Has(r.Context(), h)
		if err != nil {
			return err
		}
		if !ok {
			return newErr(http.StatusNotFound, api.CodeNotFound, "object %s not found", h)
		}
	}
	w.Header().Set("Content-Type", "application/x-f1-pack")
	bw := bufio.NewWriterSize(w, 1<<20)
	pw, err := pack.NewWriter(bw)
	if err != nil {
		return err
	}
	for _, h := range hashes {
		rec, err := rp.Store().Get(r.Context(), h)
		if err != nil {
			s.Log.Error("fetch: object vanished", "hash", h, "err", err)
			return nil // the response is truncated; the client sees a bad pack
		}
		if _, err := pw.Add(rec); err != nil {
			return nil
		}
	}
	if _, err := pw.Finish(); err != nil {
		return nil
	}
	bw.Flush()
	return nil
}

// commit creates a commit from uploaded objects. Its changes name objects
// by hash, so it needs whole-namespace read (D13), and write on every
// changed file.
func (s *Server) commit(w http.ResponseWriter, r *http.Request) error {
	rp, c, g, err := s.ns(r)
	if err != nil {
		return err
	}
	if err := need(g.Whole(serverdb.PermRead)); err != nil {
		return err
	}
	var req api.CommitRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if req.Ref == "" {
		return badRequest("ref is required")
	}
	parent, err := parseHash(req.Parent)
	if err != nil {
		return err
	}
	var changes []repo.Change
	for _, ch := range req.Changes {
		switch ch.Op {
		case "put":
			e, err := entryFrom(ch.Entry)
			if err != nil {
				return err
			}
			changes = append(changes, repo.Change{Op: repo.OpPut, Path: ch.Path, Entry: e})
		case "delete":
			changes = append(changes, repo.Change{Op: repo.OpDelete, Path: ch.Path})
		case "rename":
			changes = append(changes, repo.Change{Op: repo.OpRename, Path: ch.Path, From: ch.From})
		default:
			return badRequest("unknown change op %q", ch.Op)
		}
	}
	h, base, err := rp.CommitOn(r.Context(), repo.CommitRequest{Ref: req.Ref, Parent: &parent, Changes: changes,
		Author: person(c), Message: req.Message, Principal: c.Principal, CheckPath: writeCheck(g), Rebase: req.Rebase})
	if err != nil {
		return err
	}
	// Uploaded Files have no DigestRecord yet.
	for _, ch := range changes {
		if ch.Op != repo.OpPut {
			continue
		}
		if _, ok, err := rp.Store().Cache().Digest(r.Context(), ch.Entry.Target); err == nil && !ok {
			rp.DigestLater(ch.Entry.Target)
		}
	}
	return writeJSON(w, api.CommitResult{Commit: h.String(), Parent: hexOrEmpty(base)})
}
