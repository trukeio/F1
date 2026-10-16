package repo

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
	"github.com/trukeio/f1/internal/policy"
)

// The history benchmarks of ADR-0015 run on a generated history of
// F1_BENCH_COMMITS commits (default 2,000; the ADR's measurement uses
// 60,000) over 5,000 files in 100 directories. Each commit changes two
// random files. "rare.txt" is written by the first commit only, and the
// branch "side" forks off 10,000 commits (or half the history) before the
// head, with 10 commits of its own.
type benchHistory struct {
	r          *Repo
	head, side hash.Hash
	child      hash.Hash // a commit on top of head, as on a new branch
}

var (
	benchOnce sync.Once
	benchHist *benchHistory
	benchDir  string
	benchErr  error
)

// TestMain removes the shared benchmark history after the run.
func TestMain(m *testing.M) {
	code := m.Run()
	if benchHist != nil {
		benchHist.r.Close(ctx)
	}
	if benchDir != "" {
		os.RemoveAll(benchDir)
	}
	os.Exit(code)
}

func history(b *testing.B) *benchHistory {
	b.Helper()
	benchOnce.Do(func() { benchHist, benchErr = buildHistory(b) })
	if benchErr != nil {
		b.Fatal(benchErr)
	}
	return benchHist
}

func buildHistory(b *testing.B) (*benchHistory, error) {
	n := 2000
	if v := os.Getenv("F1_BENCH_COMMITS"); v != "" {
		var err error
		if n, err = strconv.Atoi(v); err != nil {
			return nil, err
		}
	}
	// The repository is shared by the benchmarks of a run, so it is not
	// in a b.TempDir; TestMain removes it.
	dir, err := os.MkdirTemp("", "f1-history-bench-")
	if err != nil {
		return nil, err
	}
	benchDir = dir
	path := filepath.Join(dir, "ns")
	if err := Create(path, policy.Default()); err != nil {
		return nil, err
	}
	r, _, err := Open(ctx, path, Options{})
	if err != nil {
		return nil, err
	}
	rng := rand.New(rand.NewPCG(1, 2))
	entry := func(s string) (object.Entry, error) {
		res, err := r.Ingest(ctx, strings.NewReader(s), int64(len(s)))
		return object.Entry{Kind: object.KindFile, Target: res.File}, err
	}
	var changes []Change
	for d := range 100 {
		for f := range 50 {
			e, err := entry(fmt.Sprintf("file %d/%d v0", d, f))
			if err != nil {
				return nil, err
			}
			changes = append(changes, Change{Op: OpPut, Path: fmt.Sprintf("d%03d/f%02d.txt", d, f), Entry: e})
		}
	}
	e, err := entry("rare")
	if err != nil {
		return nil, err
	}
	changes = append(changes, Change{Op: OpPut, Path: "rare.txt", Entry: e})
	commit := func(ref string, parent hash.Hash, changes []Change) (hash.Hash, error) {
		return r.Commit(ctx, CommitRequest{Ref: ref, Parent: &parent, Changes: changes, Author: alice, Message: "bench"})
	}
	head, err := commit("main", hash.Zero, changes)
	if err != nil {
		return nil, err
	}
	fork := n - 10000
	if fork < n/2 {
		fork = n / 2
	}
	var forkAt hash.Hash
	for i := 1; i < n; i++ {
		var cs []Change
		for range 2 {
			d, f := rng.IntN(100), rng.IntN(50)
			e, err := entry(fmt.Sprintf("file %d/%d v%d", d, f, i))
			if err != nil {
				return nil, err
			}
			cs = append(cs, Change{Op: OpPut, Path: fmt.Sprintf("d%03d/f%02d.txt", d, f), Entry: e})
		}
		if head, err = commit("main", head, cs); err != nil {
			return nil, err
		}
		if i == fork {
			forkAt = head
		}
	}
	h := &benchHistory{r: r, head: head}
	branch := func(name string, at hash.Hash) error {
		return r.UpdateRefs(ctx, []RefUpdate{{Ref: "heads/" + name, New: at}}, UpdateOptions{Principal: "bench", Reason: "branch"})
	}
	if err := branch("side", forkAt); err != nil {
		return nil, err
	}
	side := forkAt
	for i := range 10 {
		e, err := entry(fmt.Sprint("side ", i))
		if err != nil {
			return nil, err
		}
		if side, err = commit("side", side, []Change{{Op: OpPut, Path: "side.txt", Entry: e}}); err != nil {
			return nil, err
		}
	}
	h.side = side
	if err := branch("child", head); err != nil {
		return nil, err
	}
	if e, err = entry("child"); err != nil {
		return nil, err
	}
	if h.child, err = commit("child", head, []Change{{Op: OpPut, Path: "child.txt", Entry: e}}); err != nil {
		return nil, err
	}
	return h, nil
}

// cold runs the first query of a benchmark and reports its time.
func cold(b *testing.B, fn func() error) {
	b.Helper()
	coldMu.Lock()
	defer coldMu.Unlock()
	if coldDone[b.Name()] == 0 {
		// Creating the benchmark's branches computed rows; start empty.
		if err := benchHist.r.resetGraph(ctx); err != nil {
			b.Fatal(err)
		}
		start := time.Now()
		if err := fn(); err != nil {
			b.Fatal(err)
		}
		coldDone[b.Name()] = float64(time.Since(start).Microseconds()) / 1000
	}
	// Reported after the loop: ResetTimer clears reported metrics.
	b.Cleanup(func() { b.ReportMetric(coldDone[b.Name()], "cold-ms") })
}

var (
	coldMu   sync.Mutex
	coldDone = map[string]float64{}
)

// BenchmarkMergeBaseNewBranch is the merge base computed when a branch is
// created from the head of the default branch.
//
// The first query, on an empty commit graph, computes the rows of the
// whole history; its time is reported as cold-ms.
func BenchmarkMergeBaseNewBranch(b *testing.B) {
	h := history(b)
	cold(b, func() error { _, err := h.r.MergeBase(ctx, h.child, h.head); return err })
	b.ResetTimer()
	for b.Loop() {
		mb, err := h.r.MergeBase(ctx, h.child, h.head)
		if err != nil || mb != h.head {
			b.Fatalf("merge base = %s, %v", mb.Short(), err)
		}
	}
}

// BenchmarkMergeBaseApart is the merge base of two branches 10,000
// commits apart.
func BenchmarkMergeBaseApart(b *testing.B) {
	h := history(b)
	b.ResetTimer()
	for b.Loop() {
		if _, err := h.r.MergeBase(ctx, h.side, h.head); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkLogRarePath is log of a path changed only by the first commit,
// limit 20: the walk covers the whole history.
//
// The first query, on an empty commit graph, computes the rows and path
// filters of the whole history; its time is reported as cold-ms.
func BenchmarkLogRarePath(b *testing.B) {
	h := history(b)
	cold(b, func() error { _, err := h.r.Log(ctx, h.head, "rare.txt", 20); return err })
	b.ResetTimer()
	for b.Loop() {
		log, err := h.r.Log(ctx, h.head, "rare.txt", 20)
		if err != nil || len(log) != 1 {
			b.Fatalf("log = %d, %v", len(log), err)
		}
	}
}
