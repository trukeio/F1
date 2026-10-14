package textmerge

import "sort"

// run is a matched run: x[x:x+n] equals y[y:y+n].
type run struct {
	x, y, n int
}

// Limits of the Myers fallback, which runs where patience diff finds no
// anchor. Beyond them the region counts as replaced, which is correct but
// coarse; such regions consist of repeated elements only.
const (
	myersMaxLen = 1 << 16 // elements on both sides together
	myersMaxD   = 1 << 11 // edit distance
)

// matchSeq returns the matched runs of x and y in increasing order. It uses
// patience diff: elements unique in both sequences anchor the alignment
// (their longest increasing subsequence), and the gaps between anchors are
// diffed recursively; gaps without unique elements fall back to Myers'
// linear-space algorithm.
func matchSeq(x, y []int32) []run {
	m := &seqMatcher{x: x, y: y}
	m.diff(0, len(x), 0, len(y))
	return m.out
}

type seqMatcher struct {
	x, y []int32
	out  []run
}

func (m *seqMatcher) emit(x, y, n int) {
	if n == 0 {
		return
	}
	if k := len(m.out) - 1; k >= 0 && m.out[k].x+m.out[k].n == x && m.out[k].y+m.out[k].n == y {
		m.out[k].n += n
		return
	}
	m.out = append(m.out, run{x, y, n})
}

func (m *seqMatcher) diff(x0, x1, y0, y1 int) {
	// Common prefix and suffix.
	p := 0
	for x0+p < x1 && y0+p < y1 && m.x[x0+p] == m.y[y0+p] {
		p++
	}
	m.emit(x0, y0, p)
	x0, y0 = x0+p, y0+p
	s := 0
	for x1-s > x0 && y1-s > y0 && m.x[x1-s-1] == m.y[y1-s-1] {
		s++
	}
	x1, y1 = x1-s, y1-s
	if x0 < x1 && y0 < y1 {
		if anchors := m.anchors(x0, x1, y0, y1); len(anchors) > 0 {
			px, py := x0, y0
			for _, a := range anchors {
				m.diff(px, a.x, py, a.y)
				m.emit(a.x, a.y, 1)
				px, py = a.x+1, a.y+1
			}
			m.diff(px, x1, py, y1)
		} else {
			m.myers(x0, x1, y0, y1)
		}
	}
	m.emit(x1, y1, s)
}

type pair struct{ x, y int }

// anchors returns the longest increasing sequence of elements that occur
// exactly once in x[x0:x1] and once in y[y0:y1].
func (m *seqMatcher) anchors(x0, x1, y0, y1 int) []pair {
	type count struct{ nx, ny, x, y int }
	counts := map[int32]*count{}
	for i := x0; i < x1; i++ {
		c := counts[m.x[i]]
		if c == nil {
			c = &count{}
			counts[m.x[i]] = c
		}
		c.nx++
		c.x = i
	}
	for i := y0; i < y1; i++ {
		c := counts[m.y[i]]
		if c == nil {
			continue
		}
		c.ny++
		c.y = i
	}
	var cands []pair
	for _, c := range counts {
		if c.nx == 1 && c.ny == 1 {
			cands = append(cands, pair{c.x, c.y})
		}
	}
	if len(cands) == 0 {
		return nil
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].x < cands[j].x })
	return lis(cands)
}

// lis returns the longest subsequence of ps (sorted by x) increasing in y
// (patience sorting).
func lis(ps []pair) []pair {
	var tails []int // index in ps of the smallest tail of each length
	prev := make([]int, len(ps))
	for i, p := range ps {
		k := sort.Search(len(tails), func(j int) bool { return ps[tails[j]].y >= p.y })
		if k > 0 {
			prev[i] = tails[k-1]
		} else {
			prev[i] = -1
		}
		if k == len(tails) {
			tails = append(tails, i)
		} else {
			tails[k] = i
		}
	}
	out := make([]pair, len(tails))
	for i, k := len(tails)-1, tails[len(tails)-1]; i >= 0; i, k = i-1, prev[k] {
		out[i] = ps[k]
	}
	return out
}

