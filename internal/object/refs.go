package object

import "github.com/trukeio/f1/internal/hash"

// References returns the hashes of the objects that o references, for the
// storage invariant D9. Zero hashes (gitlink targets) are not references.
func References(o Object) []hash.Hash {
	var out []hash.Hash
	add := func(h hash.Hash) {
		if !h.IsZero() {
			out = append(out, h)
		}
	}
	switch o := o.(type) {
	case *File:
		for _, r := range o.Refs {
			add(r.Hash)
		}
	case *FileIndex:
		for _, r := range o.Refs {
			add(r.Hash)
		}
	case *TreeNode:
		for _, e := range o.Entries {
			add(e.Target)
			if e.Attrs != nil && e.Attrs.GitRaw != nil {
				add(*e.Attrs.GitRaw)
			}
		}
		for _, c := range o.Children {
			add(c.Node)
		}
	case *Commit:
		add(o.Tree)
		for _, p := range o.Parents {
			add(p)
		}
		if o.GitTreeRaw != nil {
			add(*o.GitTreeRaw)
		}
	case *Tag:
		add(o.Object)
	case *DigestRecord:
		add(o.File)
	}
	return out
}
