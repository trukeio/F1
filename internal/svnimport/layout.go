package svnimport

import (
	"fmt"
	"strings"
)

// The kinds of SVN paths.
const (
	container = iota + 1 // a directory of the layout itself: "", "branches", ...
	refRoot              // the directory of a branch or tag: "trunk", "branches/x"
	inRef                // a path inside a branch or tag
)

// class says where an SVN path falls in the layout.
type class struct {
	kind    int
	ref     string // the SVN path of the branch or tag ("" in --root mode)
	rel     string // the path inside it
	tag     bool
	trunk   bool
	name    string // the branch or tag name as F1 will use it
	renamed bool   // name differs from the SVN directory name
}

// refName returns the F1 ref of a branch or tag.
func (c class) refName(defaultBranch string) string {
	switch {
	case c.trunk:
		return "heads/" + defaultBranch
	case c.tag:
		return "tags/" + c.name
	default:
		return "heads/" + c.name
	}
}

type layout struct {
	root     bool
	trunk    string
	branches []string
	tags     []string
}

func newLayout(l Layout) (layout, error) {
	if l.Root {
		if l.Trunk != "" || len(l.Branches) > 0 || len(l.Tags) > 0 {
			return layout{}, fmt.Errorf("svnimport: --root takes no --trunk, --branches or --tags")
		}
		return layout{root: true}, nil
	}
	out := layout{trunk: clean(l.Trunk), branches: cleanAll(l.Branches), tags: cleanAll(l.Tags)}
	if out.trunk == "" {
		out.trunk = "trunk"
	}
	if len(l.Branches) == 0 {
		out.branches = []string{"branches"}
	}
	if len(l.Tags) == 0 {
		out.tags = []string{"tags"}
	}
	all := append(append([]string{out.trunk}, out.branches...), out.tags...)
	for i, a := range all {
		if a == "" {
			return layout{}, fmt.Errorf("svnimport: a layout path is empty")
		}
		for _, b := range all[i+1:] {
			if under(a, b) || under(b, a) {
				return layout{}, fmt.Errorf("svnimport: layout paths %s and %s overlap", a, b)
			}
		}
	}
	return out, nil
}

func clean(p string) string { return strings.Trim(p, "/") }

func cleanAll(ps []string) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = clean(p)
	}
	return out
}

// classify places an SVN path (without a leading "/") in the layout.
func (l layout) classify(p string) (class, error) {
	if l.root {
		if p == "" {
			return class{kind: container}, nil
		}
		return class{kind: inRef, rel: p, trunk: true}, nil
	}
	if p == l.trunk {
		return class{kind: refRoot, ref: p, trunk: true}, nil
	}
	if under(p, l.trunk) {
		return class{kind: inRef, ref: l.trunk, rel: p[len(l.trunk)+1:], trunk: true}, nil
	}
	for i, dirs := range [][]string{l.branches, l.tags} {
		for _, d := range dirs {
			if p == d {
				return class{kind: container}, nil
			}
			if !under(p, d) {
				continue
			}
			dir, rel, _ := strings.Cut(p[len(d)+1:], "/")
			name := refComponent(dir)
			c := class{kind: refRoot, ref: d + "/" + dir, tag: i == 1, name: name, renamed: name != dir}
			if rel != "" {
				c.kind, c.rel = inRef, rel
			}
			return c, nil
		}
	}
	all := append(append([]string{l.trunk}, l.branches...), l.tags...)
	for _, a := range all {
		if p == "" || strings.HasPrefix(a, p+"/") {
			return class{kind: container}, nil
		}
	}
	return class{}, fmt.Errorf("%s is outside the layout (trunk %s, branches %s, tags %s; see --root)",
		p, l.trunk, strings.Join(l.branches, ","), strings.Join(l.tags, ","))
}

// refComponent turns an SVN directory name into a valid ref name
// component, replacing what Git and F1 refuse with "-".
func refComponent(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r <= ' ' || r == 0x7f || strings.ContainsRune(`~^:?*[\`, r) {
			r = '-'
		}
		b.WriteRune(r)
	}
	s := b.String()
	s = strings.ReplaceAll(s, "..", "--")
	s = strings.ReplaceAll(s, "@{", "-{")
	if strings.HasPrefix(s, ".") {
		s = "-" + s[1:]
	}
	if strings.HasSuffix(s, ".lock") {
		s = strings.TrimSuffix(s, ".lock") + "-lock"
	}
	if strings.HasSuffix(s, ".") {
		s = s[:len(s)-1] + "-"
	}
	if s == "@" {
		s = "-"
	}
	return s
}