// myers diffs x[x0:x1] and y[y0:y1], which differ at both ends, with
// Myers' linear-space algorithm (divide and conquer on the middle snake).
// Regions over the limits count as replaced.
func (m *seqMatcher) myers(x0, x1, y0, y1 int) {
	if (x1-x0)+(y1-y0) > myersMaxLen {
		return
	}
	size := 2*((x1-x0)+(y1-y0)) + 3
	vf := make([]int, size)
	vb := make([]int, size)
	// Sub-problems have smaller edit distances, so only the top level can
	// exceed myersMaxD.
	if _, _, _, _, ok := m.middleSnake(x0, x1, y0, y1, vf, vb); !ok {
		return
	}
	m.lcs(x0, x1, y0, y1, vf, vb)
}

func (m *seqMatcher) lcs(x0, x1, y0, y1 int, vf, vb []int) {
	p := 0
	for x0+p < x1 && y0+p < y1 && m.x[x0+p] == m.y[y0+p] {
		p++
	}
	m.emit(x0, y0, p)
	x0, y0 = x0+p, y0+p
	s := 0
	for x1-s > x0 && y1-s > y0 && m.x[x1-s-1] == m.y[y1-s-1] {
		s++
	}
	x1, y1 = x1-s, y1-s
	if x0 < x1 && y0 < y1 {
		sx, sy, ex, ey, _ := m.middleSnake(x0, x1, y0, y1, vf, vb)
		m.lcs(x0, sx, y0, sy, vf, vb)
		m.emit(sx, sy, ex-sx)
		m.lcs(ex, x1, ey, y1, vf, vb)
	}
	m.emit(x1, y1, s)
}

// middleSnake finds the middle snake of a shortest edit script of
// x[x0:x1] and y[y0:y1]: its start and end, or ok = false if the edit
// distance exceeds myersMaxD.
func (m *seqMatcher) middleSnake(x0, x1, y0, y1 int, vf, vb []int) (sx, sy, ex, ey int, ok bool) {
	n, mm := x1-x0, y1-y0
	delta := n - mm
	odd := delta&1 != 0
	off := n + mm + 1 // index of diagonal 0
	maxD := min((n+mm+1)/2, myersMaxD)
	vf[off+1] = 0
	vb[off+1] = 0
	for d := 0; d <= maxD; d++ {
		// Forward: the furthest x on each diagonal k = x - y.
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && vf[off+k-1] < vf[off+k+1]) {
				x = vf[off+k+1]
			} else {
				x = vf[off+k-1] + 1
			}
			y := x - k
			bx, by := x, y
			for x < n && y < mm && m.x[x0+x] == m.y[y0+y] {
				x++
				y++
			}
			vf[off+k] = x
			// The reverse diagonal delta - k, reached in d-1 steps.
			if c := delta - k; odd && c >= -(d-1) && c <= d-1 && x+vb[off+c] >= n {
				return x0 + bx, y0 + by, x0 + x, y0 + y, true
			}
		}
		// Backward, in reversed coordinates (distance from the end).
		for c := -d; c <= d; c += 2 {
			var x int
			if c == -d || (c != d && vb[off+c-1] < vb[off+c+1]) {
				x = vb[off+c+1]
			} else {
				x = vb[off+c-1] + 1
			}
			y := x - c
			bx, by := x, y
			for x < n && y < mm && m.x[x1-x-1] == m.y[y1-y-1] {
				x++
				y++
			}
			vb[off+c] = x
			if k := delta - c; !odd && k >= -d && k <= d && x+vf[off+k] >= n {
				return x1 - x, y1 - y, x1 - bx, y1 - by, true
			}
		}
	}
	return 0, 0, 0, 0, false
}
