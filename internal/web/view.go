package web

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/trukeio/f1/pkg/f1client"
)

// errNotFound is a page that does not exist, found by f1web itself.
var errNotFound = errors.New("not found")

// repoView is what every page of a namespace shows: the namespace header,
// its tabs and the ref picker.
type repoView struct {
	NS   f1client.Namespace
	Ref  string // as in the URL: "main", "tags/v1" or a commit hash
	Path string // file or directory; "" for the root
	Tab  string // code, commits, branches, tags, locks
	// Refs are loaded only for pages with a ref picker.
	Branches, Tags []f1client.Ref
	// View is the page the picker links to for another ref: tree, blob
	// or commits.
	View string
	// Permalink is this page at the commit Ref points to, for the "y"
	// shortcut; "" when Ref is a commit already.
	Permalink string
}

// view returns the view; every namespace page embeds one.
func (v *repoView) view() *repoView { return v }

// History reports whether the namespace keeps history to browse.
func (v *repoView) History() bool { return v.NS.Config.Retention != "none" }

// Locks reports whether the namespace uses locks.
func (v *repoView) Locks() bool { return v.NS.Config.Locks != "" && v.NS.Config.Locks != "off" }

// RefIsCommit reports whether the page shows a fixed commit.
func (v *repoView) RefIsCommit() bool { return isHash(v.Ref) }

// Crumbs returns the path's parts, each with its path from the root.
func (v *repoView) Crumbs() []crumb {
	if v.Path == "" {
		return nil
	}
	parts := strings.Split(v.Path, "/")
	out := make([]crumb, len(parts))
	for i, p := range parts {
		out[i] = crumb{Name: p, Path: strings.Join(parts[:i+1], "/"), Last: i == len(parts)-1}
	}
	return out
}

type crumb struct {
	Name, Path string
	Last       bool
}

// loadRepo returns the view of namespace ns at ref. An empty ref is the
// default ref.
func (s *Server) loadRepo(ctx context.Context, api *f1client.Client, ns, ref, p, tab string) (*repoView, error) {
	list, err := api.Namespaces(ctx)
	if err != nil {
		return nil, err
	}
	for _, n := range list {
		if n.Name == ns {
			if ref == "" {
				ref = shortRef(n.Config.DefaultRef)
			}
			return &repoView{NS: n, Ref: ref, Path: p, Tab: tab}, nil
		}
	}
	return nil, errNotFound // unknown, or not readable: the same to the user
}

// loadRefs fills in the branches and tags, for the ref picker.
func (v *repoView) loadRefs(ctx context.Context, api *f1client.Client) error {
	refs, err := api.Refs(ctx, v.NS.Name)
	if err != nil {
		return err
	}
	for _, r := range refs {
		switch {
		case strings.HasPrefix(r.Name, "heads/"):
			v.Branches = append(v.Branches, r)
		case strings.HasPrefix(r.Name, "tags/"):
			v.Tags = append(v.Tags, r)
		}
	}
	return nil
}

// shortRef shortens a branch name for URLs and display: "heads/main" is
// "main". Other refs keep their category, as F1 resolves them (ADR-0006).
func shortRef(name string) string { return strings.TrimPrefix(name, "heads/") }

func isHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range []byte(s) {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// escPath escapes a file path for a URL, keeping its "/" separators.
func escPath(p string) string {
	parts := strings.Split(p, "/")
	for i, x := range parts {
		parts[i] = url.PathEscape(x)
	}
	return strings.Join(parts, "/")
}

// pageURL returns the URL of a view (tree, blob, raw, commits) of a path
// at a ref.
func pageURL(ns, view, ref, p string) string {
	u := "/" + ns + "/" + view + "/" + url.PathEscape(ref)
	if p != "" {
		u += "/" + escPath(p)
	}
	return u
}

func commitURL(ns, hash string) string { return "/" + ns + "/commit/" + hash }

// shortHash abbreviates a commit hash for display.
func shortHash(h string) string {
	if len(h) > 10 {
		return h[:10]
	}
	return h
}

// displayRef names a ref for display.
func displayRef(ref string) string {
	if isHash(ref) {
		return shortHash(ref)
	}
	return ref
}

// size formats a byte count.
func size(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d Bytes", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// firstLine is a commit message's title; rest is its body.
func firstLine(msg string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(msg), "\n")
	return line
}

func rest(msg string) string {
	_, body, _ := strings.Cut(strings.TrimSpace(msg), "\n")
	return strings.TrimSpace(body)
}

// date formats a commit time.
func date(unix int64) string { return time.Unix(unix, 0).UTC().Format("Jan 2, 2006") }

func timestamp(unix int64) string {
	return time.Unix(unix, 0).UTC().Format("2006-01-02 15:04:05 UTC")
}

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"pageURL":    pageURL,
		"commitURL":  commitURL,
		"shortRef":   shortRef,
		"shortHash":  shortHash,
		"displayRef": displayRef,
		"size":       size,
		"firstLine":  firstLine,
		"rest":       rest,
		"date":       date,
		"timestamp":  timestamp,
		"base":       path.Base,
		"add":        func(a, b int) int { return a + b },
		"hasPrefix":  strings.HasPrefix,
		"compareURL": compareURL,
		"parent": func(p string) string {
			if d := path.Dir(p); d != "." {
				return d
			}
			return ""
		},
	}
}

// entry is a row of a directory listing.
type entry struct {
	Name, Path string
	Dir        bool
	Kind       string
	Size       int64
	Last       *f1client.Commit // the last commit that changed it, if known
}

// listDir lists a directory at a ref, directories first. It returns the
// commit listed and the next page's After value, if any.
func listDir(ctx context.Context, api *f1client.Client, ns, ref, dir, after string) ([]entry, string, string, error) {
	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}
	resp, err := api.List(ctx, ns, ref, prefix, &f1client.ListOptions{Delimiter: "/", After: after, Limit: listPage})
	if err != nil {
		return nil, "", "", err
	}
	out := make([]entry, 0, len(resp.Items))
	for _, it := range resp.Items {
		p := strings.TrimSuffix(it.Path, "/")
		out = append(out, entry{Name: strings.TrimPrefix(p, prefix), Path: p, Dir: it.Prefix, Kind: it.Kind, Size: it.Size})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Dir && !out[j].Dir })
	return out, resp.Commit, resp.Next, nil
}

// listPage is the number of entries per page of a directory.
const listPage = 1000
