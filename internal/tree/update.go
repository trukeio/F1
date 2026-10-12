package tree

import (
	"bytes"
	"context"
	"fmt"
	"sort"

	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
)

// Change sets (Entry != nil) or deletes (Entry == nil) the entry Name.
// Deleting a missing name is a no-op.
type Change struct {
	Name  []byte
	Entry *object.Entry // Entry.Name is ignored; Name is used
}

// item is one element of a tree level: an entry at level 0, a child above.
type item struct {
	key   []byte
	entry object.Entry
	child object.Child
}

// mod replaces (it != nil) or deletes (it == nil) the item with key.
type mod struct {
	key []byte
	it  *item
}

// Update applies changes to the tree at root and returns the new root. The
// result equals object.BuildTree of the resulting entries. Only nodes on the
// paths of the changes are rewritten: each level is re-chunked from the
// start of the first affected node until the new node boundaries line up
// with the old ones again.
func Update(ctx context.Context, ns NodeStore, root hash.Hash, changes []Change) (hash.Hash, error) {
	if len(changes) == 0 {
		return root, nil
	}
	mods := make([]mod, 0, len(changes))
	for _, c := range changes {
		m := mod{key: c.Name}
		if c.Entry != nil {
			e := *c.Entry
			e.Name = c.Name
			m.it = &item{key: c.Name, entry: e}
		}
		mods = append(mods, m)
	}
	mods = normalize(mods)

	rootNode, err := ns.GetNode(ctx, root)
	if err != nil {
		return hash.Zero, err
	}
	u := &updater{ctx: ctx, ns: ns, root: rootNode, rootHash: root}

	// Levels below the root: incremental.
	level := uint64(0)
	for ; level < rootNode.Level; level++ {
		segs, err := u.processLevel(level, mods)
		if err != nil {
			return hash.Zero, err
		}
		mods = parentMods(segs)
		if len(mods) == 0 {
			// Nothing changed at this level, so nothing changes above it.
			return root, nil
		}
	}
	// The root level: its single node, changed, gives the full list of new
	// nodes at this level.
	segs, err := u.processLevel(level, mods)
	if err != nil {
		return hash.Zero, err
	}
	var nodes []object.Child
	for _, s := range segs {
		nodes = append(nodes, s.created...)
	}
	// Build the levels above until one node remains.
	for len(nodes) > 1 {
		level++
		nodes, err = u.buildLevel(level, nodes)
		if err != nil {
			return hash.Zero, err
		}
	}
	if len(nodes) == 0 {
		return Empty(ctx, ns)
	}
	// Strip single-child interior roots: the root is the first level that
	// forms a single node.
	h := nodes[0].Node
	for {
		n, err := ns.GetNode(ctx, h)
		if err != nil {
			return hash.Zero, err
		}
		if n.Level == 0 || len(n.Children) > 1 {
			return h, nil
		}
		h = n.Children[0].Node
	}
}

// normalize sorts mods by key; for duplicate keys the last one wins.
func normalize(mods []mod) []mod {
	sort.SliceStable(mods, func(i, j int) bool { return bytes.Compare(mods[i].key, mods[j].key) < 0 })
	out := mods[:0]
	for i, m := range mods {
		if i+1 < len(mods) && bytes.Equal(mods[i+1].key, m.key) {
			continue
		}
		out = append(out, m)
	}
	return out
}

type updater struct {
	ctx      context.Context
	ns       NodeStore
	root     *object.TreeNode
	rootHash hash.Hash
}

// segment is a run of consecutive old nodes at one level that was replaced
// by newly built nodes.
type segment struct {
	replaced [][]byte       // first keys of the replaced old nodes
	created  []object.Child // new nodes, as items of the parent level
}

// parentMods turns the segments of one level into mods of the level above:
// delete the replaced nodes' items, then put the created ones.
func parentMods(segs []segment) []mod {
	var mods []mod
	byKey := map[string]int{}
	for _, s := range segs {
		for _, k := range s.replaced {
			byKey[string(k)] = len(mods)
			mods = append(mods, mod{key: k})
		}
	}
	for _, s := range segs {
		for _, c := range s.created {
			it := &item{key: c.FirstName, child: c}
			if i, ok := byKey[string(c.FirstName)]; ok {
				mods[i].it = it
				continue
			}
			mods = append(mods, mod{key: c.FirstName, it: it})
		}
	}
	return normalize(mods)
}

// cursor walks the old nodes of one level from left to right.
type cursor struct {
	u     *updater
	level uint64
	path  []frame // path[0] is the root; the last frame is the current node
}

func (u *updater) seek(level uint64, key []byte) (*cursor, error) {
	c := &cursor{u: u, level: level}
	n := u.root
	for n.Level > level {
		i := max(childFor(n, key), 0)
		c.path = append(c.path, frame{n, i})
		var err error
		if n, err = u.ns.GetNode(u.ctx, n.Children[i].Node); err != nil {
			return nil, err
		}
	}
	c.path = append(c.path, frame{n, 0})
	return c, nil
}

func (c *cursor) node() *object.TreeNode { return c.path[len(c.path)-1].n }

// nextFirstKey returns the first key of the next node at this level, or nil
// at the end of the level, without loading the node.
func (c *cursor) nextFirstKey() []byte {
	for d := len(c.path) - 2; d >= 0; d-- {
		f := c.path[d]
		if f.i+1 < len(f.n.Children) {
			return f.n.Children[f.i+1].FirstName
		}
	}
	return nil
}

