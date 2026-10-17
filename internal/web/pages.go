package web

import (
	"errors"
	"html/template"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/trukeio/f1/internal/client/textmerge"
	"github.com/trukeio/f1/pkg/f1client"
)

// commitsPerPage is the length of a page of history.
const commitsPerPage = 35

// routes registers the pages of ADR-0018, item 8. f1web's own pages are
// under /-/, which no namespace name can be.
func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /{ns}", s.page(s.nsRoot))
	mux.HandleFunc("GET /{ns}/tree/{ref}", s.page(s.tree))
	mux.HandleFunc("GET /{ns}/tree/{ref}/{path...}", s.page(s.tree))
	mux.HandleFunc("GET /{ns}/blob/{ref}/{path...}", s.page(s.blob))
	mux.HandleFunc("GET /{ns}/raw/{ref}/{path...}", s.page(s.raw))
	mux.HandleFunc("GET /{ns}/commits/{ref}", s.page(s.commits))
	mux.HandleFunc("GET /{ns}/commits/{ref}/{path...}", s.page(s.commits))
	mux.HandleFunc("GET /{ns}/commit/{hash}", s.page(s.commit))
	mux.HandleFunc("GET /{ns}/compare", s.page(s.compareForm))
	mux.HandleFunc("GET /{ns}/compare/{range}", s.page(s.compare))
	mux.HandleFunc("GET /{ns}/branches", s.page(s.refs))
	mux.HandleFunc("GET /{ns}/tags", s.page(s.refs))
	mux.HandleFunc("GET /{ns}/locks", s.page(s.locks))
}

// pathValue returns the path in a URL, without surrounding slashes.
func pathValue(r *http.Request) string { return strings.Trim(r.PathValue("path"), "/") }

func (s *Server) show(w http.ResponseWriter, r *http.Request, rq *request, name, title string, data any) error {
	s.render(w, r, http.StatusOK, name, pageData{User: rq.sess.principal, Title: title, Data: data})
	return nil
}

// treePage is a directory listing.
type treePage struct {
	*repoView
	Entries    []entry
	Next       string // After value of the next page
	Last       *f1client.Commit
	Empty      bool // the namespace has no commits yet
	ReadmeName string
	ReadmeHTML template.HTML // Markdown README
	ReadmeText string        // other README
}

func (s *Server) nsRoot(w http.ResponseWriter, r *http.Request, rq *request) error {
	v, err := s.loadRepo(r.Context(), rq.api, r.PathValue("ns"), "", "", "code")
	if err != nil {
		return err
	}
	return s.showTree(w, r, rq, v)
}

func (s *Server) tree(w http.ResponseWriter, r *http.Request, rq *request) error {
	v, err := s.loadRepo(r.Context(), rq.api, r.PathValue("ns"), r.PathValue("ref"), pathValue(r), "code")
	if err != nil {
		return err
	}
	return s.showTree(w, r, rq, v)
}

func (s *Server) showTree(w http.ResponseWriter, r *http.Request, rq *request, v *repoView) error {
	ctx := r.Context()
	v.View = "tree"
	if err := v.loadRefs(ctx, rq.api); err != nil {
		return err
	}
	p := &treePage{repoView: v}
	title := v.NS.Name
	if v.Path != "" {
		title = v.Path + " at " + displayRef(v.Ref) + " · " + v.NS.Name
	}
	entries, next, err := listDir(ctx, rq.api, v.NS.Name, v.Ref, v.Path, r.URL.Query().Get("after"))
	if f1client.Code(err) == f1client.CodeNotFound && v.Path == "" && len(v.Branches)+len(v.Tags) == 0 {
		p.Empty = true
		return s.show(w, r, rq, "tree", title, p)
	}
	if err != nil {
		return err
	}
	if len(entries) == 0 && v.Path != "" {
		// Not a directory: perhaps a file.
		if _, err := rq.api.Stat(ctx, v.NS.Name, v.Ref, v.Path); err != nil {
			return err
		}
		http.Redirect(w, r, pageURL(v.NS.Name, "blob", v.Ref, v.Path), http.StatusSeeOther)
		return nil
	}
	p.Entries, p.Next = entries, next
	if v.History() {
		// Without whole-namespace read, the root's history is refused
		// (D13); the page shows no last commit then.
		if log, err := rq.api.LogPage(ctx, v.NS.Name, v.Ref, v.Path, 0, 1); err == nil && len(log) == 1 {
			p.Last = &log[0]
		}
	}
	if rd, ok := findReadme(entries); ok {
		data, tooLarge, err := s.readFile(ctx, rq.api, v.NS.Name, v.Ref, rd.Path)
		if err == nil && !tooLarge {
			p.ReadmeName = rd.Name
			if isMarkdown(rd.Name) {
				if p.ReadmeHTML, err = renderMarkdown(data, v.NS.Name, v.Ref, rd.Path); err != nil {
					return err
				}
			} else {
				p.ReadmeText = string(data)
			}
		}
	}
	return s.show(w, r, rq, "tree", title, p)
}

