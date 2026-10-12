package object

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/trukeio/f1/internal/hash"
)

// Prolly tree parameters (D20). Part of the on-disk format.
const (
	// NodeMinEntries is the minimum number of items before a content-defined
	// boundary is honored.
	NodeMinEntries = 16
	// NodeMaxEntries forces a split.
	NodeMaxEntries = 512
	// NodeMaxBytes forces a split when the encoded items of a node reach it.
	NodeMaxBytes = 128 << 10
	// boundaryMask selects the low 6 bits of the first byte of the boundary
	// hash: about 1 in 64 items ends a node.
	boundaryMask = 0x3F
)

// Kind is the kind of a tree entry.
type Kind uint8

// Entry kinds. Values are part of the on-disk format.
const (
	KindFile    Kind = 1 // regular file; target is a File
	KindExec    Kind = 2 // executable file; target is a File
	KindSymlink Kind = 3 // symbolic link; target is a File holding the link text
	KindDir     Kind = 4 // directory (tree layout only); target is a Tree root
	KindGitlink Kind = 5 // Git submodule; target is zero, attrs.git_sha1 holds the commit
	KindLFS     Kind = 6 // Git LFS file; target is a File, attrs.lfs_pointer holds the pointer
)

// Attrs are optional entry attributes, a CBOR map with string keys. An entry
// without attributes omits the map; an empty map is not canonical.
type Attrs struct {
	// S3 attributes; they do not affect Git export.
	ContentType string            `cbor:"ct,omitempty"`
	Meta        map[string]string `cbor:"meta,omitempty"`
	ETag        string            `cbor:"etag,omitempty"`
	MTime       int64             `cbor:"mtime,omitempty"` // Unix ns
	Checksums   map[string][]byte `cbor:"cks,omitempty"`   // algorithm name → raw checksum

	// Git mapping attributes.
	Exec       bool       `cbor:"exec,omitempty"`        // lfs entries only
	GitSHA1    []byte     `cbor:"git_sha1,omitempty"`    // gitlink entries only
	LFSPointer []byte     `cbor:"lfs_pointer,omitempty"` // lfs entries only
	GitRaw     *hash.Hash `cbor:"git_raw,omitempty"`     // dir entries only
}

func (a *Attrs) empty() bool {
	return a.ContentType == "" && len(a.Meta) == 0 && a.ETag == "" && a.MTime == 0 &&
		len(a.Checksums) == 0 && !a.Exec && len(a.GitSHA1) == 0 &&
		len(a.LFSPointer) == 0 && a.GitRaw == nil
}

// Entry is a leaf entry. Encoded as a map {0: name, 1: kind, 2: target, 3: attrs?}.
type Entry struct {
	Name   []byte    `cbor:"0,keyasint"`
	Kind   Kind      `cbor:"1,keyasint"`
	Target hash.Hash `cbor:"2,keyasint"`
	Attrs  *Attrs    `cbor:"3,keyasint,omitempty"`
}

// Child is an interior node item. Encoded as [first_name, node, count].
type Child struct {
	_         struct{} `cbor:",toarray"`
	FirstName []byte
	Node      hash.Hash
	Count     uint64 // number of leaf entries below Node
}

// TreeNode is a prolly tree node. Level 0 nodes hold Entries, higher levels
// hold Children. Encoded as a map {0: level, 1: entries?, 2: children?}.
type TreeNode struct {
	Level    uint64  `cbor:"0,keyasint"`
	Entries  []Entry `cbor:"1,keyasint,omitempty"`
	Children []Child `cbor:"2,keyasint,omitempty"`
}

func (*TreeNode) Type() Type { return TypeTree }

// Validate checks the TreeNode invariants.
func (n *TreeNode) Validate() error {
	if n.Level == 0 {
		if len(n.Children) > 0 {
			return errors.New("leaf node with children")
		}
		if len(n.Entries) > NodeMaxEntries {
			return fmt.Errorf("%d entries > %d", len(n.Entries), NodeMaxEntries)
		}
		for i := range n.Entries {
			if err := n.Entries[i].validate(); err != nil {
				return fmt.Errorf("entry %d: %w", i, err)
			}
			if i > 0 && bytes.Compare(n.Entries[i-1].Name, n.Entries[i].Name) >= 0 {
				return fmt.Errorf("entry %d out of order", i)
			}
		}
		return nil
	}
	if len(n.Entries) > 0 {
		return errors.New("interior node with entries")
	}
	if len(n.Children) == 0 {
		return errors.New("interior node without children")
	}
	if len(n.Children) > NodeMaxEntries {
		return fmt.Errorf("%d children > %d", len(n.Children), NodeMaxEntries)
	}
	for i, c := range n.Children {
		if len(c.FirstName) == 0 {
			return fmt.Errorf("child %d has empty first name", i)
		}
		if c.Count == 0 || c.Node.IsZero() {
			return fmt.Errorf("child %d is empty", i)
		}
		if i > 0 && bytes.Compare(n.Children[i-1].FirstName, c.FirstName) >= 0 {
			return fmt.Errorf("child %d out of order", i)
		}
	}
	return nil
}

