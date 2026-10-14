package textmerge

import (
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var ctx = context.Background()

func merge(t *testing.T, base, mine, theirs string) (string, int) {
	t.Helper()
	var out bytes.Buffer
	n, err := Merge(ctx, Bytes([]byte(base)), Bytes([]byte(mine)), Bytes([]byte(theirs)), &out)
	if err != nil {
		t.Fatal(err)
	}
	return out.String(), n
}

func TestMerge(t *testing.T) {
	base := "a\nb\nc\nd\ne\nf\ng\n"
	for _, tc := range []struct {
		name, mine, theirs, want string
		conflicts                int
	}{
		{"one side", "a\nB\nc\nd\ne\nf\ng\n", base, "a\nB\nc\nd\ne\nf\ng\n", 0},
		{"other side", base, "a\nb\nc\nd\ne\nF\ng\n", "a\nb\nc\nd\ne\nF\ng\n", 0},
		{"both, apart", "a\nB\nc\nd\ne\nf\ng\n", "a\nb\nc\nd\ne\nF\ng\n", "a\nB\nc\nd\ne\nF\ng\n", 0},
		{"same change", "a\nb\nX\nd\ne\nf\ng\n", "a\nb\nX\nd\ne\nf\ng\n", "a\nb\nX\nd\ne\nf\ng\n", 0},
		{"conflict", "a\nb\nMINE\nd\ne\nf\ng\n", "a\nb\nTHEIRS\nd\ne\nf\ng\n",
			"a\nb\n" + MarkerMine + "MINE\n" + MarkerSep + "THEIRS\n" + MarkerTheirs + "d\ne\nf\ng\n", 1},
		{"insert and delete", "a\nb\nc\nnew\nd\ne\nf\ng\n", "a\nb\nc\nd\ne\ng\n", "a\nb\nc\nnew\nd\ne\ng\n", 0},
		{"append both", base + "m\n", base + "t\n", base + MarkerMine + "m\n" + MarkerSep + "t\n" + MarkerTheirs, 1},
		{"no final newline", "a\nb\nc\nd\ne\nf\ng\nh", "A\nb\nc\nd\ne\nf\ng\n", "A\nb\nc\nd\ne\nf\ng\nh", 0},
		{"conflict without newline", "x", "y", MarkerMine + "x\n" + MarkerSep + "y\n" + MarkerTheirs, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := base
			if tc.name == "conflict without newline" {
				b = ""
			}
			got, n := merge(t, b, tc.mine, tc.theirs)
			if got != tc.want || n != tc.conflicts {
				t.Fatalf("got %d conflicts:\n%s\nwant %d:\n%s", n, got, tc.conflicts, tc.want)
			}
		})
	}
}

// Random edits that do not overlap merge to the result of applying both.
func TestMergeRandom(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for iter := range 300 {
		n := 20 + r.IntN(200)
		lines := make([]string, n)
		for i := range lines {
			lines[i] = fmt.Sprintf("line %d %d\n", i, r.IntN(5))
		}
		if r.IntN(3) == 0 {
			// Repeated lines exercise the Myers fallback.
			for i := range lines {
				lines[i] = fmt.Sprintf("%d\n", r.IntN(3))
			}
		}
		// Mine edits the first half, theirs the second, with a gap.
		mine := append([]string(nil), lines...)
		theirs := append([]string(nil), lines...)
		mine[r.IntN(n/2-2)] = "MINE\n"
		theirs[n/2+1+r.IntN(n/2-2)] = "THEIRS\n"
		want := append([]string(nil), mine...)
		for i := range theirs {
			if theirs[i] != lines[i] {
				want[i] = theirs[i]
			}
		}
		got, c := merge(t, strings.Join(lines, ""), strings.Join(mine, ""), strings.Join(theirs, ""))
		if c != 0 || got != strings.Join(want, "") {
			// Repeated lines may align ambiguously; a conflict is then
			// acceptable, but a clean merge must be right.
			if c == 0 {
				t.Fatalf("iteration %d: wrong clean merge", iter)
			}
		}
	}
}

