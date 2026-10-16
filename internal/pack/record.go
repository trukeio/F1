// Package pack implements the F1 pack file format: a sequence of object
// records followed by a sorted index and a checksummed trailer. The same
// format is used for packs on disk and for pack streams in the native API.
// The byte layout is specified in doc/format.md and is permanent.
package pack

import (
	"errors"
	"fmt"
	"runtime"

	"github.com/klauspost/compress/zstd"

	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
)

// MaxObjectSize bounds the uncompressed payload of one record.
const MaxObjectSize = 64 << 20

// FlagZstd marks a record whose payload is zstd-compressed. No other flag
// bits are defined; readers reject records that set them.
const FlagZstd uint8 = 0x01

// Record is one stored object.
type Record struct {
	Type  object.Type
	Hash  hash.Hash
	Flags uint8
	Size  uint32 // uncompressed payload length
	Data  []byte // stored payload (compressed when Flags&FlagZstd != 0)
}

var (
	encoder *zstd.Encoder
	decoder *zstd.Decoder
)

func init() {
	var err error
	encoder, err = zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithEncoderCRC(false), // payloads are verified by their hash
		// Chunks are at most 256 KiB and other objects small: a 1 MiB
		// window compresses them as well as the default 8 MiB one, and
		// keeps the encoders' history buffers (one per concurrent
		// encoding) small.
		zstd.WithWindowSize(1<<20),
		zstd.WithEncoderConcurrency(runtime.GOMAXPROCS(0)))
	if err != nil {
		panic(err)
	}
	decoder, err = zstd.NewReader(nil,
		zstd.WithDecoderConcurrency(0),
		zstd.WithDecoderMaxMemory(MaxObjectSize))
	if err != nil {
		panic(err)
	}
}

// NewRecord builds the record for an object payload (raw chunk bytes or
// canonical CBOR). The payload is stored compressed only if zstd saves at
// least 5 % (D4): 20 × compressed ≤ 19 × uncompressed. NewRecord does not
// retain payload; dst, if not nil, is reused for the compressed bytes.
func NewRecord(t object.Type, h hash.Hash, payload, dst []byte) (Record, error) {
	if !t.Valid() {
		return Record{}, fmt.Errorf("pack: invalid type %s", t)
	}
	if len(payload) > MaxObjectSize {
		return Record{}, fmt.Errorf("pack: object of %d bytes exceeds %d", len(payload), MaxObjectSize)
	}
	rec := Record{Type: t, Hash: h, Size: uint32(len(payload))}
	if len(payload) > 0 {
		c := encoder.EncodeAll(payload, dst[:0])
		if 20*len(c) <= 19*len(payload) {
			rec.Flags = FlagZstd
			rec.Data = c
			return rec, nil
		}
	}
	rec.Data = append(dst[:0], payload...)
	return rec, nil
}

// ErrCorrupt is returned for malformed packs and records.
var ErrCorrupt = errors.New("pack: corrupt")

// Payload returns the uncompressed payload and checks that it hashes to
// rec.Hash. dst, if not nil, is reused.
func (rec Record) Payload(dst []byte) ([]byte, error) {
	var p []byte
	switch rec.Flags {
	case 0:
		if len(rec.Data) != int(rec.Size) {
			return nil, fmt.Errorf("%w: stored length %d != size %d", ErrCorrupt, len(rec.Data), rec.Size)
		}
		p = rec.Data
	case FlagZstd:
		if rec.Size > MaxObjectSize {
			return nil, fmt.Errorf("%w: size %d too large", ErrCorrupt, rec.Size)
		}
		if cap(dst) < int(rec.Size) {
			dst = make([]byte, 0, rec.Size)
		}
		var err error
		p, err = decoder.DecodeAll(rec.Data, dst[:0])
		if err != nil {
			return nil, fmt.Errorf("%w: zstd: %v", ErrCorrupt, err)
		}
		if len(p) != int(rec.Size) {
			return nil, fmt.Errorf("%w: decompressed %d bytes, want %d", ErrCorrupt, len(p), rec.Size)
		}
	default:
		return nil, fmt.Errorf("%w: unknown flags 0x%02x", ErrCorrupt, rec.Flags)
	}
	if got := hash.Sum(byte(rec.Type), p); got != rec.Hash {
		return nil, fmt.Errorf("%w: %s %s: %w", ErrCorrupt, rec.Type, rec.Hash.Short(), hash.ErrMismatch)
	}
	return p, nil
}