func (e *Entry) validate() error {
	if len(e.Name) == 0 {
		return errors.New("empty name")
	}
	if e.Kind < KindFile || e.Kind > KindLFS {
		return fmt.Errorf("unknown kind %d", e.Kind)
	}
	if e.Kind == KindGitlink {
		if !e.Target.IsZero() {
			return errors.New("gitlink with target")
		}
	} else if e.Target.IsZero() {
		return errors.New("zero target")
	}
	a := e.Attrs
	if a == nil {
		a = &Attrs{}
	} else if a.empty() {
		return errors.New("empty attrs map")
	}
	if (len(a.GitSHA1) > 0) != (e.Kind == KindGitlink) {
		return errors.New("git_sha1 is required for and only allowed on gitlink entries")
	}
	if len(a.GitSHA1) > 0 && len(a.GitSHA1) != 20 {
		return errors.New("git_sha1 must be 20 bytes")
	}
	if (len(a.LFSPointer) > 0) != (e.Kind == KindLFS) {
		return errors.New("lfs_pointer is required for and only allowed on lfs entries")
	}
	if a.Exec && e.Kind != KindLFS {
		return errors.New("exec is only allowed on lfs entries")
	}
	if a.GitRaw != nil && (e.Kind != KindDir || a.GitRaw.IsZero()) {
		return errors.New("git_raw is only allowed on dir entries")
	}
	return nil
}

// LeafBoundary reports whether a leaf node may end after an entry with this
// name: BLAKE3(name)[0] & 0x3F == 0.
func LeafBoundary(name []byte) bool {
	h := hash.Plain(name)
	return h[0]&boundaryMask == 0
}

// InteriorBoundary reports whether an interior node at level (>= 1) may end
// after a child with this first name: BLAKE3(uint8(level) ‖ first_name)[0] &
// 0x3F == 0.
func InteriorBoundary(level uint64, firstName []byte) bool {
	buf := make([]byte, 0, 1+len(firstName))
	buf = append(buf, byte(level))
	buf = append(buf, firstName...)
	h := hash.Plain(buf)
	return h[0]&boundaryMask == 0
}

// SplitLevel splits n items of one tree level into nodes and returns the
// item count of each node. Walking left to right, an item is added to the
// current node and the node ends after it when the node has at least
// NodeMinEntries items and boundary(i) holds, when it has NodeMaxEntries
// items, or when the encoded sizes of its items sum to NodeMaxBytes or more.
// The last node takes the remaining items.
func SplitLevel(n int, boundary func(i int) bool, size func(i int) int) []int {
	var counts []int
	count, nbytes := 0, 0
	for i := 0; i < n; i++ {
		count++
		nbytes += size(i)
		if (count >= NodeMinEntries && boundary(i)) || count == NodeMaxEntries || nbytes >= NodeMaxBytes {
			counts = append(counts, count)
			count, nbytes = 0, 0
		}
	}
	if count > 0 {
		counts = append(counts, count)
	}
	return counts
}

// BuildTree builds a prolly tree from entries sorted by name and returns the
// root hash. put stores each node and returns its hash. An empty entry list
// yields an empty leaf. Levels are built bottom-up until a level forms a
// single node, which is the root.
func BuildTree(entries []Entry, put func(*TreeNode) (hash.Hash, error)) (hash.Hash, error) {
	if len(entries) == 0 {
		return put(&TreeNode{})
	}
	sizes := make([]int, len(entries))
	for i := range entries {
		b, err := encMode.Marshal(&entries[i])
		if err != nil {
			return hash.Zero, err
		}
		sizes[i] = len(b)
	}
	counts := SplitLevel(len(entries),
		func(i int) bool { return LeafBoundary(entries[i].Name) },
		func(i int) int { return sizes[i] })
	children := make([]Child, 0, len(counts))
	off := 0
	for _, c := range counts {
		node := &TreeNode{Entries: entries[off : off+c]}
		h, err := put(node)
		if err != nil {
			return hash.Zero, err
		}
		children = append(children, Child{FirstName: entries[off].Name, Node: h, Count: uint64(c)})
		off += c
	}
	for level := uint64(1); ; level++ {
		if len(children) == 1 {
			return children[0].Node, nil
		}
		sizes := make([]int, len(children))
		for i := range children {
			b, err := encMode.Marshal(&children[i])
			if err != nil {
				return hash.Zero, err
			}
			sizes[i] = len(b)
		}
		counts := SplitLevel(len(children),
			func(i int) bool { return InteriorBoundary(level, children[i].FirstName) },
			func(i int) int { return sizes[i] })
		next := make([]Child, 0, len(counts))
		off := 0
		for _, c := range counts {
			group := children[off : off+c]
			var total uint64
			for _, ch := range group {
				total += ch.Count
			}
			h, err := put(&TreeNode{Level: level, Children: group})
			if err != nil {
				return hash.Zero, err
			}
			next = append(next, Child{FirstName: group[0].FirstName, Node: h, Count: total})
			off += c
		}
		children = next
	}
}
