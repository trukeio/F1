package tree

import (
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"sort"
	"sync"
	"testing"

	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
	"github.com/trukeio/f1/internal/pack"
)

var ctx = context.Background()

// memNodes is an in-memory NodeStore that counts newly written nodes.
type memNodes struct {
	mu       sync.Mutex
	nodes    map[hash.Hash]*object.TreeNode
	newNodes int
	newBytes int
}

func newMem() *memNodes { return &memNodes{nodes: map[hash.Hash]*object.TreeNode{}} }

func (m *memNodes) GetNode(_ context.Context, h hash.Hash) (*object.TreeNode, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nodes[h]
	if !ok {
		return nil, fmt.Errorf("node %s not found", h.Short())
	}
	return n, nil
}

func (m *memNodes) PutNode(_ context.Context, n *object.TreeNode) (hash.Hash, error) {
	h, b, err := object.Hash(n)
	if err != nil {
		return hash.Zero, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.nodes[h]; !ok {
		m.nodes[h] = n
		m.newNodes++
		m.newBytes += len(b)
	}
	return h, nil
}

func (m *memNodes) resetCounts() { m.newNodes, m.newBytes = 0, 0 }

func entry(name string, version int) object.Entry {
	return object.Entry{Name: []byte(name), Kind: object.KindFile, Target: hash.Sum(1, fmt.Appendf(nil, "%s@%d", name, version))}
}

func build(t testing.TB, ns NodeStore, set map[string]object.Entry) hash.Hash {
	t.Helper()
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	entries := make([]object.Entry, len(names))
	for i, n := range names {
		entries[i] = set[n]
	}
	root, err := object.BuildTree(entries, func(n *object.TreeNode) (hash.Hash, error) { return ns.PutNode(ctx, n) })
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func randomName(rng *rand.Rand) string {
	// Mostly short names, some long ones (to exercise the byte cap and
	// size-dependent boundaries), with shared prefixes.
	n := fmt.Sprintf("dir%02d/file-%06d", rng.IntN(20), rng.IntN(1_000_000))
	if rng.IntN(50) == 0 {
		n += string(bytes.Repeat([]byte{'x'}, 2000+rng.IntN(4000)))
	}
	return n
}

// Update gives exactly the tree that BuildTree gives for the resulting set,
// whatever sequence of batches led to it.
func TestUpdateMatchesBuild(t *testing.T) {
	for seed := range uint64(8) {
		t.Run(fmt.Sprint("seed", seed), func(t *testing.T) {
			t.Parallel()
			rng := rand.New(rand.NewPCG(seed, 99))
			ns := newMem()
			set := map[string]object.Entry{}
			// Most trees have one or two levels; seed 0 is deep enough for
			// incremental updates of several interior levels.
			size := rng.IntN(3000)
			if seed == 0 {
				size = 300_000
				if raceEnabled {
					size = 30_000
				}
			}
			for range size {
				n := randomName(rng)
				set[n] = entry(n, 0)
			}
			root := build(t, ns, set)
			rounds := 40
			if seed == 0 {
				rounds = 10
			}
			for round := range rounds {
				var changes []Change
				names := make([]string, 0, len(set))
				for n := range set {
					names = append(names, n)
				}
				sort.Strings(names)
				for range 1 + rng.IntN(1+rng.IntN(400)) {
					switch op := rng.IntN(10); {
					case op < 4 || len(names) == 0: // insert
						n := randomName(rng)
						e := entry(n, round+1)
						set[n] = e
						changes = append(changes, Change{Name: []byte(n), Entry: &e})
					case op < 7: // modify
						n := names[rng.IntN(len(names))]
						e := entry(n, round+1)
						if rng.IntN(3) == 0 {
							e.Attrs = &object.Attrs{ContentType: "text/plain", MTime: int64(round + 1)}
						}
						set[n] = e
						changes = append(changes, Change{Name: []byte(n), Entry: &e})
					case op < 9: // delete
						n := names[rng.IntN(len(names))]
						delete(set, n)
						changes = append(changes, Change{Name: []byte(n)})
					default: // delete a missing name
						changes = append(changes, Change{Name: []byte("missing/" + randomName(rng))})
					}
				}
				// Apply in the given order; Update sorts, and for duplicate
				// names the last change wins, as in the set.
				var err error
				root, err = Update(ctx, ns, root, changes)
				if err != nil {
					t.Fatal(err)
				}
				if want := build(t, ns, set); root != want {
					t.Fatalf("round %d: Update root %s != BuildTree root %s (%d entries)", round, root.Short(), want.Short(), len(set))
				}
			}
		})
	}
}

// Deleting almost everything collapses the tree to the canonical root.
func TestUpdateCollapse(t *testing.T) {
	t.Parallel()
	ns := newMem()
	set := map[string]object.Entry{}
	for i := range 20000 {
		n := fmt.Sprintf("k%06d", i)
		set[n] = entry(n, 0)
	}
	root := build(t, ns, set)
	for _, keep := range []int{100, 10, 1, 0} {
		var changes []Change
		for n := range set {
			if len(set)-len(changes) > keep {
				changes = append(changes, Change{Name: []byte(n)})
			}
		}
		for _, c := range changes {
			delete(set, string(c.Name))
		}
		var err error
		if root, err = Update(ctx, ns, root, changes); err != nil {
			t.Fatal(err)
		}
		if want := build(t, ns, set); root != want {
			t.Fatalf("keep %d: root differs from BuildTree", keep)
		}
	}
	// And grows again from the empty tree.
	var changes []Change
	for i := range 5000 {
		n := fmt.Sprintf("n%06d", i)
		e := entry(n, 1)
		set[n] = e
		changes = append(changes, Change{Name: []byte(n), Entry: &e})
	}
	root, err := Update(ctx, ns, root, changes)
	if err != nil {
		t.Fatal(err)
	}
	if want := build(t, ns, set); root != want {
		t.Fatal("growing from empty differs from BuildTree")
	}
}

func TestLookupAndIter(t *testing.T) {
	ns := newMem()
	set := map[string]object.Entry{}
	for i := range 10000 {
		n := fmt.Sprintf("k%06d", i*2)
		set[n] = entry(n, 0)
	}
	root := build(t, ns, set)
	for _, i := range []int{0, 1, 2, 5001, 19998, 19999, 20000} {
		n := fmt.Sprintf("k%06d", i)
		e, ok, err := Lookup(ctx, ns, root, []byte(n))
		if err != nil {
			t.Fatal(err)
		}
		if want, exists := set[n]; ok != exists || (ok && e.Target != want.Target) {
			t.Errorf("Lookup(%s) = %v, want %v", n, ok, exists)
		}
	}
	it, err := Seek(ctx, ns, root, []byte("k010001"))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for e, ok := it.Next(); ok && len(got) < 3; e, ok = it.Next() {
		got = append(got, string(e.Name))
	}
	if fmt.Sprint(got) != "[k010002 k010004 k010006]" {
		t.Fatalf("Seek/Next = %v", got)
	}
	all, err := All(ctx, ns, root)
	if err != nil || len(all) != len(set) {
		t.Fatalf("All = %d entries, %v", len(all), err)
	}
	if !slices.IsSortedFunc(all, func(a, b object.Entry) int { return bytes.Compare(a.Name, b.Name) }) {
		t.Fatal("iteration is not sorted")
	}
	if n, _ := Count(ctx, ns, root); n != uint64(len(set)) {
		t.Fatalf("Count = %d", n)
	}
	empty, _ := Empty(ctx, ns)
	if _, ok, _ := Lookup(ctx, ns, empty, []byte("x")); ok {
		t.Fatal("lookup in the empty tree succeeded")
	}
}

// Diff equals the naive set difference.
func TestDiff(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(7, 7))
	ns := newMem()
	for round := range 30 {
		a := map[string]object.Entry{}
		for range rng.IntN(5000) {
			n := randomName(rng)
			a[n] = entry(n, 0)
		}
		b := map[string]object.Entry{}
		for n, e := range a {
			switch rng.IntN(20) {
			case 0: // deleted in b
			case 1:
				b[n] = entry(n, 1) // changed
			default:
				b[n] = e
			}
		}
		for range rng.IntN(200) {
			n := randomName(rng)
			b[n] = entry(n, 2)
		}
		want := map[string]string{}
		for n, e := range a {
			if f, ok := b[n]; !ok {
				want[n] = "deleted"
			} else if f.Target != e.Target {
				want[n] = "changed"
			}
		}
		for n := range b {
			if _, ok := a[n]; !ok {
				want[n] = "added"
			}
		}
		got := map[string]string{}
		var prev []byte
		err := Diff(ctx, ns, build(t, ns, a), build(t, ns, b), func(d DiffEntry) error {
			if prev != nil && bytes.Compare(prev, d.Name) >= 0 {
				t.Fatal("diff not in name order")
			}
			prev = d.Name
			switch {
			case d.Old == nil:
				got[string(d.Name)] = "added"
			case d.New == nil:
				got[string(d.Name)] = "deleted"
			default:
				got[string(d.Name)] = "changed"
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("round %d: diff has %d entries, naive %d", round, len(got), len(want))
		}
	}
}

func s3Attrs(i int) *object.Attrs {
	return &object.Attrs{
		ContentType: "application/octet-stream",
		ETag:        fmt.Sprintf(`"%032x"`, i),
		MTime:       1700000000000000000 + int64(i),
		Meta:        map[string]string{"uploaded-by": "backup-agent"},
	}
}

// Updating one key of a 1,000,000-entry flat tree with typical S3 attrs
// writes fewer than 10 nodes and less than 256 KiB.
func TestMillionEntryUpdate(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("large tree")
	}
	ns := newMem()
	n := 1_000_000
	if raceEnabled {
		n = 100_000
	}
	entries := make([]object.Entry, n)
	for i := range entries {
		name := fmt.Sprintf("backups/2026/host-%04d/object-%07d.bin", i%1000, i)
		entries[i] = object.Entry{Name: []byte(name), Kind: object.KindFile, Target: hash.Sum(1, []byte(name)), Attrs: s3Attrs(i)}
	}
	sort.Slice(entries, func(i, j int) bool { return bytes.Compare(entries[i].Name, entries[j].Name) < 0 })
	root, err := object.BuildTree(entries, func(nd *object.TreeNode) (hash.Hash, error) { return ns.PutNode(ctx, nd) })
	if err != nil {
		t.Fatal(err)
	}
	ns.resetCounts()
	e := entries[n/2]
	e.Target = hash.Sum(1, []byte("new content"))
	e.Attrs = s3Attrs(-1)
	if _, err := Update(ctx, ns, root, []Change{{Name: e.Name, Entry: &e}}); err != nil {
		t.Fatal(err)
	}
	t.Logf("one update wrote %d nodes, %d bytes (%d nodes in tree)", ns.newNodes, ns.newBytes, len(ns.nodes))
	if ns.newNodes >= 10 || ns.newBytes >= 256<<10 {
		t.Fatalf("one update wrote %d nodes and %d bytes", ns.newNodes, ns.newBytes)
	}
}

func leafShape(t *testing.T, ns *memNodes, root hash.Hash) []string {
	t.Helper()
	var shape []string
	var walk func(hash.Hash)
	walk = func(h hash.Hash) {
		n, err := ns.GetNode(ctx, h)
		if err != nil {
			t.Fatal(err)
		}
		if n.Level == 0 {
			shape = append(shape, fmt.Sprintf("%s+%d", n.Entries[0].Name, len(n.Entries)))
			return
		}
		for _, c := range n.Children {
			walk(c.Node)
		}
	}
	walk(root)
	return shape
}

// Changing targets or attributes never moves a node boundary.
func TestBoundariesIgnoreTargetsAndAttrs(t *testing.T) {
	t.Parallel()
	ns := newMem()
	set := map[string]object.Entry{}
	for i := range 20000 {
		n := fmt.Sprintf("f%06d", i)
		set[n] = entry(n, 0)
	}
	root := build(t, ns, set)
	before := leafShape(t, ns, root)
	var changes []Change
	for n := range set {
		e := entry(n, 1)
		e.Attrs = &object.Attrs{ContentType: "image/png", MTime: 42}
		changes = append(changes, Change{Name: []byte(n), Entry: &e})
	}
	root2, err := Update(ctx, ns, root, changes)
	if err != nil {
		t.Fatal(err)
	}
	if root2 == root {
		t.Fatal("root unchanged")
	}
	if after := leafShape(t, ns, root2); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatal("leaf boundaries moved")
	}
}

func TestNodesCache(t *testing.T) {
	ns := newMem()
	set := map[string]object.Entry{}
	for i := range 3000 {
		n := fmt.Sprintf("f%06d", i)
		set[n] = entry(n, 0)
	}
	root := build(t, ns, set)
	// Nodes over a store that only knows memNodes' contents.
	all, err := All(ctx, nodesOver(ns), root)
	if err != nil || len(all) != len(set) {
		t.Fatalf("All through Nodes = %d, %v", len(all), err)
	}
}

// recStore is an ObjectStore holding records in memory.
type recStore struct {
	mu   sync.Mutex
	recs map[hash.Hash]pack.Record
}

func (s *recStore) Has(_ context.Context, h hash.Hash) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.recs[h]
	return ok, nil
}

func (s *recStore) Put(_ context.Context, rec pack.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec.Data = bytes.Clone(rec.Data)
	s.recs[rec.Hash] = rec
	return nil
}

func (s *recStore) Get(_ context.Context, h hash.Hash) (pack.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.recs[h]
	if !ok {
		return pack.Record{}, fmt.Errorf("not found")
	}
	return rec, nil
}

// nodesOver copies every node of m into a record store and returns a small
// Nodes cache over it, so iteration exercises cache eviction.
func nodesOver(m *memNodes) *Nodes {
	rs := &recStore{recs: map[hash.Hash]pack.Record{}}
	nodes := NewNodes(rs, 4)
	for _, n := range m.nodes {
		if _, err := nodes.PutNode(ctx, n); err != nil {
			panic(err)
		}
	}
	return nodes
}
