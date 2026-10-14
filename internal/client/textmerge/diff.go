// Package textmerge implements line diffs, three-way merges and unified
// diffs of text files of any size with bounded memory (D19, ADR-0010).
//
// A diff works in two levels. First both files are cut into content-defined
// chunks (FastCDC, as F1 stores them), and equal chunks are matched; an edit
// changes only the chunks around it, so matched chunks cover all but a few
// small regions of a large file. The matches are shrunk to whole lines.
// Then each region between matches is diffed line by line, if it is at most
// RegionLimit bytes on both sides; a larger region is treated as replaced
// as a whole. Memory is proportional to the number of chunks (one entry per
// 64 KiB on average) and to RegionLimit, never to the file size.
package textmerge

import (
	"bytes"
	"context"
	"errors"
	"io"
	"unicode/utf8"

	"github.com/trukeio/f1/internal/chunk"
	"github.com/trukeio/f1/internal/hash"
)

// RegionLimit is the largest region, per side, that is diffed line by line.
// Files up to this size are diffed line by line without chunk matching.
const RegionLimit = 8 << 20

// regionLimit is RegionLimit, lowered by tests to exercise chunk matching
// with small files.
var regionLimit int64 = RegionLimit

// maxMatches bounds the matches of one diff. Beyond it, regions are no
// longer diffed line by line (they count as replaced).
const maxMatches = 1 << 22

// File is one version of a file.
type File struct {
	R io.ReaderAt
	N int64
}

// Bytes returns a File over b.
func Bytes(b []byte) File { return File{R: bytes.NewReader(b), N: int64(len(b))} }

// Match is a pair of equal byte ranges, A[A:A+N] and B[B:B+N], that start
// and end at line boundaries.
type Match struct {
	A, B, N int64
}

// IsText reports whether a file is text: its first 8 KiB are valid UTF-8 and
// contain no NUL byte.
func IsText(f File) (bool, error) {
	n := min(f.N, 8<<10)
	buf := make([]byte, n)
	if _, err := f.R.ReadAt(buf, 0); err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	if bytes.IndexByte(buf, 0) >= 0 {
		return false, nil
	}
	if n < f.N {
		// A rune may be cut at the end of the sample.
		for i := 0; i < utf8.UTFMax-1 && len(buf) > 0 && !utf8.FullRune(buf[lastRuneStart(buf):]); i++ {
			buf = buf[:lastRuneStart(buf)]
		}
	}
	return utf8.Valid(buf), nil
}

func lastRuneStart(b []byte) int {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			return i
		}
	}
	return len(b)
}

// Diff returns the matching line ranges of a and b, in increasing order.
// Everything outside them differs.
func Diff(ctx context.Context, a, b File) ([]Match, error) {
	d := &differ{ctx: ctx, a: a, b: b, ids: map[hash.Hash]int32{}}
	return d.run()
}

type differ struct {
	ctx  context.Context
	a, b File
	ids  map[hash.Hash]int32
	out  []Match
}

type span struct {
	off, n int64
}

// chunks cuts f into content-defined chunks and returns their ids (equal
// content, equal id) and byte ranges.
func (d *differ) chunks(f File) ([]int32, []span, error) {
	var ids []int32
	var spans []span
	c := chunk.New(io.NewSectionReader(f.R, 0, f.N))
	var off int64
	for {
		if err := d.ctx.Err(); err != nil {
			return nil, nil, err
		}
		b, err := c.Next()
		if errors.Is(err, io.EOF) {
			return ids, spans, nil
		}
		if err != nil {
			return nil, nil, err
		}
		h := hash.Plain(b)
		id, ok := d.ids[h]
		if !ok {
			id = int32(len(d.ids))
			d.ids[h] = id
		}
		ids = append(ids, id)
		spans = append(spans, span{off, int64(len(b))})
		off += int64(len(b))
	}
}

func (d *differ) run() ([]Match, error) {
	if d.a.N <= regionLimit && d.b.N <= regionLimit {
		// Small enough to diff line by line as a whole.
		err := d.region(0, d.a.N, 0, d.b.N)
		return d.out, err
	}
	ia, sa, err := d.chunks(d.a)
	if err != nil {
		return nil, err
	}
	ib, sb, err := d.chunks(d.b)
	if err != nil {
		return nil, err
	}
	d.ids = nil
	// Matched chunk runs, as byte ranges, shrunk to whole lines.
	var coarse []Match
	for _, r := range matchSeq(ia, ib) {
		m := Match{A: sa[r.x].off, B: sb[r.y].off}
		last := r.n - 1
		m.N = sa[r.x+last].off + sa[r.x+last].n - m.A
		m, ok, err := d.align(m)
		if err != nil {
			return nil, err
		}
		if ok {
			coarse = append(coarse, m)
		}
	}
	// Line diffs of the regions between them.
	var pa, pb int64
	for _, m := range coarse {
		if err := d.region(pa, m.A, pb, m.B); err != nil {
			return nil, err
		}
		d.add(m)
		pa, pb = m.A+m.N, m.B+m.N
	}
	if err := d.region(pa, d.a.N, pb, d.b.N); err != nil {
		return nil, err
	}
	return d.out, nil
}

