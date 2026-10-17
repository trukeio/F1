package web

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"html/template"
	"io"
	"strings"

	"github.com/trukeio/f1/internal/client/textmerge"
	"github.com/trukeio/f1/pkg/f1client"
)

// Limits of one diff page. Files beyond them are listed without content.
const (
	diffFilesShown  = 1000 // changed files listed
	diffFilesDiffed = 100  // changed files whose content is diffed
	diffLinesShown  = 3000 // diff lines rendered per file
)

// fileDiff is one changed file of a commit or comparison.
type fileDiff struct {
	Path             string
	Status           string // added, deleted, modified
	OldKind, NewKind string
	// Note explains why no lines are shown: binary, too large, not
	// diffed (over diffFilesDiffed), or a kind with no text.
	Note       string
	Truncated  bool // more than diffLinesShown lines
	Hunks      []hunk
	Adds, Dels int
}

type hunk struct {
	Header string
	Lines  []diffLine
}

// diffLine is a line of a hunk; Old or New is 0 where the line is absent.
type diffLine struct {
	Op       string // "add", "del" or "ctx"
	Old, New int
	Text     string
	HTML     template.HTML // Text, highlighted or escaped
}

// diffPage is a commit or comparison: its changed files and their diffs.
type diffPage struct {
	NS, Base, Head string // for links to the files
	Files          []fileDiff
	Total          int // changed files, including those not listed
	Adds, Dels     int
}

// buildDiff diffs the changes between commits (or refs) a and b. An empty
// a means the empty tree, for a root commit.
func (s *Server) buildDiff(ctx context.Context, api *f1client.Client, ns, a, b string) (*diffPage, error) {
	var changes []f1client.Change
	if a == "" {
		resp, err := api.List(ctx, ns, b, "", &f1client.ListOptions{Limit: diffFilesShown + 1})
		if err != nil {
			return nil, err
		}
		for _, it := range resp.Items {
			changes = append(changes, f1client.Change{Path: it.Path, New: &f1client.Entry{Kind: it.Kind, Target: it.File}})
		}
	} else {
		var err error
		if changes, err = api.Diff(ctx, ns, a, b, ""); err != nil {
			return nil, err
		}
	}
	d := &diffPage{NS: ns, Base: a, Head: b, Total: len(changes)}
	for i, c := range changes {
		if i == diffFilesShown {
			break
		}
		fd := fileDiff{Path: c.Path, Status: "modified"}
		switch {
		case c.Old == nil:
			fd.Status, fd.NewKind = "added", c.New.Kind
		case c.New == nil:
			fd.Status, fd.OldKind = "deleted", c.Old.Kind
		default:
			fd.OldKind, fd.NewKind = c.Old.Kind, c.New.Kind
		}
		if i >= diffFilesDiffed {
			fd.Note = "Not shown: too many changed files."
		} else if err := s.diffFile(ctx, api, ns, a, b, &fd); err != nil {
			return nil, err
		}
		d.Adds += fd.Adds
		d.Dels += fd.Dels
		d.Files = append(d.Files, fd)
	}
	return d, nil
}

// textKind reports whether entries of a kind have text content to diff.
func textKind(k string) bool { return k == "" || k == "file" || k == "exec" || k == "symlink" }

