package git

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/policy"
	"github.com/trukeio/f1/internal/repo"
)

// agent is the agent capability value.
const agent = "agent=f1/1"

// v0Caps are the capabilities advertised to protocol v0/v1 fetches.
var v0Caps = []string{
	"multi_ack_detailed", "side-band-64k", "ofs-delta", "shallow", "deepen-since", "deepen-relative", "no-progress",
	"include-tag", "thin-pack", "allow-tip-sha1-in-want", "allow-reachable-sha1-in-want", "filter",
	"object-format=sha1", agent,
}

// gitRef is a ref as Git sees it.
type gitRef struct {
	name   string // full Git name: refs/heads/main
	sha    SHA
	peeled *SHA // the commit an annotated tag points to (recursively)
	tag    *Object
}

// advertisedRefs exports the namespace's refs: Git names, ids and peeled
// tags, in name order.
func advertisedRefs(ctx context.Context, x *Exporter, rp *repo.Repo) ([]gitRef, error) {
	refs := rp.Refs()
	names := make([]string, 0, len(refs))
	for n := range refs {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []gitRef
	for _, n := range names {
		s, t, err := x.RefTarget(ctx, refs[n])
		if err != nil {
			return nil, fmt.Errorf("ref %s: %w", n, err)
		}
		r := gitRef{name: "refs/" + n, sha: s}
		if t == TypeTag {
			r.tag = &Object{Type: TypeTag, SHA: s, F1: refs[n]}
			peeled, err := peel(ctx, x, refs[n])
			if err != nil {
				return nil, fmt.Errorf("ref %s: %w", n, err)
			}
			r.peeled = &peeled
		}
		out = append(out, r)
	}
	return out, nil
}

// peel returns the Git id of the non-tag object a tag chain points to.
func peel(ctx context.Context, x *Exporter, h hash.Hash) (SHA, error) {
	for {
		t, err := x.readTag(ctx, h)
		if err != nil {
			return SHA{}, err
		}
		if t.TargetType != "tag" {
			return x.TagTarget(ctx, t)
		}
		h = t.Object
	}
}

// head returns the Git name of the default ref (the HEAD symref target).
func head(rp *repo.Repo) string {
	return "refs/" + policy.FullRefName(rp.Meta().DefaultRef)
}

func refTags(refs []gitRef) []Object {
	var out []Object
	for _, r := range refs {
		if r.tag != nil {
			out = append(out, *r.tag)
		}
	}
	return out
}

// --- advertisement ---

// advertise answers GET info/refs?service=git-upload-pack.
func (s *Server) advertise(ctx context.Context, w http.ResponseWriter, rp *repo.Repo, version int) error {
	var buf bytes.Buffer
	p := &pktWriter{w: &buf}
	p.line("# service=git-upload-pack\n")
	p.flush()
	if version == 2 {
		p.line("version 2\n")
		p.line("%s\n", agent)
		p.line("ls-refs=unborn\n")
		p.line("fetch=shallow filter\n")
		p.line("object-info\n")
		p.line("object-format=sha1\n")
		p.flush()
	} else {
		x := NewExporter(rp)
		refs, err := advertisedRefs(ctx, x, rp)
		if err != nil {
			return err
		}
		if version == 1 {
			p.line("version 1\n")
		}
		caps := strings.Join(v0Caps, " ")
		hd := head(rp)
		var headRef *gitRef
		for i := range refs {
			if refs[i].name == hd {
				headRef = &refs[i]
			}
		}
		first := true
		emit := func(sha SHA, name string) {
			if first {
				extra := ""
				if headRef != nil {
					extra = " symref=HEAD:" + hd
				}
				p.line("%s %s\x00%s%s\n", sha, name, caps, extra)
				first = false
				return
			}
			p.line("%s %s\n", sha, name)
		}
		if headRef != nil {
			emit(headRef.sha, "HEAD")
		}
		for _, r := range refs {
			emit(r.sha, r.name)
			if r.peeled != nil {
				p.line("%s %s^{}\n", *r.peeled, r.name)
			}
		}
		if first {
			p.line("%s capabilities^{}\x00%s\n", SHA{}, caps)
		}
		p.flush()
	}
	if p.err != nil {
		return p.err
	}
	w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
	w.Header().Set("Cache-Control", "no-cache")
	_, err := w.Write(buf.Bytes())
	return err
}

// --- protocol v2 ---

// serveV2 answers a protocol v2 command (POST git-upload-pack).
func (s *Server) serveV2(ctx context.Context, w http.ResponseWriter, body io.Reader, rp *repo.Repo) error {
	pr := &pktReader{r: newBufReader(body)}
	command := ""
	var caps, args []string
	section := &caps
	for n := 0; ; n++ {
		if n > maxRequestLines {
			return badRequest("request too long")
		}
		kind, data, err := pr.next()
		if err != nil {
			return badRequest("%v", err)
		}
		if kind == pktIsFlush || kind == pktEOF {
			break
		}
		if kind == pktIsDelim {
			section = &args
			continue
		}
		if kind != pktData {
			continue
		}
		line := string(data)
		if c, ok := strings.CutPrefix(line, "command="); ok && section == &caps {
			command = c
			continue
		}
		*section = append(*section, line)
	}
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	switch command {
	case "ls-refs":
		return s.lsRefs(ctx, w, rp, args)
	case "fetch":
		return s.fetchV2(ctx, w, rp, args)
	case "object-info":
		return s.objectInfo(ctx, w, rp, args)
	case "":
		return badRequest("no command")
	}
	return badRequest("unknown command %q", command)
}

func (s *Server) lsRefs(ctx context.Context, w io.Writer, rp *repo.Repo, args []string) error {
	var symrefs, peelTags, unborn bool
	var prefixes []string
	for _, a := range args {
		switch {
		case a == "symrefs":
			symrefs = true
		case a == "peel":
			peelTags = true
		case a == "unborn":
			unborn = true
		case strings.HasPrefix(a, "ref-prefix "):
			prefixes = append(prefixes, strings.TrimPrefix(a, "ref-prefix "))
		}
	}
	match := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	x := NewExporter(rp)
	refs, err := advertisedRefs(ctx, x, rp)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	p := &pktWriter{w: &buf}
	if match("HEAD") {
		hd := head(rp)
		found := false
		for _, r := range refs {
			if r.name == hd {
				found = true
				line := r.sha.String() + " HEAD"
				if symrefs {
					line += " symref-target:" + hd
				}
				p.line("%s\n", line)
			}
		}
		if !found && unborn && symrefs {
			p.line("unborn HEAD symref-target:%s\n", hd)
		}
	}
	for _, r := range refs {
		if !match(r.name) {
			continue
		}
		line := r.sha.String() + " " + r.name
		if peelTags && r.peeled != nil {
			line += " peeled:" + r.peeled.String()
		}
		p.line("%s\n", line)
	}
	p.flush()
	if p.err != nil {
		return p.err
	}
	_, err = w.Write(buf.Bytes())
	return err
}

func (s *Server) parseFetchArgs(args []string) (*fetchRequest, bool, bool, error) {
	req := &fetchRequest{shallow: map[SHA]bool{}}
	done, progress := false, true
	for _, a := range args {
		k, v, _ := strings.Cut(a, " ")
		switch k {
		case "want":
			sha, err := ParseSHA(v)
			if err != nil {
				return nil, false, false, err
			}
			req.wants = append(req.wants, sha)
		case "have":
			sha, err := ParseSHA(v)
			if err != nil {
				return nil, false, false, err
			}
			req.haves = append(req.haves, sha)
		case "shallow":
			sha, err := ParseSHA(v)
			if err != nil {
				return nil, false, false, err
			}
			req.shallow[sha] = true
		case "deepen":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return nil, false, false, fmt.Errorf("invalid deepen %q", v)
			}
			req.depth = n
		case "deepen-since":
			t, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return nil, false, false, fmt.Errorf("invalid deepen-since %q", v)
			}
			req.deepenSince = t
		case "deepen-relative":
			req.relative = true
		case "deepen-not":
			return nil, false, false, fmt.Errorf("%s is not supported", k)
		case "filter":
			f, err := parseFilter(v)
			if err != nil {
				return nil, false, false, err
			}
			req.filter = &f
		case "include-tag":
			req.includeTag = true
		case "done":
			done = true
		case "no-progress":
			progress = false
		case "ofs-delta":
			req.ofsDelta = true
		case "thin-pack", "wait-for-done":
		case "want-ref", "packfile-uris", "sideband-all":
			return nil, false, false, fmt.Errorf("%s is not supported", k)
		}
	}
	return req, done, progress, nil
}

