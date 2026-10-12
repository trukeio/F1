// Package object defines the F1 object types and their canonical encoding.
//
// Every object except Chunk is encoded as canonical CBOR (RFC 8949 §4.2.1,
// core deterministic encoding) and identified by BLAKE3(type tag ‖ CBOR
// bytes) (D5). Decoding is strict: an encoding is accepted only if
// re-encoding the decoded value reproduces it byte for byte, so every object
// has exactly one valid encoding. The byte-level format is specified in
// doc/format.md and is permanent.
package object

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/fxamacker/cbor/v2"

	"github.com/trukeio/f1/internal/hash"
)

// Type is the 1-byte object type tag.
type Type byte

// Object type tags. Values are part of the on-disk format.
const (
	TypeChunk        Type = 0x01
	TypeFile         Type = 0x02
	TypeFileIndex    Type = 0x03
	TypeTree         Type = 0x04
	TypeCommit       Type = 0x05
	TypeTag          Type = 0x06
	TypeDigestRecord Type = 0x07
	TypeGitRaw       Type = 0x08
)

func (t Type) String() string {
	switch t {
	case TypeChunk:
		return "chunk"
	case TypeFile:
		return "file"
	case TypeFileIndex:
		return "fileindex"
	case TypeTree:
		return "tree"
	case TypeCommit:
		return "commit"
	case TypeTag:
		return "tag"
	case TypeDigestRecord:
		return "digestrecord"
	case TypeGitRaw:
		return "gitraw"
	}
	return fmt.Sprintf("type(0x%02x)", byte(t))
}

// Valid reports whether t is a known object type.
func (t Type) Valid() bool { return t >= TypeChunk && t <= TypeGitRaw }

// Object is a CBOR-encoded object (every type except Chunk).
type Object interface {
	Type() Type
	// Validate checks the invariants that the CBOR schema cannot express.
	Validate() error
}

// ErrNonCanonical is returned for encodings that decode but are not the
// canonical encoding of the decoded value.
var ErrNonCanonical = errors.New("object: non-canonical encoding")

var (
	encMode cbor.EncMode
	decMode cbor.DecMode
)

func init() {
	eo := cbor.CoreDetEncOptions()
	eo.ByteArray = cbor.ByteArrayToByteSlice    // hashes and digests as byte strings
	eo.NilContainers = cbor.NilContainerAsEmpty // nil and empty slices encode alike
	var err error
	if encMode, err = eo.EncMode(); err != nil {
		panic(err)
	}
	do := cbor.DecOptions{
		DupMapKey:         cbor.DupMapKeyEnforcedAPF,
		IndefLength:       cbor.IndefLengthForbidden,
		TagsMd:            cbor.TagsForbidden,
		ExtraReturnErrors: cbor.ExtraDecErrorUnknownField,
		MaxNestedLevels:   16,
		MaxArrayElements:  1 << 20,
		MaxMapPairs:       1 << 16,
		UTF8:              cbor.UTF8RejectInvalid,
	}
	if decMode, err = do.DecMode(); err != nil {
		panic(err)
	}
}

// Encode validates o and returns its canonical encoding.
func Encode(o Object) ([]byte, error) {
	if err := o.Validate(); err != nil {
		return nil, fmt.Errorf("object: invalid %s: %w", o.Type(), err)
	}
	b, err := encMode.Marshal(o)
	if err != nil {
		return nil, fmt.Errorf("object: encode %s: %w", o.Type(), err)
	}
	return b, nil
}

// Hash encodes o and returns its hash and encoding.
func Hash(o Object) (hash.Hash, []byte, error) {
	b, err := Encode(o)
	if err != nil {
		return hash.Zero, nil, err
	}
	return hash.Sum(byte(o.Type()), b), b, nil
}

// ChunkHash returns the hash of a chunk: BLAKE3(0x01 ‖ bytes).
func ChunkHash(data []byte) hash.Hash { return hash.Sum(byte(TypeChunk), data) }

// Decode decodes data as an object of type t. It rejects unknown fields,
// invalid values and any encoding that is not canonical.
func Decode(t Type, data []byte) (Object, error) {
	var o Object
	switch t {
	case TypeFile:
		o = new(File)
	case TypeFileIndex:
		o = new(FileIndex)
	case TypeTree:
		o = new(TreeNode)
	case TypeCommit:
		o = new(Commit)
	case TypeTag:
		o = new(Tag)
	case TypeDigestRecord:
		o = new(DigestRecord)
	case TypeGitRaw:
		o = new(GitRaw)
	case TypeChunk:
		return nil, errors.New("object: chunks are not CBOR objects")
	default:
		return nil, fmt.Errorf("object: unknown type 0x%02x", byte(t))
	}
	if err := decodeInto(data, o); err != nil {
		return nil, err
	}
	return o, nil
}

// DecodeAs decodes data into a value of the concrete type T, for example
// DecodeAs[*object.File](data).
func DecodeAs[T Object](data []byte) (T, error) {
	var zero T
	var probe T
	o, err := Decode(probe.Type(), data)
	if err != nil {
		return zero, err
	}
	return o.(T), nil
}

func decodeInto(data []byte, o Object) error {
	if err := decMode.Unmarshal(data, o); err != nil {
		return fmt.Errorf("object: decode %s: %w", o.Type(), err)
	}
	re, err := Encode(o)
	if err != nil {
		return err
	}
	if !bytes.Equal(re, data) {
		return fmt.Errorf("%w (%s)", ErrNonCanonical, o.Type())
	}
	return nil
}
