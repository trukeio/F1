package web

import (
	"bytes"
	"html/template"
	"net/url"
	"path"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// markdown renders GitHub Flavored Markdown. Raw HTML is not rendered
// (goldmark's default), and goldmark drops dangerous URLs such as
// javascript:, so the output is safe to embed (ADR-0018, item 15).
var markdown = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithParserOptions(
		parser.WithAutoHeadingID(),
		parser.WithASTTransformers(util.Prioritized(linkRewriter{}, 100)),
	),
)

// linkBaseKey carries the document's location to linkRewriter.
var linkBaseKey = parser.NewContextKey()

// linkBase is where a Markdown file is: relative links resolve against
// its directory.
type linkBase struct {
	ns, ref, dir string
}

// renderMarkdown renders the Markdown file at file in ns at ref.
func renderMarkdown(src []byte, ns, ref, file string) (template.HTML, error) {
	pc := parser.NewContext()
	pc.Set(linkBaseKey, linkBase{ns: ns, ref: ref, dir: path.Dir(file)})
	var buf bytes.Buffer
	if err := markdown.Convert(src, &buf, parser.WithContext(pc)); err != nil {
		return "", err
	}
	return template.HTML(buf.String()), nil
}

// linkRewriter points relative links at the file pages of the same ref,
// and relative images at the raw files, as GitHub does.
type linkRewriter struct{}

func (linkRewriter) Transform(doc *ast.Document, _ text.Reader, pc parser.Context) {
	base, ok := pc.Get(linkBaseKey).(linkBase)
	if !ok {
		return
	}
	ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n := n.(type) {
		case *ast.Link:
			n.Destination = base.resolve(n.Destination, "blob")
		case *ast.Image:
			n.Destination = base.resolve(n.Destination, "raw")
		}
		return ast.WalkContinue, nil
	})
}

// resolve rewrites a relative destination; others are kept.
func (b linkBase) resolve(dest []byte, view string) []byte {
	u, err := url.Parse(string(dest))
	if err != nil || u.Scheme != "" || u.Host != "" || u.Path == "" {
		return dest // absolute, or only a fragment or query
	}
	p := u.Path
	if strings.HasPrefix(p, "/") {
		p = strings.TrimPrefix(p, "/") // from the namespace root
	} else {
		p = path.Join(b.dir, p)
	}
	p = path.Clean(p)
	if p == "." || p == ".." || strings.HasPrefix(p, "../") {
		p = ""
	}
	if p == "" {
		view = "tree"
	}
	out := pageURL(b.ns, view, b.ref, p)
	if u.Fragment != "" {
		out += "#" + u.EscapedFragment()
	}
	return []byte(out)
}

// isMarkdown reports whether a file name is a Markdown file.
func isMarkdown(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".md", ".markdown", ".mdown", ".mkd":
		return true
	}
	return false
}

// readmeNames are the files shown below a directory listing, in order of
// preference.
var readmeNames = []string{"readme.md", "readme.markdown", "readme", "readme.txt"}

// findReadme returns the README among a directory's entries.
func findReadme(entries []entry) (entry, bool) {
	for _, want := range readmeNames {
		for _, e := range entries {
			if !e.Dir && strings.ToLower(e.Name) == want {
				return e, true
			}
		}
	}
	return entry{}, false
}