// add appends a match, joining it to the previous one if adjacent.
func (d *differ) add(m Match) {
	if m.N == 0 {
		return
	}
	if k := len(d.out) - 1; k >= 0 && d.out[k].A+d.out[k].N == m.A && d.out[k].B+d.out[k].N == m.B {
		d.out[k].N += m.N
		return
	}
	d.out = append(d.out, m)
}

// lineStart reports whether off starts a line of f.
func lineStart(f File, off int64) (bool, error) {
	if off == 0 {
		return true, nil
	}
	var c [1]byte
	if _, err := f.R.ReadAt(c[:], off-1); err != nil {
		return false, err
	}
	return c[0] == '\n', nil
}

// align shrinks an equal range to the whole lines inside it.
func (d *differ) align(m Match) (Match, bool, error) {
	okA, err := lineStart(d.a, m.A)
	if err != nil {
		return m, false, err
	}
	okB, err := lineStart(d.b, m.B)
	if err != nil {
		return m, false, err
	}
	if !okA || !okB {
		i, err := indexNewline(d.a, m.A, m.A+m.N)
		if err != nil || i < 0 {
			return m, false, err
		}
		skip := i + 1 - m.A
		m.A, m.B, m.N = m.A+skip, m.B+skip, m.N-skip
	}
	// The end is a line boundary if the range ends with a newline, or at
	// the end of both files.
	if m.N > 0 && !(m.A+m.N == d.a.N && m.B+m.N == d.b.N) {
		i, err := lastNewline(d.a, m.A, m.A+m.N)
		if err != nil {
			return m, false, err
		}
		m.N = i + 1 - m.A // 0 if there is no newline
	}
	return m, m.N > 0, nil
}

// indexNewline returns the offset of the first newline in f[from:to], or -1.
func indexNewline(f File, from, to int64) (int64, error) {
	buf := make([]byte, 32<<10)
	for off := from; off < to; {
		n := min(int64(len(buf)), to-off)
		if _, err := f.R.ReadAt(buf[:n], off); err != nil && !errors.Is(err, io.EOF) {
			return -1, err
		}
		if i := bytes.IndexByte(buf[:n], '\n'); i >= 0 {
			return off + int64(i), nil
		}
		off += n
	}
	return -1, nil
}

// lastNewline returns the offset of the last newline in f[from:to], or
// from-1.
func lastNewline(f File, from, to int64) (int64, error) {
	buf := make([]byte, 32<<10)
	for end := to; end > from; {
		n := min(int64(len(buf)), end-from)
		if _, err := f.R.ReadAt(buf[:n], end-n); err != nil && !errors.Is(err, io.EOF) {
			return -1, err
		}
		if i := bytes.LastIndexByte(buf[:n], '\n'); i >= 0 {
			return end - n + int64(i), nil
		}
		end -= n
	}
	return from - 1, nil
}

// region diffs a[a0:a1] and b[b0:b1], which start and end at line
// boundaries, line by line, unless one side is larger than RegionLimit.
func (d *differ) region(a0, a1, b0, b1 int64) error {
	if a0 == a1 || b0 == b1 || a1-a0 > regionLimit || b1-b0 > regionLimit || len(d.out) >= maxMatches {
		return nil
	}
	if err := d.ctx.Err(); err != nil {
		return err
	}
	ra := make([]byte, a1-a0)
	if _, err := d.a.R.ReadAt(ra, a0); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	rb := make([]byte, b1-b0)
	if _, err := d.b.R.ReadAt(rb, b0); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	ids := map[string]int32{}
	la, oa := lineIDs(ra, ids)
	lb, ob := lineIDs(rb, ids)
	for _, r := range matchSeq(la, lb) {
		start := oa[r.x]
		d.add(Match{A: a0 + start, B: b0 + ob[r.y], N: oa[r.x+r.n] - start})
	}
	return nil
}

// lineIDs splits b into lines (with their newline) and returns an id per
// line, equal for equal lines, and the line start offsets followed by
// len(b).
func lineIDs(b []byte, ids map[string]int32) ([]int32, []int64) {
	var out []int32
	offs := []int64{0}
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n') + 1
		if i == 0 {
			i = len(b)
		}
		id, ok := ids[string(b[:i])]
		if !ok {
			id = int32(len(ids))
			ids[string(b[:i])] = id
		}
		out = append(out, id)
		offs = append(offs, offs[len(offs)-1]+int64(i))
		b = b[i:]
	}
	return out, offs
}

// Equal reports whether a[a0:a0+n] equals b[b0:b0+n].
func equal(a File, a0 int64, b File, b0 int64, n int64) (bool, error) {
	ba := make([]byte, min(n, 64<<10))
	bb := make([]byte, len(ba))
	for n > 0 {
		k := min(n, int64(len(ba)))
		if _, err := a.R.ReadAt(ba[:k], a0); err != nil && !errors.Is(err, io.EOF) {
			return false, err
		}
		if _, err := b.R.ReadAt(bb[:k], b0); err != nil && !errors.Is(err, io.EOF) {
			return false, err
		}
		if !bytes.Equal(ba[:k], bb[:k]) {
			return false, nil
		}
		a0, b0, n = a0+k, b0+k, n-k
	}
	return true, nil
}
