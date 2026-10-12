// Package tree implements the prolly tree operations on Tree objects:
// lookup, ordered iteration, incremental update and diff.
//
// Update produces exactly the tree that object.BuildTree builds from the
// resulting entry set (history independence), while writing only the nodes
// on the paths of the changes: O(changes × log n) nodes.
package tree

import (
	"bytes"
	"container/list"
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
	"github.com/trukeio/f1/internal/pack"
)

// NodeStore reads and writes tree nodes.
type NodeStore interface {
	GetNode(ctx context.Context, h hash.Hash) (*object.TreeNode, error)
	PutNode(ctx context.Context, n *object.TreeNode) (hash.Hash, error)
}

// ObjectStore is the subset of the pack store that Nodes needs.
type ObjectStore interface {
	Has(ctx context.Context, h hash.Hash) (bool, error)
	Put(ctx context.Context, rec pack.Record) error
	Get(ctx context.Context, h hash.Hash) (pack.Record, error)
}

// Nodes is a NodeStore over an object store, with an LRU cache of decoded
// nodes. Cached nodes are shared and must not be modified.
type Nodes struct {
	store ObjectStore
	mu    sync.Mutex
	max   int
	lru   *list.List
	byKey map[hash.Hash]*list.Element
}

type cached struct {
	h hash.Hash
	n *object.TreeNode
}

// NewNodes returns a NodeStore caching up to size decoded nodes.
func NewNodes(store ObjectStore, size int) *Nodes {
	return &Nodes{store: store, max: size, lru: list.New(), byKey: map[hash.Hash]*list.Element{}}
}

// GetNode implements NodeStore.
func (s *Nodes) GetNode(ctx context.Context, h hash.Hash) (*object.TreeNode, error) {
	s.mu.Lock()
	if el, ok := s.byKey[h]; ok {
		s.lru.MoveToFront(el)
		n := el.Value.(cached).n
		s.mu.Unlock()
		return n, nil
	}
	s.mu.Unlock()
	rec, err := s.store.Get(ctx, h)
	if err != nil {
		return nil, err
	}
	if rec.Type != object.TypeTree || rec.Hash != h {
		return nil, fmt.Errorf("tree: object %s is a %s, not a tree node", h.Short(), rec.Type)
	}
	p, err := rec.Payload(nil)
	if err != nil {
		return nil, err
	}
	n, err := object.DecodeAs[*object.TreeNode](p)
	if err != nil {
		return nil, err
	}
	s.add(h, n)
	return n, nil
}

// PutNode implements NodeStore.
func (s *Nodes) PutNode(ctx context.Context, n *object.TreeNode) (hash.Hash, error) {
	h, b, err := object.Hash(n)
	if err != nil {
		return hash.Zero, err
	}
	ok, err := s.store.Has(ctx, h)
	if err != nil {
		return hash.Zero, err
	}
	if !ok {
		rec, err := pack.NewRecord(object.TypeTree, h, b, nil)
		if err != nil {
			return hash.Zero, err
		}
		if err := s.store.Put(ctx, rec); err != nil {
			return hash.Zero, err
		}
	}
	s.add(h, n)
	return h, nil
}

func (s *Nodes) add(h hash.Hash, n *object.TreeNode) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if el, ok := s.byKey[h]; ok {
		s.lru.MoveToFront(el)
		return
	}
	s.byKey[h] = s.lru.PushFront(cached{h, n})
	for s.lru.Len() > s.max {
		el := s.lru.Back()
		s.lru.Remove(el)
		delete(s.byKey, el.Value.(cached).h)
	}
}

// Empty stores the empty tree and returns its hash.
func Empty(ctx context.Context, ns NodeStore) (hash.Hash, error) {
	return ns.PutNode(ctx, &object.TreeNode{})
}

