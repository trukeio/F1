package web

import (
	"fmt"
	"html/template"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
)

// Syntax highlighting (ADR-0018, item 15) with chroma, on the server.
// Tokens get CSS classes; highlightCSS defines their colors for light and
// dark mode.

var formatter = html.New(html.WithClasses(true), html.PreventSurroundingPre(true))

// lexerCache maps a file name to its lexer (nil: none), because
// lexers.Match tries every pattern of every lexer. It holds at most
// lexerCacheMax names.
var (
	lexerMu    sync.Mutex
	lexerCache = map[string]chroma.Lexer{}
)

const lexerCacheMax = 4096

func lexerFor(name string) chroma.Lexer {
	name = path.Base(name)
	lexerMu.Lock()
	l, ok := lexerCache[name]
	lexerMu.Unlock()
	if ok {
		return l
	}
	if l = lexers.Match(name); l != nil {
		l = chroma.Coalesce(l)
	}
	lexerMu.Lock()
	if len(lexerCache) < lexerCacheMax {
		lexerCache[name] = l
	}
	lexerMu.Unlock()
	return l
}

// highlight returns the lines of a text file as highlighted HTML, or nil
// if no lexer knows the file. Line i of the result is line i of
// strings.Split(src, "\n"), without the newline.
func highlight(name string, src string) []template.HTML {
	l := lexerFor(name)
	if l == nil {
		return nil
	}
	it, err := l.Tokenise(nil, src)
	if err != nil {
		return nil
	}
	lines := chroma.SplitTokensIntoLines(it.Tokens())
	want := strings.Count(src, "\n") + 1
	out := make([]template.HTML, 0, want)
	var b strings.Builder
	for _, line := range lines {
		if n := len(line); n > 0 {
			line[n-1].Value = strings.TrimSuffix(line[n-1].Value, "\n")
		}
		b.Reset()
		if err := formatter.Format(&b, styles.Fallback, chroma.Literator(line...)); err != nil {
			return nil
		}
		out = append(out, template.HTML(b.String()))
	}
	// The lexer drops a final empty line.
	for len(out) < want {
		out = append(out, "")
	}
	if len(out) != want {
		return nil // lines do not match the source: show it plain
	}
	return out
}

// highlightCSS returns the token colors of GitHub's light and dark styles.
// Backgrounds are left out, so diff colors show through.
var highlightCSS = sync.OnceValue(func() string {
	var b strings.Builder
	writeStyle(&b, styles.Get("github"))
	b.WriteString("@media (prefers-color-scheme: dark) {\n")
	writeStyle(&b, styles.Get("github-dark"))
	b.WriteString("}\n")
	return b.String()
})

func writeStyle(b *strings.Builder, s *chroma.Style) {
	types := make([]chroma.TokenType, 0, len(chroma.StandardTypes))
	for t := range chroma.StandardTypes {
		types = append(types, t)
	}
	sort.Slice(types, func(i, j int) bool { return types[i] < types[j] })
	for _, t := range types {
		class := chroma.StandardTypes[t]
		if class == "" || t == chroma.Background || t == chroma.PreWrapper || t == chroma.Line || t == chroma.CodeLine ||
			t == chroma.LineNumbers || t == chroma.LineNumbersTable || t == chroma.LineHighlight || t == chroma.LineTable || t == chroma.LineTableTD {
			continue
		}
		e := s.Get(t)
		var decl []string
		if e.Colour.IsSet() {
			decl = append(decl, "color: "+e.Colour.String())
		}
		if e.Bold == chroma.Yes {
			decl = append(decl, "font-weight: bold")
		}
		if e.Italic == chroma.Yes {
			decl = append(decl, "font-style: italic")
		}
		if e.Underline == chroma.Yes {
			decl = append(decl, "text-decoration: underline")
		}
		if len(decl) > 0 {
			fmt.Fprintf(b, ".hl .%s { %s }\n", class, strings.Join(decl, "; "))
		}
	}
}