// blobPage is a file.
type blobPage struct {
	*repoView
	Info      f1client.FileInfo
	Permalink string
	Lines     []string      // text
	Markdown  template.HTML // rendered Markdown
	CanPlain  bool          // Markdown, which can also be shown as source
	Image     bool
	Symlink   string // target
	Note      string // why the content is not shown
}

func (s *Server) blob(w http.ResponseWriter, r *http.Request, rq *request) error {
	ctx := r.Context()
	v, err := s.loadRepo(ctx, rq.api, r.PathValue("ns"), r.PathValue("ref"), pathValue(r), "code")
	if err != nil {
		return err
	}
	v.View = "blob"
	if err := v.loadRefs(ctx, rq.api); err != nil {
		return err
	}
	info, err := rq.api.Stat(ctx, v.NS.Name, v.Ref, v.Path)
	var ae *f1client.APIError
	if errors.As(err, &ae) && (ae.Status == http.StatusNotFound || ae.Status == http.StatusConflict) {
		// A directory is a conflict (path_conflict); a HEAD response
		// carries only the status.
		list, lerr := rq.api.List(ctx, v.NS.Name, v.Ref, v.Path+"/", &f1client.ListOptions{Limit: 1})
		if lerr == nil && len(list.Items) > 0 {
			http.Redirect(w, r, pageURL(v.NS.Name, "tree", v.Ref, v.Path), http.StatusSeeOther)
			return nil
		}
	}
	if err != nil {
		return err
	}
	p := &blobPage{repoView: v, Info: info, Permalink: pageURL(v.NS.Name, "blob", info.Commit, v.Path)}
	title := v.Path + " at " + displayRef(v.Ref) + " · " + v.NS.Name
	switch {
	case info.Kind == "gitlink":
		p.Note = "A Git submodule."
	case info.Kind == "lfs":
		p.Note = "Stored with Git LFS. Download it to view it."
	case isImage(rawType(info.ContentType, v.Path)):
		p.Image = true
	case info.Size > s.cfg.MaxRender:
		p.Note = "This file is too large to display."
	default:
		data, tooLarge, err := s.readFile(ctx, rq.api, v.NS.Name, info.Commit, v.Path)
		if err != nil {
			return err
		}
		f := textmerge.Bytes(data)
		text, err := textmerge.IsText(f)
		switch {
		case err != nil:
			return err
		case tooLarge:
			p.Note = "This file is too large to display."
		case info.Kind == "symlink":
			p.Symlink = string(data)
		case !text:
			p.Note = "This is a binary file."
		case isMarkdown(v.Path) && r.URL.Query().Get("plain") != "1":
			p.CanPlain = true
			if p.Markdown, err = renderMarkdown(data, v.NS.Name, v.Ref, v.Path); err != nil {
				return err
			}
		default:
			p.CanPlain = isMarkdown(v.Path)
			p.Lines = strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
			if len(data) == 0 {
				p.Lines = nil
			}
		}
	}
	return s.show(w, r, rq, "blob", title, p)
}

// rawType returns the media type of a file: the one stored with it, or
// one guessed from its name.
func rawType(stored, name string) string {
	if t, _, err := mime.ParseMediaType(stored); err == nil && t != "application/octet-stream" {
		return t
	}
	if t, _, err := mime.ParseMediaType(mime.TypeByExtension(path.Ext(name))); err == nil {
		return t
	}
	return "application/octet-stream"
}

func isImage(t string) bool {
	switch t {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "image/avif", "image/bmp", "image/svg+xml", "image/x-icon", "image/vnd.microsoft.icon":
		return true
	}
	return false
}