func (s *Server) fetchV2(ctx context.Context, w http.ResponseWriter, rp *repo.Repo, args []string) error {
	req, done, _, err := s.parseFetchArgs(args)
	if err != nil {
		return badRequest("%v", err)
	}
	x := NewExporter(rp)
	refs, err := advertisedRefs(ctx, x, rp)
	if err != nil {
		return err
	}
	res, err := walk(ctx, x, req, refTags(refs))
	if err != nil {
		return badRequest("%v", err)
	}
	p := &pktWriter{w: w}
	if !done {
		// Stateless negotiation: acknowledge the common commits and send
		// the pack at once ("ready"); haves the server does not know only
		// make the pack larger.
		p.line("acknowledgments\n")
		if len(res.common) == 0 {
			p.line("NAK\n")
		}
		for _, c := range res.common {
			p.line("ACK %s\n", c)
		}
		p.line("ready\n")
		p.delim()
	}
	if req.deepening() || len(res.shallow) > 0 || len(res.unshallow) > 0 {
		p.line("shallow-info\n")
		for _, sh := range res.shallow {
			p.line("shallow %s\n", sh)
		}
		for _, u := range res.unshallow {
			p.line("unshallow %s\n", u)
		}
		p.delim()
	}
	p.line("packfile\n")
	if p.err != nil {
		return p.err
	}
	if err := writePack(ctx, &sideband{p: p}, x, res.objects, req.ofsDelta); err != nil {
		s.Log.Warn("git fetch interrupted", "namespace", rp.Meta().UUID, "err", err)
		return nil // the response has started; the client sees a broken pack
	}
	p.flush()
	return p.err
}