// next moves to the next node at this level; false at the end.
func (c *cursor) next() (bool, error) {
	d := len(c.path) - 2
	for d >= 0 && c.path[d].i+1 >= len(c.path[d].n.Children) {
		d--
	}
	if d < 0 {
		return false, nil
	}
	c.path = c.path[:d+1]
	c.path[d].i++
	n, err := c.u.ns.GetNode(c.u.ctx, c.path[d].n.Children[c.path[d].i].Node)
	if err != nil {
		return false, err
	}
	for n.Level > c.level {
		c.path = append(c.path, frame{n, 0})
		if n, err = c.u.ns.GetNode(c.u.ctx, n.Children[0].Node); err != nil {
			return false, err
		}
	}
	c.path = append(c.path, frame{n, 0})
	return true, nil
}

func nodeItems(n *object.TreeNode) []item {
	if n.Level == 0 {
		out := make([]item, len(n.Entries))
		for i, e := range n.Entries {
			out[i] = item{key: e.Name, entry: e}
		}
		return out
	}
	out := make([]item, len(n.Children))
	for i, c := range n.Children {
		out[i] = item{key: c.FirstName, child: c}
	}
	return out
}

func firstKey(n *object.TreeNode) []byte {
	if n.Level == 0 {
		if len(n.Entries) == 0 {
			return nil
		}
		return n.Entries[0].Name
	}
	return n.Children[0].FirstName
}

// processLevel applies mods (sorted, unique) to the old nodes of a level and
// returns the replaced segments. A mod belongs to the old node that would
// contain its key: the last node whose first key is <= the key, or the
// first node.
func (u *updater) processLevel(level uint64, mods []mod) ([]segment, error) {
	var segs []segment
	mi := 0
	for mi < len(mods) {
		cur, err := u.seek(level, mods[mi].key)
		if err != nil {
			return nil, err
		}
		seg := segment{}
		ch := &chunker{u: u, level: level}
		for {
			n := cur.node()
			if fk := firstKey(n); fk != nil {
				seg.replaced = append(seg.replaced, fk)
			}
			// Mods up to the next node's first key belong to this node.
			limit := cur.nextFirstKey()
			for _, it := range nodeItems(n) {
				for mi < len(mods) && bytes.Compare(mods[mi].key, it.key) < 0 {
					if err := ch.addMod(mods[mi]); err != nil {
						return nil, err
					}
					mi++
				}
				if mi < len(mods) && bytes.Equal(mods[mi].key, it.key) {
					if err := ch.addMod(mods[mi]); err != nil {
						return nil, err
					}
					mi++
					continue
				}
				if err := ch.add(it); err != nil {
					return nil, err
				}
			}
			for mi < len(mods) && (limit == nil || bytes.Compare(mods[mi].key, limit) < 0) {
				if err := ch.addMod(mods[mi]); err != nil {
					return nil, err
				}
				mi++
			}
			if limit == nil {
				// End of the level.
				if err := ch.flush(); err != nil {
					return nil, err
				}
				break
			}
			if !ch.pending() {
				// The new nodes end exactly where the old node ended, and
				// both chunkings restart here: the rest of the level is
				// unchanged up to the next mod.
				break
			}
			ok, err := cur.next()
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, fmt.Errorf("tree: level %d ended before its last node", level)
			}
		}
		seg.created = ch.out
		segs = append(segs, seg)
	}
	return segs, nil
}

// buildLevel builds a level from the complete, ordered list of the nodes
// below it.
func (u *updater) buildLevel(level uint64, below []object.Child) ([]object.Child, error) {
	ch := &chunker{u: u, level: level}
	for _, c := range below {
		if err := ch.add(item{key: c.FirstName, child: c}); err != nil {
			return nil, err
		}
	}
	if err := ch.flush(); err != nil {
		return nil, err
	}
	return ch.out, nil
}

// chunker groups the items of one level into nodes using the boundary rules
// of object.SplitLevel.
type chunker struct {
	u      *updater
	level  uint64
	items  []item
	nbytes int
	out    []object.Child
}

func (c *chunker) addMod(m mod) error {
	if m.it == nil {
		return nil
	}
	return c.add(*m.it)
}

func (c *chunker) add(it item) error {
	var size int
	var err error
	var boundary bool
	if c.level == 0 {
		size, err = object.EntrySize(&it.entry)
		boundary = object.LeafBoundary(it.key)
	} else {
		size, err = object.ChildSize(&it.child)
		boundary = object.InteriorBoundary(c.level, it.key)
	}
	if err != nil {
		return err
	}
	c.items = append(c.items, it)
	c.nbytes += size
	if (len(c.items) >= object.NodeMinEntries && boundary) || len(c.items) == object.NodeMaxEntries || c.nbytes >= object.NodeMaxBytes {
		return c.close()
	}
	return nil
}

func (c *chunker) pending() bool { return len(c.items) > 0 }

func (c *chunker) flush() error {
	if len(c.items) == 0 {
		return nil
	}
	return c.close()
}

func (c *chunker) close() error {
	n := &object.TreeNode{Level: c.level}
	var count uint64
	if c.level == 0 {
		n.Entries = make([]object.Entry, len(c.items))
		for i, it := range c.items {
			n.Entries[i] = it.entry
		}
		count = uint64(len(c.items))
	} else {
		n.Children = make([]object.Child, len(c.items))
		for i, it := range c.items {
			n.Children[i] = it.child
			count += it.child.Count
		}
	}
	h, err := c.u.ns.PutNode(c.u.ctx, n)
	if err != nil {
		return err
	}
	c.out = append(c.out, object.Child{FirstName: c.items[0].key, Node: h, Count: count})
	c.items = nil
	c.nbytes = 0
	return nil
}