// Lookup returns the entry with the given name.
func Lookup(ctx context.Context, ns NodeStore, root hash.Hash, name []byte) (object.Entry, bool, error) {
	n, err := ns.GetNode(ctx, root)
	if err != nil {
		return object.Entry{}, false, err
	}
	for n.Level > 0 {
		i := childFor(n, name)
		if i < 0 {
			return object.Entry{}, false, nil
		}
		if n, err = ns.GetNode(ctx, n.Children[i].Node); err != nil {
			return object.Entry{}, false, err
		}
	}
	i := sort.Search(len(n.Entries), func(i int) bool { return bytes.Compare(n.Entries[i].Name, name) >= 0 })
	if i < len(n.Entries) && bytes.Equal(n.Entries[i].Name, name) {
		return n.Entries[i], true, nil
	}
	return object.Entry{}, false, nil
}

// childFor returns the index of the last child whose first name is <= key,
// or -1 if key sorts before every child.
func childFor(n *object.TreeNode, key []byte) int {
	i := sort.Search(len(n.Children), func(i int) bool { return bytes.Compare(n.Children[i].FirstName, key) > 0 })
	return i - 1
}

// Count returns the number of entries in the tree.
func Count(ctx context.Context, ns NodeStore, root hash.Hash) (uint64, error) {
	n, err := ns.GetNode(ctx, root)
	if err != nil {
		return 0, err
	}
	if n.Level == 0 {
		return uint64(len(n.Entries)), nil
	}
	var total uint64
	for _, c := range n.Children {
		total += c.Count
	}
	return total, nil
}

// Iter iterates over a tree's entries in name order.
type Iter struct {
	ctx   context.Context
	ns    NodeStore
	stack []frame
	err   error
}

type frame struct {
	n *object.TreeNode
	i int
}

// Seek returns an iterator positioned at the first entry whose name is >=
// from (nil: the first entry).
func Seek(ctx context.Context, ns NodeStore, root hash.Hash, from []byte) (*Iter, error) {
	it := &Iter{ctx: ctx, ns: ns}
	n, err := ns.GetNode(ctx, root)
	if err != nil {
		return nil, err
	}
	for n.Level > 0 {
		i := max(childFor(n, from), 0)
		it.stack = append(it.stack, frame{n, i})
		if n, err = ns.GetNode(ctx, n.Children[i].Node); err != nil {
			return nil, err
		}
	}
	i := sort.Search(len(n.Entries), func(i int) bool { return bytes.Compare(n.Entries[i].Name, from) >= 0 })
	it.stack = append(it.stack, frame{n, i})
	return it, nil
}

// Next returns the next entry, or false at the end or on error (see Err).
func (it *Iter) Next() (object.Entry, bool) {
	for len(it.stack) > 0 {
		top := &it.stack[len(it.stack)-1]
		if top.n.Level == 0 {
			if top.i < len(top.n.Entries) {
				e := top.n.Entries[top.i]
				top.i++
				return e, true
			}
			it.stack = it.stack[:len(it.stack)-1]
			continue
		}
		top.i++
		if top.i >= len(top.n.Children) {
			it.stack = it.stack[:len(it.stack)-1]
			continue
		}
		// Descend to the leftmost leaf of the next child.
		n, err := it.ns.GetNode(it.ctx, top.n.Children[top.i].Node)
		if err != nil {
			it.err = err
			it.stack = nil
			return object.Entry{}, false
		}
		for n.Level > 0 {
			it.stack = append(it.stack, frame{n, 0})
			if n, err = it.ns.GetNode(it.ctx, n.Children[0].Node); err != nil {
				it.err = err
				it.stack = nil
				return object.Entry{}, false
			}
		}
		it.stack = append(it.stack, frame{n, 0})
	}
	return object.Entry{}, false
}

// Err returns the error that stopped iteration, if any.
func (it *Iter) Err() error { return it.err }

// All returns every entry of the tree; intended for tests and small trees.
func All(ctx context.Context, ns NodeStore, root hash.Hash) ([]object.Entry, error) {
	it, err := Seek(ctx, ns, root, nil)
	if err != nil {
		return nil, err
	}
	var out []object.Entry
	for {
		e, ok := it.Next()
		if !ok {
			return out, it.Err()
		}
		out = append(out, e)
	}
}