// Matches are equal, line-aligned and increasing; for sequences without
// unique elements the Myers result is a longest common subsequence.
func TestDiffProperties(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	for range 300 {
		a := randomLines(r, r.IntN(60), 3)
		b := randomLines(r, r.IntN(60), 3)
		ms, err := Diff(ctx, Bytes(a), Bytes(b))
		if err != nil {
			t.Fatal(err)
		}
		var pa, pb, total int64
		for _, m := range ms {
			if m.A < pa || m.B < pb || !bytes.Equal(a[m.A:m.A+m.N], b[m.B:m.B+m.N]) {
				t.Fatalf("bad match %+v", m)
			}
			if (m.A > 0 && a[m.A-1] != '\n') || (m.B > 0 && b[m.B-1] != '\n') || a[m.A+m.N-1] != '\n' {
				t.Fatalf("match not line-aligned %+v", m)
			}
			pa, pb = m.A+m.N, m.B+m.N
			total += int64(bytes.Count(a[m.A:m.A+m.N], []byte{'\n'}))
		}
		if want := lcsLen(bytes.SplitAfter(a, []byte{'\n'}), bytes.SplitAfter(b, []byte{'\n'})); total != int64(want) {
			t.Fatalf("matched %d lines, LCS is %d", total, want)
		}
	}
}

func randomLines(r *rand.Rand, n, alphabet int) []byte {
	var b []byte
	for range n {
		b = append(b, byte('a'+r.IntN(alphabet)), '\n')
	}
	return b
}

func lcsLen(a, b [][]byte) int {
	a, b = trimEmpty(a), trimEmpty(b)
	dp := make([][]int, len(a)+1)
	for i := range dp {
		dp[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if bytes.Equal(a[i], b[j]) {
				dp[i][j] = dp[i+1][j+1] + 1
			} else {
				dp[i][j] = max(dp[i+1][j], dp[i][j+1])
			}
		}
	}
	return dp[0][0]
}

func trimEmpty(x [][]byte) [][]byte {
	if len(x) > 0 && len(x[len(x)-1]) == 0 {
		return x[:len(x)-1]
	}
	return x
}

// textOf generates n bytes of numbered lines.
func textOf(n int) []byte {
	var b bytes.Buffer
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "this is line number %d of a large text file\n", i)
	}
	return b.Bytes()[:n]
}

// A large file merges through chunk-level matches: edits at both ends.
func TestMergeLarge(t *testing.T) {
	base := textOf(40 << 20) // over RegionLimit: chunk-level matching
	mine := bytes.Replace(base, []byte("line number 1000 of"), []byte("LINE NUMBER 1000 OF"), 1)
	theirs := bytes.Replace(base, []byte("line number 800000 of"), []byte("LINE NUMBER 800000 OF"), 1)
	want := bytes.Replace(mine, []byte("line number 800000 of"), []byte("LINE NUMBER 800000 OF"), 1)
	var out bytes.Buffer
	n, err := Merge(ctx, Bytes(base), Bytes(mine), Bytes(theirs), &out)
	if err != nil || n != 0 || !bytes.Equal(out.Bytes(), want) {
		t.Fatalf("merge: %d conflicts, %v, equal %v", n, err, bytes.Equal(out.Bytes(), want))
	}
	// A region over RegionLimit changed on both sides is one conflict.
	mine = append(bytes.ToUpper(base[:RegionLimit+100]), base[RegionLimit+100:]...)
	theirs = append(bytes.ReplaceAll(base[:RegionLimit+100], []byte("line"), []byte("LINE")), base[RegionLimit+100:]...)
	out.Reset()
	n, err = Merge(ctx, Bytes(base), Bytes(mine), Bytes(theirs), &out)
	if err != nil || n != 1 {
		t.Fatalf("coarse merge: %d conflicts, %v", n, err)
	}
}

func TestIsText(t *testing.T) {
	for _, tc := range []struct {
		data string
		want bool
	}{
		{"", true},
		{"hello\n", true},
		{"héllo", true},
		{"a\x00b", false},
		{"\xff\xfe", false},
		{strings.Repeat("a", 8191) + "é" + "\x00", true}, // rune cut at 8 KiB, NUL after it
	} {
		got, err := IsText(Bytes([]byte(tc.data)))
		if err != nil || got != tc.want {
			t.Errorf("IsText(%.20q) = %v, %v", tc.data, got, err)
		}
	}
}