// rawHeaders are passed from the browser to F1, and back.
var (
	rawRequestHeaders  = []string{"Range", "If-Range", "If-None-Match"}
	rawResponseHeaders = []string{"Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Cache-Control"}
)

// raw streams a file. The content comes from users, so it is served as
// inert: images and media inline, text as text/plain, everything else as
// a download, always under a sandboxing CSP, so that no file can run
// script in f1web's origin (ADR-0018, item 8).
func (s *Server) raw(w http.ResponseWriter, r *http.Request, rq *request) error {
	ns, ref, p := r.PathValue("ns"), r.PathValue("ref"), pathValue(r)
	hdr := http.Header{}
	for _, h := range rawRequestHeaders {
		if v := r.Header.Get(h); v != "" {
			hdr.Set(h, v)
		}
	}
	resp, err := rq.api.Open(r.Context(), ns, ref, p, hdr)
	var ae *f1client.APIError
	if errors.As(err, &ae) && ae.Status == http.StatusRequestedRangeNotSatisfiable {
		w.WriteHeader(ae.Status)
		return nil
	}
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	h := w.Header()
	for _, k := range rawResponseHeaders {
		if v := resp.Header.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	t := rawType(resp.Header.Get("Content-Type"), p)
	switch {
	case isImage(t), strings.HasPrefix(t, "video/"), strings.HasPrefix(t, "audio/"):
		h.Set("Content-Type", t)
	case strings.HasPrefix(t, "text/"), t == "application/json", t == "application/xml", t == "application/javascript":
		h.Set("Content-Type", "text/plain; charset=utf-8")
	default:
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(p)}))
	}
	h.Set("Content-Security-Policy", "default-src 'none'; img-src data:; media-src 'self'; style-src 'unsafe-inline'; sandbox")
	w.WriteHeader(resp.StatusCode)
	_, err = io.Copy(w, resp.Body)
	if err != nil {
		s.log.Info("raw download interrupted", "path", r.URL.Path, "err", err)
	}
	return nil // the status is sent; an error page cannot follow
}

// commitsPage is a page of history.
type commitsPage struct {
	*repoView
	Days  []commitDay
	Page  int
	Older bool   // there is a next page
	Note  string // why there is no history to show
}

type commitDay struct {
	Date    string
	Commits []f1client.Commit
}

