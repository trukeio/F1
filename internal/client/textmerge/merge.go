package textmerge

import (
	"bufio"
	"context"
	"errors"
	"io"
)

// Conflict markers.
const (
	MarkerMine   = "<<<<<<< mine\n"
	MarkerSep    = "=======\n"
	MarkerTheirs = ">>>>>>> theirs\n"
)

// Merge writes the three-way merge of mine and theirs, two versions derived
// from base, to w. Regions changed on one side take that side's version;
// regions changed on both sides in the same way are taken once; other
// regions are conflicts, written between markers. It returns the number of
// conflicts.
func Merge(ctx context.Context, base, mine, theirs File, w io.Writer) (int, error) {
	ma, err := Diff(ctx, base, mine)
	if err != nil {
		return 0, err
	}
	mb, err := Diff(ctx, base, theirs)
	if err != nil {
		return 0, err
	}
	bw := bufio.NewWriterSize(w, 64<<10)
	m := &merger{base: base, a: mine, b: theirs, w: bw}
	conflicts := 0
	// Stable regions: base ranges matched in both versions.
	var po, pa, pb int64
	flush := func(so, sa, sb int64) error {
		c, err := m.unstable(po, so, pa, sa, pb, sb)
		conflicts += c
		return err
	}
	for i, j := 0, 0; i < len(ma) && j < len(mb); {
		x, y := ma[i], mb[j]
		s, e := max(x.A, y.A), min(x.A+x.N, y.A+y.N)
		if s < e {
			sa, sb := x.B+s-x.A, y.B+s-y.A
			if err := flush(s, sa, sb); err != nil {
				return 0, err
			}
			if err := m.copy(mine, sa, e-s); err != nil {
				return 0, err
			}
			po, pa, pb = e, sa+e-s, sb+e-s
		}
		if x.A+x.N < y.A+y.N {
			i++
		} else {
			j++
		}
	}
	if err := flush(base.N, mine.N, theirs.N); err != nil {
		return 0, err
	}
	return conflicts, bw.Flush()
}

type merger struct {
	base, a, b File
	w          *bufio.Writer
}

// unstable resolves the region between two stable regions.
func (m *merger) unstable(o0, o1, a0, a1, b0, b1 int64) (int, error) {
	if o0 == o1 && a0 == a1 && b0 == b1 {
		return 0, nil
	}
	aSame, err := m.same(m.base, o0, o1, m.a, a0, a1)
	if err != nil {
		return 0, err
	}
	bSame, err := m.same(m.base, o0, o1, m.b, b0, b1)
	if err != nil {
		return 0, err
	}
	switch {
	case aSame:
		return 0, m.copy(m.b, b0, b1-b0)
	case bSame:
		return 0, m.copy(m.a, a0, a1-a0)
	}
	abSame, err := m.same(m.a, a0, a1, m.b, b0, b1)
	if err != nil {
		return 0, err
	}
	if abSame {
		return 0, m.copy(m.a, a0, a1-a0)
	}
	m.w.WriteString(MarkerMine)
	if err := m.copyLines(m.a, a0, a1); err != nil {
		return 0, err
	}
	m.w.WriteString(MarkerSep)
	if err := m.copyLines(m.b, b0, b1); err != nil {
		return 0, err
	}
	_, err = m.w.WriteString(MarkerTheirs)
	return 1, err
}

func (m *merger) same(x File, x0, x1 int64, y File, y0, y1 int64) (bool, error) {
	if x1-x0 != y1-y0 {
		return false, nil
	}
	return equal(x, x0, y, y0, x1-x0)
}

func (m *merger) copy(f File, off, n int64) error {
	_, err := io.Copy(m.w, io.NewSectionReader(f.R, off, n))
	return err
}

// copyLines copies f[from:to] and ends it with a newline, so that a marker
// that follows starts a line.
func (m *merger) copyLines(f File, from, to int64) error {
	if err := m.copy(f, from, to-from); err != nil {
		return err
	}
	if from < to {
		var c [1]byte
		if _, err := f.R.ReadAt(c[:], to-1); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if c[0] != '\n' {
			return m.w.WriteByte('\n')
		}
	}
	return nil
}
