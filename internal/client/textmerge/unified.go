package textmerge

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
)

// context is the number of unchanged lines around each change.
const contextLines = 3

// Unified writes the unified diff of a and b to w, with the file names in
// its header. It writes nothing if they are equal.
func Unified(ctx context.Context, a, b File, nameA, nameB string, w io.Writer) error {
	ms, err := Diff(ctx, a, b)
	if err != nil {
		return err
	}
	// Segments alternate between a change (possibly empty) and a match;
	// the last one is a change.
	type seg struct {
		a0, a1, b0, b1 int64 // byte ranges
		la, lb         int64 // line counts
		match          bool
	}
	var segs []seg
	var pa, pb int64
	add := func(s seg) error {
		var err error
		if s.la, err = countLines(a, s.a0, s.a1); err != nil {
			return err
		}
		if s.lb, err = countLines(b, s.b0, s.b1); err != nil {
			return err
		}
		segs = append(segs, s)
		return nil
	}
	for _, m := range ms {
		if err := add(seg{a0: pa, a1: m.A, b0: pb, b1: m.B}); err != nil {
			return err
		}
		if err := add(seg{a0: m.A, a1: m.A + m.N, b0: m.B, b1: m.B + m.N, match: true}); err != nil {
			return err
		}
		pa, pb = m.A+m.N, m.B+m.N
	}
	if err := add(seg{a0: pa, a1: a.N, b0: pb, b1: b.N}); err != nil {
		return err
	}

	bw := bufio.NewWriterSize(w, 64<<10)
	header := false
	// Line numbers (0-based) at the start of each segment.
	lineA := make([]int64, len(segs)+1)
	lineB := make([]int64, len(segs)+1)
	for i, s := range segs {
		lineA[i+1], lineB[i+1] = lineA[i]+s.la, lineB[i]+s.lb
	}
	changed := func(i int) bool { return !segs[i].match && (segs[i].la > 0 || segs[i].lb > 0) }
	for i := 0; i < len(segs); i++ {
		if !changed(i) {
			continue
		}
		// A hunk from change i to change j, joined across matches of at
		// most 2 × context lines.
		j := i
		for j+2 < len(segs) && segs[j+1].la <= 2*contextLines && changed(j+2) {
			j += 2
		}
		pre := int64(0)
		if i > 0 {
			pre = min(contextLines, segs[i-1].la)
		}
		post := int64(0)
		if j+1 < len(segs) {
			post = min(contextLines, segs[j+1].la)
		}
		startA, startB := lineA[i]-pre, lineB[i]-pre
		endA, endB := lineA[j+1]+post, lineB[j+1]+post
		if !header {
			fmt.Fprintf(bw, "--- %s\n+++ %s\n", nameA, nameB)
			header = true
		}
		fmt.Fprintf(bw, "@@ -%s +%s @@\n", hunkRange(startA, endA-startA), hunkRange(startB, endB-startB))
		if pre > 0 {
			s := segs[i-1]
			from, err := lastLines(a, s.a0, s.a1, pre)
			if err != nil {
				return err
			}
			if err := writeLines(bw, ' ', a, from, s.a1); err != nil {
				return err
			}
		}
		for k := i; k <= j; k++ {
			s := segs[k]
			if s.match {
				if err := writeLines(bw, ' ', a, s.a0, s.a1); err != nil {
					return err
				}
				continue
			}
			if err := writeLines(bw, '-', a, s.a0, s.a1); err != nil {
				return err
			}
			if err := writeLines(bw, '+', b, s.b0, s.b1); err != nil {
				return err
			}
		}
		if post > 0 {
			s := segs[j+1]
			to, err := firstLines(a, s.a0, s.a1, post)
			if err != nil {
				return err
			}
			if err := writeLines(bw, ' ', a, s.a0, to); err != nil {
				return err
			}
		}
		i = j
	}
	return bw.Flush()
}

func hunkRange(start, n int64) string {
	if n == 0 {
		return fmt.Sprintf("%d,0", start)
	}
	if n == 1 {
		return fmt.Sprint(start + 1)
	}
	return fmt.Sprintf("%d,%d", start+1, n)
}

// countLines counts the lines of f[from:to], which starts at a line
// boundary; a last line without newline counts.
func countLines(f File, from, to int64) (int64, error) {
	buf := make([]byte, 64<<10)
	var n int64
	var last byte = '\n'
	for off := from; off < to; {
		k := min(int64(len(buf)), to-off)
		if _, err := f.R.ReadAt(buf[:k], off); err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		n += int64(bytes.Count(buf[:k], []byte{'\n'}))
		last = buf[k-1]
		off += k
	}
	if last != '\n' {
		n++
	}
	return n, nil
}

// lastLines returns the start of the last n lines of f[from:to].
func lastLines(f File, from, to, n int64) (int64, error) {
	pos := to
	for range n {
		// Skip the newline that ends the line before pos.
		i, err := lastNewline(f, from, pos-1)
		if err != nil {
			return 0, err
		}
		pos = i + 1
		if pos <= from {
			return from, nil
		}
	}
	return pos, nil
}

// firstLines returns the end of the first n lines of f[from:to].
func firstLines(f File, from, to, n int64) (int64, error) {
	pos := from
	for range n {
		i, err := indexNewline(f, pos, to)
		if err != nil {
			return 0, err
		}
		if i < 0 {
			return to, nil
		}
		pos = i + 1
	}
	return pos, nil
}

// writeLines writes the lines of f[from:to], each prefixed with p.
func writeLines(w *bufio.Writer, p byte, f File, from, to int64) error {
	if from == to {
		return nil
	}
	r := bufio.NewReaderSize(io.NewSectionReader(f.R, from, to-from), 64<<10)
	start := true
	var last byte
	for {
		b, err := r.ReadSlice('\n')
		if len(b) > 0 {
			if start {
				w.WriteByte(p)
			}
			w.Write(b)
			last = b[len(b)-1]
			start = last == '\n'
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	if last != '\n' {
		_, err := w.WriteString("\n\\ No newline at end of file\n")
		return err
	}
	return nil
}