func (s *Server) commits(w http.ResponseWriter, r *http.Request, rq *request) error {
	ctx := r.Context()
	v, err := s.loadRepo(ctx, rq.api, r.PathValue("ns"), r.PathValue("ref"), pathValue(r), "commits")
	if err != nil {
		return err
	}
	v.View = "commits"
	if err := v.loadRefs(ctx, rq.api); err != nil {
		return err
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = max(page, 1)
	p := &commitsPage{repoView: v, Page: page}
	title := "History of " + v.NS.Name
	if v.Path != "" {
		title = "History of " + v.Path + " · " + v.NS.Name
	}
	log, err := rq.api.LogPage(ctx, v.NS.Name, v.Ref, v.Path, (page-1)*commitsPerPage, commitsPerPage+1)
	switch {
	case f1client.Code(err) == f1client.CodeForbidden && v.Path == "":
		p.Note = "You can read only parts of this namespace, so its whole history is not shown. Open the history of a file or directory from its page."
		return s.show(w, r, rq, "commits", title, p)
	case err != nil:
		return err
	}
	if len(log) > commitsPerPage {
		p.Older, log = true, log[:commitsPerPage]
	}
	for _, c := range log {
		d := date(c.Committer.Time)
		if len(p.Days) == 0 || p.Days[len(p.Days)-1].Date != d {
			p.Days = append(p.Days, commitDay{Date: d})
		}
		day := &p.Days[len(p.Days)-1]
		day.Commits = append(day.Commits, c)
	}
	return s.show(w, r, rq, "commits", title, p)
}

// commitPage is a commit, or a comparison of two refs, with its diff.
type commitPage struct {
	*repoView
	Commit     *f1client.Commit // nil for a comparison
	Base, Head string           // a comparison
	Diff       *diffPage
}

func (s *Server) commit(w http.ResponseWriter, r *http.Request, rq *request) error {
	ctx := r.Context()
	v, err := s.loadRepo(ctx, rq.api, r.PathValue("ns"), r.PathValue("hash"), "", "commits")
	if err != nil {
		return err
	}
	log, err := rq.api.Log(ctx, v.NS.Name, v.Ref, "", 1)
	if err != nil {
		return err
	}
	if len(log) == 0 {
		return errNotFound
	}
	c := log[0]
	if c.Hash != v.Ref {
		// A ref name: show the commit under its permanent URL.
		http.Redirect(w, r, commitURL(v.NS.Name, c.Hash), http.StatusSeeOther)
		return nil
	}
	parent := ""
	if len(c.Parents) > 0 {
		parent = c.Parents[0]
	}
	d, err := s.buildDiff(ctx, rq.api, v.NS.Name, parent, c.Hash)
	if err != nil {
		return err
	}
	p := &commitPage{repoView: v, Commit: &c, Diff: d}
	return s.show(w, r, rq, "commit", firstLine(c.Message)+" · "+v.NS.Name+"@"+shortHash(c.Hash), p)
}

// compareForm picks two refs to compare.
func (s *Server) compareForm(w http.ResponseWriter, r *http.Request, rq *request) error {
	ctx := r.Context()
	v, err := s.loadRepo(ctx, rq.api, r.PathValue("ns"), "", "", "commits")
	if err != nil {
		return err
	}
	base, head := r.URL.Query().Get("base"), r.URL.Query().Get("head")
	if base != "" && head != "" {
		http.Redirect(w, r, compareURL(v.NS.Name, base, head), http.StatusSeeOther)
		return nil
	}
	if err := v.loadRefs(ctx, rq.api); err != nil {
		return err
	}
	return s.show(w, r, rq, "commit", "Compare · "+v.NS.Name, &commitPage{repoView: v, Base: v.Ref, Head: v.Ref})
}

func compareURL(ns, base, head string) string {
	return "/" + ns + "/compare/" + url.PathEscape(base) + "..." + url.PathEscape(head)
}

func (s *Server) compare(w http.ResponseWriter, r *http.Request, rq *request) error {
	ctx := r.Context()
	base, head, ok := strings.Cut(r.PathValue("range"), "...")
	if !ok {
		base, head, ok = strings.Cut(r.PathValue("range"), "..")
	}
	if !ok || base == "" || head == "" {
		return errNotFound
	}
	v, err := s.loadRepo(ctx, rq.api, r.PathValue("ns"), "", "", "commits")
	if err != nil {
		return err
	}
	if err := v.loadRefs(ctx, rq.api); err != nil {
		return err
	}
	d, err := s.buildDiff(ctx, rq.api, v.NS.Name, base, head)
	if err != nil {
		return err
	}
	p := &commitPage{repoView: v, Base: base, Head: head, Diff: d}
	return s.show(w, r, rq, "commit", "Comparing "+displayRef(base)+"..."+displayRef(head)+" · "+v.NS.Name, p)
}

// refsPage lists branches or tags.
type refsPage struct {
	*repoView
	Refs    []f1client.Ref
	Default string // full name of the default branch
}

func (s *Server) refs(w http.ResponseWriter, r *http.Request, rq *request) error {
	ctx := r.Context()
	tab := path.Base(r.URL.Path) // branches or tags
	v, err := s.loadRepo(ctx, rq.api, r.PathValue("ns"), "", "", tab)
	if err != nil {
		return err
	}
	if err := v.loadRefs(ctx, rq.api); err != nil {
		return err
	}
	p := &refsPage{repoView: v, Refs: v.Branches, Default: "heads/" + shortRef(v.NS.Config.DefaultRef)}
	title := "Branches · " + v.NS.Name
	if tab == "tags" {
		p.Refs, title = v.Tags, "Tags · "+v.NS.Name
	}
	return s.show(w, r, rq, "refs", title, p)
}

// locksPage lists the namespace's locks.
type locksPage struct {
	*repoView
	Locks []f1client.Lock
}

func (s *Server) locks(w http.ResponseWriter, r *http.Request, rq *request) error {
	ctx := r.Context()
	v, err := s.loadRepo(ctx, rq.api, r.PathValue("ns"), "", "", "locks")
	if err != nil {
		return err
	}
	locks, err := rq.api.Locks(ctx, v.NS.Name, "", "")
	if err != nil {
		return err
	}
	return s.show(w, r, rq, "locks", "Locks · "+v.NS.Name, &locksPage{repoView: v, Locks: locks})
}