func (s *Server) objectInfo(ctx context.Context, w io.Writer, rp *repo.Repo, args []string) error {
	x := NewExporter(rp)
	size := false
	var oids []SHA
	for _, a := range args {
		switch {
		case a == "size":
			size = true
		case strings.HasPrefix(a, "oid "):
			sha, err := ParseSHA(strings.TrimPrefix(a, "oid "))
			if err != nil {
				return badRequest("%v", err)
			}
			oids = append(oids, sha)
		}
	}
	var buf bytes.Buffer
	p := &pktWriter{w: &buf}
	if size {
		p.line("size\n")
	}
	for _, sha := range oids {
		o, ok, err := x.Resolve(ctx, sha)
		if err != nil {
			return err
		}
		if !ok {
			p.line("%s\n", sha) // unknown: no attributes
			continue
		}
		line := sha.String()
		if size {
			var n int64
			if o.Type == TypeBlob {
				m, err := rp.FileSize(ctx, o.Blob.Target)
				if err != nil {
					return err
				}
				n = int64(m)
			} else {
				data, err := x.Content(ctx, o)
				if err != nil {
					return err
				}
				n = int64(len(data))
			}
			line += " " + strconv.FormatInt(n, 10)
		}
		p.line("%s\n", line)
	}
	p.flush()
	if p.err != nil {
		return p.err
	}
	_, err := w.Write(buf.Bytes())
	return err
}

// --- protocol v0/v1 (stateless RPC) ---

// serveV0 answers a v0/v1 upload-pack request: wants (capabilities on the
// first), shallow and deepen lines, a flush, haves, and "done" when the
// client is finished negotiating. With multi_ack_detailed, a request
// without "done" gets ACK lines for the common commits, "ready" and NAK,
// and the client sends a final request with "done".
func (s *Server) serveV0(ctx context.Context, w http.ResponseWriter, body io.Reader, rp *repo.Repo) error {
	pr := &pktReader{r: newBufReader(body)}
	var args []string
	caps := map[string]bool{}
	done := false
	flushes, afterFlush := 0, 0 // lines after the wants section
	for n := 0; ; n++ {
		if n > maxRequestLines {
			return badRequest("request too long")
		}
		kind, data, err := pr.next()
		if err != nil {
			return badRequest("%v", err)
		}
		if kind == pktEOF {
			break
		}
		if kind == pktIsFlush {
			flushes++
		}
		if kind != pktData {
			continue
		}
		if flushes > 0 {
			afterFlush++
		}
		line := string(data)
		if line == "done" {
			done = true
			break
		}
		if strings.HasPrefix(line, "want ") && len(caps) == 0 {
			if i := strings.IndexByte(line[5:], ' '); i >= 0 {
				for _, c := range strings.Fields(line[5+i+1:]) {
					caps[c] = true
				}
				line = line[:5+i]
			}
		}
		if f, ok := strings.CutPrefix(line, "filter "); ok {
			line = "filter " + f
		}
		args = append(args, line)
	}
	for _, c := range []string{"include-tag", "deepen-relative", "ofs-delta"} {
		if caps[c] {
			args = append(args, c)
		}
	}
	req, _, _, err := s.parseFetchArgs(args)
	if err != nil {
		return badRequest("%v", err)
	}
	x := NewExporter(rp)
	refs, err := advertisedRefs(ctx, x, rp)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	res, err := walk(ctx, x, req, refTags(refs))
	if err != nil {
		return badRequest("%v", err)
	}
	p := &pktWriter{w: w}
	if req.deepening() || len(req.shallow) > 0 {
		for _, sh := range res.shallow {
			p.line("shallow %s\n", sh)
		}
		for _, u := range res.unshallow {
			p.line("unshallow %s\n", u)
		}
		p.flush()
	}
	if !done && afterFlush == 0 {
		// The first request of a stateless shallow fetch: wants and depth
		// only; the answer is the shallow list alone.
		return p.err
	}
	if !done {
		for _, c := range res.common {
			p.line("ACK %s common\n", c)
		}
		if len(res.common) > 0 {
			p.line("ACK %s ready\n", res.common[len(res.common)-1])
		}
		p.line("NAK\n")
		return p.err
	}
	if len(res.common) > 0 {
		p.line("ACK %s\n", res.common[len(res.common)-1])
	} else {
		p.line("NAK\n")
	}
	if p.err != nil {
		return p.err
	}
	var out io.Writer = w
	if caps["side-band-64k"] || caps["side-band"] {
		out = &sideband{p: p}
	}
	if err := writePack(ctx, out, x, res.objects, req.ofsDelta); err != nil {
		s.Log.Warn("git fetch interrupted", "err", err)
		return nil
	}
	if out != io.Writer(w) {
		p.flush()
	}
	return p.err
}
