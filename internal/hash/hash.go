// Package hash defines the F1 object hash (D1: BLAKE3, 32 bytes).
//
// An object's hash is BLAKE3(type tag ‖ payload), where the payload is the
// raw bytes for a Chunk and the canonical CBOR encoding for every other
// object type (D5). See doc/format.md.
package hash

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"

	"lukechampine.com/blake3"
)

// Size is the length of a Hash in bytes.
const Size = 32

// Hash is a BLAKE3-256 digest.
type Hash [Size]byte

// Zero is the all-zero hash. It never identifies a stored object.
var Zero Hash

// Sum returns BLAKE3(tag ‖ payload).
func Sum(tag byte, payload []byte) Hash {
	h := blake3.New(Size, nil)
	h.Write([]byte{tag})
	h.Write(payload)
	var out Hash
	h.Sum(out[:0])
	return out
}

// Plain returns BLAKE3(data) without a type tag. It is used where the format
// needs a hash that is not an object identity, such as prolly tree boundary
// decisions.
func Plain(data []byte) Hash {
	return blake3.Sum256(data)
}

// IsZero reports whether h is the zero hash.
func (h Hash) IsZero() bool { return h == Zero }

// String returns the lowercase hex encoding of h.
func (h Hash) String() string { return hex.EncodeToString(h[:]) }

// Short returns the first 12 hex digits of h, for logs.
func (h Hash) Short() string { return hex.EncodeToString(h[:6]) }

// Parse decodes a 64-digit hex string.
func Parse(s string) (Hash, error) {
	var h Hash
	if len(s) != 2*Size {
		return h, fmt.Errorf("hash: %q has length %d, want %d", s, len(s), 2*Size)
	}
	if _, err := hex.Decode(h[:], []byte(s)); err != nil {
		return h, fmt.Errorf("hash: %w", err)
	}
	return h, nil
}

// MustParse is Parse for constants in tests; it panics on error.
func MustParse(s string) Hash {
	h, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return h
}

// MarshalText implements encoding.TextMarshaler (hex).
func (h Hash) MarshalText() ([]byte, error) { return []byte(h.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler (hex).
func (h *Hash) UnmarshalText(b []byte) error {
	p, err := Parse(string(b))
	if err != nil {
		return err
	}
	*h = p
	return nil
}

// Compare returns -1, 0 or 1 comparing a and b bytewise.
func Compare(a, b Hash) int { return bytes.Compare(a[:], b[:]) }

// ErrMismatch is returned when data does not hash to the expected value.
var ErrMismatch = errors.New("hash mismatch")