// diffFile fills in the hunks of one changed file.
func (s *Server) diffFile(ctx context.Context, api *f1client.Client, ns, a, b string, fd *fileDiff) error {
	if !textKind(fd.OldKind) || !textKind(fd.NewKind) {
		fd.Note = "Not shown: no text content."
		return nil
	}
	var old, cur []byte
	var large bool
	if fd.Status != "added" {
		data, tooLarge, err := s.readFile(ctx, api, ns, a, fd.Path)
		if err != nil {
			return err
		}
		old, large = data, tooLarge
	}
	if fd.Status != "deleted" && !large {
		data, tooLarge, err := s.readFile(ctx, api, ns, b, fd.Path)
		if err != nil {
			return err
		}
		cur, large = data, tooLarge
	}
	if large {
		fd.Note = "Not shown: the file is too large."
		return nil
	}
	fa, fb := textmerge.Bytes(old), textmerge.Bytes(cur)
	for _, f := range []textmerge.File{fa, fb} {
		if ok, err := textmerge.IsText(f); err != nil {
			return err
		} else if !ok {
			fd.Note = "Not shown: binary file."
			return nil
		}
	}
	var buf bytes.Buffer
	if err := textmerge.Unified(ctx, fa, fb, "a", "b", &buf); err != nil {
		return err
	}
	if err := parseUnified(&buf, fd); err != nil {
		return err
	}
	// Each side is highlighted whole, so that a line inside a multi-line
	// comment or string is colored as such.
	hlOld := highlight(fd.Path, strings.TrimSuffix(string(old), "\n"))
	hlNew := highlight(fd.Path, strings.TrimSuffix(string(cur), "\n"))
	for i := range fd.Hunks {
		for j := range fd.Hunks[i].Lines {
			l := &fd.Hunks[i].Lines[j]
			switch {
			case l.Op == "add" && l.New <= len(hlNew):
				l.HTML = hlNew[l.New-1]
			case l.Op != "add" && l.Old <= len(hlOld):
				l.HTML = hlOld[l.Old-1]
			default:
				l.HTML = template.HTML(template.HTMLEscapeString(l.Text))
			}
		}
	}
	return nil
}

// readFile reads a file at a ref, up to the render limit. Larger files are
// not read; tooLarge reports them.
func (s *Server) readFile(ctx context.Context, api *f1client.Client, ns, ref, p string) (data []byte, tooLarge bool, err error) {
	body, fi, err := api.Get(ctx, ns, ref, p, nil)
	if err != nil {
		return nil, false, err
	}
	defer body.Close()
	if fi.Size > s.cfg.MaxRender {
		return nil, true, nil
	}
	data, err = io.ReadAll(io.LimitReader(body, s.cfg.MaxRender+1))
	if int64(len(data)) > s.cfg.MaxRender {
		return nil, true, err
	}
	return data, false, err
}

// parseUnified reads a unified diff into hunks.
func parseUnified(r io.Reader, fd *fileDiff) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	var h *hunk
	var oldN, newN, lines int
	for sc.Scan() {
		line := sc.Text()
		switch {
		case h == nil && (strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ ")):
			continue // the file header, before the first hunk
		case strings.HasPrefix(line, `\`):
			continue // "\ No newline at end of file"
		case strings.HasPrefix(line, "@@ "):
			var a, b int
			if _, err := fmt.Sscanf(line, "@@ -%d", &a); err != nil {
				return fmt.Errorf("diff: bad hunk header %q", line)
			}
			_, after, _ := strings.Cut(line, " +")
			if _, err := fmt.Sscanf(after, "%d", &b); err != nil {
				return fmt.Errorf("diff: bad hunk header %q", line)
			}
			// A side with an empty range has no lines in the hunk, so
			// its start number is never used.
			oldN, newN = a, b
			fd.Hunks = append(fd.Hunks, hunk{Header: line})
			h = &fd.Hunks[len(fd.Hunks)-1]
			continue
		}
		if h == nil || line == "" {
			continue
		}
		op, txt := line[0], line[1:]
		switch op {
		case '+':
			fd.Adds++
		case '-':
			fd.Dels++
		}
		if lines == diffLinesShown {
			fd.Truncated = true
			continue // keep counting
		}
		lines++
		switch op {
		case '+':
			h.Lines = append(h.Lines, diffLine{Op: "add", New: newN, Text: txt})
			newN++
		case '-':
			h.Lines = append(h.Lines, diffLine{Op: "del", Old: oldN, Text: txt})
			oldN++
		default:
			h.Lines = append(h.Lines, diffLine{Op: "ctx", Old: oldN, New: newN, Text: txt})
			oldN++
			newN++
		}
	}
	return sc.Err()
}
