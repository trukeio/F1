package tree

import (
	"bytes"
	"context"

	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
)

// DiffEntry is one difference between two trees: Old == nil for an added
// entry, New == nil for a deleted one, both set for a changed one.
type DiffEntry struct {
	Name []byte
	Old  *object.Entry
	New  *object.Entry
}

// elem is a pending part of one side of a diff: a subtree or an entry.
type elem struct {
	node    hash.Hash
	level   int // node level; -1 for an entry
	entry   object.Entry
	isEntry bool
}

// side is a stack of pending elements; the front is the last element.
type side []elem

func (s side) front() elem { return s[len(s)-1] }

func (s *side) pop() { *s = (*s)[:len(*s)-1] }

// expand replaces the front node with its items.
func (s *side) expand(ctx context.Context, ns NodeStore) error {
	x := s.front()
	s.pop()
	n, err := ns.GetNode(ctx, x.node)
	if err != nil {
		return err
	}
	if n.Level == 0 {
		for i := len(n.Entries) - 1; i >= 0; i-- {
			*s = append(*s, elem{level: -1, entry: n.Entries[i], isEntry: true})
		}
		return nil
	}
	for i := len(n.Children) - 1; i >= 0; i-- {
		*s = append(*s, elem{node: n.Children[i].Node, level: int(n.Level) - 1})
	}
	return nil
}

// Diff calls fn for every difference between the trees a and b, in name
// order. Subtrees with equal hashes on both sides are skipped, so the cost
// is proportional to the changes, not to the tree size.
func Diff(ctx context.Context, ns NodeStore, a, b hash.Hash, fn func(DiffEntry) error) error {
	if a == b {
		return nil
	}
	ra, err := ns.GetNode(ctx, a)
	if err != nil {
		return err
	}
	rb, err := ns.GetNode(ctx, b)
	if err != nil {
		return err
	}
	sa := side{{node: a, level: int(ra.Level)}}
	sb := side{{node: b, level: int(rb.Level)}}
	for len(sa) > 0 && len(sb) > 0 {
		x, y := sa.front(), sb.front()
		switch {
		case !x.isEntry && !y.isEntry && x.node == y.node:
			sa.pop()
			sb.pop()
		case x.isEntry && y.isEntry:
			switch c := bytes.Compare(x.entry.Name, y.entry.Name); {
			case c < 0:
				sa.pop()
				if err := fn(DiffEntry{Name: x.entry.Name, Old: &x.entry}); err != nil {
					return err
				}
			case c > 0:
				sb.pop()
				if err := fn(DiffEntry{Name: y.entry.Name, New: &y.entry}); err != nil {
					return err
				}
			default:
				sa.pop()
				sb.pop()
				if !entryEqual(&x.entry, &y.entry) {
					if err := fn(DiffEntry{Name: x.entry.Name, Old: &x.entry, New: &y.entry}); err != nil {
						return err
					}
				}
			}
		case x.level >= y.level:
			if err := sa.expand(ctx, ns); err != nil {
				return err
			}
		default:
			if err := sb.expand(ctx, ns); err != nil {
				return err
			}
		}
	}
	for len(sa) > 0 {
		if x := sa.front(); !x.isEntry {
			if err := sa.expand(ctx, ns); err != nil {
				return err
			}
			continue
		}
		x := sa.front()
		sa.pop()
		if err := fn(DiffEntry{Name: x.entry.Name, Old: &x.entry}); err != nil {
			return err
		}
	}
	for len(sb) > 0 {
		if y := sb.front(); !y.isEntry {
			if err := sb.expand(ctx, ns); err != nil {
				return err
			}
			continue
		}
		y := sb.front()
		sb.pop()
		if err := fn(DiffEntry{Name: y.entry.Name, New: &y.entry}); err != nil {
			return err
		}
	}
	return nil
}

// entryEqual compares entries by their canonical encoding.
func entryEqual(a, b *object.Entry) bool {
	if a.Kind != b.Kind || a.Target != b.Target || !bytes.Equal(a.Name, b.Name) {
		return false
	}
	if (a.Attrs == nil) != (b.Attrs == nil) {
		return false
	}
	if a.Attrs == nil {
		return true
	}
	ea, errA := object.Encode(&object.TreeNode{Entries: []object.Entry{*a}})
	eb, errB := object.Encode(&object.TreeNode{Entries: []object.Entry{*b}})
	return errA == nil && errB == nil && bytes.Equal(ea, eb)
}