// The unified diff matches GNU diff for unambiguous edits.
func TestUnified(t *testing.T) {
	if _, err := exec.LookPath("diff"); err != nil {
		t.Skip("diff is not installed")
	}
	base := textOf(4000)
	cases := map[string][]byte{
		"middle": bytes.Replace(base, []byte("number 30 "), []byte("NUMBER 30 "), 1),
		"two hunks": bytes.Replace(bytes.Replace(base, []byte("number 3 "), []byte("NUMBER 3 "), 1),
			[]byte("number 60 "), []byte("NUMBER 60 "), 1),
		"joined": bytes.Replace(bytes.Replace(base, []byte("number 10 "), []byte("NUMBER 10 "), 1),
			[]byte("number 15 "), []byte("NUMBER 15 "), 1),
		"delete":        bytes.Replace(base, []byte("this is line number 20 of a large text file\n"), nil, 1),
		"no newline":    append(append([]byte(nil), base...), "tail"...),
		"empty to text": base,
	}
	dir := t.TempDir()
	for name, b := range cases {
		a := base
		if name == "empty to text" {
			a = nil
		}
		var out bytes.Buffer
		if err := Unified(ctx, Bytes(a), Bytes(b), "a", "b", &out); err != nil {
			t.Fatal(err)
		}
		pa, pb := filepath.Join(dir, "a"), filepath.Join(dir, "b")
		os.WriteFile(pa, a, 0o644)
		os.WriteFile(pb, b, 0o644)
		want, _ := exec.Command("diff", "-u", "--label", "a", "--label", "b", pa, pb).Output()
		if out.String() != string(want) {
			t.Errorf("%s: got\n%s\nwant\n%s", name, out.String(), want)
		}
	}
	var out bytes.Buffer
	if err := Unified(ctx, Bytes(base), Bytes(base), "a", "b", &out); err != nil || out.Len() != 0 {
		t.Fatalf("equal files: %q, %v", out.String(), err)
	}
}

// FuzzMerge checks that diffs are valid and that a change on one side
// merges to that side, for arbitrary input.
func FuzzMerge(f *testing.F) {
	f.Add([]byte("a\nb\nc\n"), []byte("a\nB\nc\n"))
	f.Add([]byte(""), []byte("x"))
	f.Add([]byte("a\na\na\n"), []byte("a\nb\na\na\n"))
	f.Fuzz(func(t *testing.T, base, other []byte) {
		ms, err := Diff(ctx, Bytes(base), Bytes(other))
		if err != nil {
			t.Fatal(err)
		}
		var pa, pb int64
		for _, m := range ms {
			if m.N <= 0 || m.A < pa || m.B < pb || m.A+m.N > int64(len(base)) || m.B+m.N > int64(len(other)) ||
				!bytes.Equal(base[m.A:m.A+m.N], other[m.B:m.B+m.N]) {
				t.Fatalf("bad match %+v", m)
			}
			pa, pb = m.A+m.N, m.B+m.N
		}
		for _, sides := range [][2][]byte{{other, base}, {base, other}} {
			var out bytes.Buffer
			n, err := Merge(ctx, Bytes(base), Bytes(sides[0]), Bytes(sides[1]), &out)
			if err != nil || n != 0 || !bytes.Equal(out.Bytes(), other) {
				t.Fatalf("one-sided merge: %d conflicts, %v:\n%q", n, err, out.Bytes())
			}
		}
	})
}

// Random edits in files diffed through chunk matching (a low region
// limit) merge like the line-level diff.
func TestMergeChunked(t *testing.T) {
	defer func(l int64) { regionLimit = l }(regionLimit)
	// Over twice the largest chunk, so that a changed chunk is diffed line
	// by line.
	regionLimit = 512 << 10
	r := rand.New(rand.NewPCG(5, 6))
	for iter := range 20 {
		n := 30000 + r.IntN(30000) // about 1-3 MiB: many chunks
		lines := make([]string, n)
		for i := range lines {
			lines[i] = fmt.Sprintf("line %d of the chunked merge test %d\n", i, r.IntN(1000))
		}
		// Edits in separate thirds, so that no chunk holds both.
		mine := append([]string(nil), lines...)
		theirs := append([]string(nil), lines...)
		for range 1 + r.IntN(20) {
			mine[r.IntN(n/3)] = "MINE\n"
			theirs[2*n/3+r.IntN(n/3)] = "THEIRS\n"
		}
		want := append([]string(nil), mine...)
		for i := range theirs {
			if theirs[i] != lines[i] {
				want[i] = theirs[i]
			}
		}
		got, c := merge(t, strings.Join(lines, ""), strings.Join(mine, ""), strings.Join(theirs, ""))
		if c != 0 || got != strings.Join(want, "") {
			t.Fatalf("iteration %d: %d conflicts, equal %v", iter, c, got == strings.Join(want, ""))
		}
	}
}
