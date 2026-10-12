package object

import (
	"errors"
	"fmt"

	"github.com/trukeio/f1/internal/hash"
)

// Person is a commit author, committer or tag tagger. Encoded as
// [name, email, time, tz]; tz is the Git offset text such as "+0200".
type Person struct {
	_     struct{} `cbor:",toarray"`
	Name  []byte
	Email []byte
	Time  int64 // Unix seconds
	TZ    string
}

// ExtraHeader is a Git header that F1 does not model (gpgsig, mergetag,
// encoding, ...), kept in original order. Encoded as [key, value].
type ExtraHeader struct {
	_     struct{} `cbor:",toarray"`
	Key   []byte
	Value []byte
}

// Commit is a commit. Encoded as a map with integer keys.
type Commit struct {
	Tree       hash.Hash     `cbor:"0,keyasint"`
	Parents    []hash.Hash   `cbor:"1,keyasint"`
	Author     Person        `cbor:"2,keyasint"`
	Committer  Person        `cbor:"3,keyasint"`
	Message    []byte        `cbor:"4,keyasint"` // raw, not normalized
	GitExtra   []ExtraHeader `cbor:"5,keyasint,omitempty"`
	GitRawHint []byte        `cbor:"6,keyasint,omitempty"` // exact Git commit bytes, only when re-encoding fails
	GitTreeRaw *hash.Hash    `cbor:"7,keyasint,omitempty"` // GitRaw for a non-canonical root Git tree
}

func (*Commit) Type() Type { return TypeCommit }

// Validate checks the Commit invariants.
func (c *Commit) Validate() error {
	if c.Tree.IsZero() {
		return errors.New("zero tree")
	}
	seen := make(map[hash.Hash]bool, len(c.Parents))
	for i, p := range c.Parents {
		if p.IsZero() {
			return fmt.Errorf("parent %d is zero", i)
		}
		if seen[p] {
			return fmt.Errorf("duplicate parent %s", p.Short())
		}
		seen[p] = true
	}
	if c.GitTreeRaw != nil && c.GitTreeRaw.IsZero() {
		return errors.New("zero git_tree_raw")
	}
	return validateExtra(c.GitExtra)
}

// Tag target types, as in the Git tag "type" header.
const (
	TagTargetCommit = "commit"
	TagTargetTree   = "tree"
	TagTargetBlob   = "blob"
	TagTargetTag    = "tag"
)

// Tag is an annotated tag. Encoded as a map with integer keys.
type Tag struct {
	Object     hash.Hash     `cbor:"0,keyasint"` // F1 hash of the tagged object
	TargetType string        `cbor:"1,keyasint"` // commit, tree, blob or tag
	Name       []byte        `cbor:"2,keyasint"`
	Tagger     *Person       `cbor:"3,keyasint,omitempty"` // Git allows tags without tagger
	Message    []byte        `cbor:"4,keyasint"`
	GitExtra   []ExtraHeader `cbor:"5,keyasint,omitempty"`
	GitRawHint []byte        `cbor:"6,keyasint,omitempty"`
}

func (*Tag) Type() Type { return TypeTag }

// Validate checks the Tag invariants.
func (t *Tag) Validate() error {
	if t.Object.IsZero() {
		return errors.New("zero object")
	}
	switch t.TargetType {
	case TagTargetCommit, TagTargetTree, TagTargetBlob, TagTargetTag:
	default:
		return fmt.Errorf("unknown target type %q", t.TargetType)
	}
	if len(t.Name) == 0 {
		return errors.New("empty tag name")
	}
	return validateExtra(t.GitExtra)
}

func validateExtra(extra []ExtraHeader) error {
	for i, h := range extra {
		if len(h.Key) == 0 {
			return fmt.Errorf("extra header %d has empty key", i)
		}
	}
	return nil
}
