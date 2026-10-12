package object

import (
	"errors"
	"fmt"
	"math"

	"github.com/trukeio/f1/internal/hash"
)

// File layout parameters. Part of the on-disk format.
const (
	// MaxRefs is the maximum number of refs in a File or FileIndex.
	MaxRefs = 4096
	// MaxLevel is the maximum depth of FileIndex levels. With at least
	// MinGroup refs per FileIndex this covers far more than 2^64 bytes.
	MaxLevel = 4
	// MinGroup is the minimum number of refs before a content-defined
	// FileIndex boundary is honored.
	MinGroup = 256
	// groupMask selects the boundary bits: a FileIndex ends after a ref whose
	// hash has the low 10 bits of its first two bytes (little-endian) zero,
	// giving about 1024 + MinGroup refs per FileIndex on average.
	groupMask = 0x3FF
	// maxChunkSize bounds a level-0 ref (chunk.MaxSize; duplicated to keep
	// this package free of the chunker).
	maxChunkSize = 256 << 10
)

// Ref points to a chunk (level 0) or a FileIndex (level > 0) and gives the
// number of content bytes it covers. Encoded as the array [hash, size].
type Ref struct {
	_    struct{} `cbor:",toarray"`
	Hash hash.Hash
	Size uint64
}

// File is a file's content: Level 0 means Refs are chunks; Level n > 0 means
// Refs are FileIndex objects of level n-1. Encoded as [size, level, refs].
type File struct {
	_     struct{} `cbor:",toarray"`
	Size  uint64
	Level uint64
	Refs  []Ref
}

// FileIndex is an interior node of a large File's chunk list. Encoded as
// [level, refs], with the same level meaning as File.
type FileIndex struct {
	_     struct{} `cbor:",toarray"`
	Level uint64
	Refs  []Ref
}

func (*File) Type() Type      { return TypeFile }
func (*FileIndex) Type() Type { return TypeFileIndex }

// Validate checks the File invariants.
func (f *File) Validate() error {
	if f.Level > MaxLevel {
		return fmt.Errorf("level %d > %d", f.Level, MaxLevel)
	}
	if f.Level > 0 && len(f.Refs) < 2 {
		return errors.New("indexed file needs at least 2 refs")
	}
	if (f.Size == 0) != (len(f.Refs) == 0) {
		return errors.New("refs must be empty exactly when size is 0")
	}
	sum, err := checkRefs(f.Level, f.Refs)
	if err != nil {
		return err
	}
	if sum != f.Size {
		return fmt.Errorf("refs cover %d bytes, size is %d", sum, f.Size)
	}
	return nil
}

// Validate checks the FileIndex invariants.
func (x *FileIndex) Validate() error {
	if x.Level >= MaxLevel {
		return fmt.Errorf("level %d >= %d", x.Level, MaxLevel)
	}
	if len(x.Refs) == 0 {
		return errors.New("empty file index")
	}
	_, err := checkRefs(x.Level, x.Refs)
	return err
}

func checkRefs(level uint64, refs []Ref) (uint64, error) {
	if len(refs) > MaxRefs {
		return 0, fmt.Errorf("%d refs > %d", len(refs), MaxRefs)
	}
	var sum uint64
	for i, r := range refs {
		if r.Size == 0 {
			return 0, fmt.Errorf("ref %d has size 0", i)
		}
		if level == 0 && r.Size > maxChunkSize {
			return 0, fmt.Errorf("chunk ref %d has size %d > %d", i, r.Size, maxChunkSize)
		}
		if r.Size > math.MaxUint64-sum {
			return 0, errors.New("size overflow")
		}
		sum += r.Size
	}
	return sum, nil
}

// groupBoundary reports whether a FileIndex group may end after r.
func groupBoundary(r Ref) bool {
	return (uint16(r.Hash[0])|uint16(r.Hash[1])<<8)&groupMask == 0
}

// FileBuilder builds a File from its chunk refs in order, emitting FileIndex
// objects as they fill. Memory use is bounded by MaxRefs refs per level.
//
// The result equals this batch definition: let refs be the chunk refs and
// level 0. While len(refs) > MaxRefs, split refs into groups, ending a group
// after a ref when the group has at least MinGroup refs and groupBoundary
// holds, or when it has MaxRefs refs; replace refs by one FileIndex ref per
// group and increment level. The File is [total size, level, refs].
type FileBuilder struct {
	emit func(*FileIndex) (hash.Hash, error)
	top  *levelBuilder
	size uint64
}

// NewFileBuilder returns a builder that calls emit for every FileIndex it
// creates; emit stores the object and returns its hash.
func NewFileBuilder(emit func(*FileIndex) (hash.Hash, error)) *FileBuilder {
	b := &FileBuilder{emit: emit}
	b.top = &levelBuilder{b: b}
	return b
}

// Add appends the ref of the next chunk.
func (b *FileBuilder) Add(r Ref) error {
	b.size += r.Size
	return b.top.add(r)
}

// Finish returns the File.
func (b *FileBuilder) Finish() (*File, error) {
	level, refs, err := b.top.finish()
	if err != nil {
		return nil, err
	}
	f := &File{Size: b.size, Level: level, Refs: refs}
	if err := f.Validate(); err != nil {
		return nil, err
	}
	return f, nil
}

type levelBuilder struct {
	b        *FileBuilder
	level    uint64
	pending  []Ref // refs while this level still fits in a File (<= MaxRefs)
	indexing bool  // this level has more than MaxRefs refs and is grouped
	group    []Ref
	groupLen uint64
	next     *levelBuilder
}

func (l *levelBuilder) add(r Ref) error {
	if !l.indexing {
		l.pending = append(l.pending, r)
		if len(l.pending) <= MaxRefs {
			return nil
		}
		l.indexing = true
		pending := l.pending
		l.pending = nil
		for _, p := range pending {
			if err := l.addToGroup(p); err != nil {
				return err
			}
		}
		return nil
	}
	return l.addToGroup(r)
}

func (l *levelBuilder) addToGroup(r Ref) error {
	l.group = append(l.group, r)
	l.groupLen += r.Size
	if (len(l.group) >= MinGroup && groupBoundary(r)) || len(l.group) == MaxRefs {
		return l.flushGroup()
	}
	return nil
}

func (l *levelBuilder) flushGroup() error {
	if l.level+1 > MaxLevel {
		return errors.New("object: file too large")
	}
	x := &FileIndex{Level: l.level, Refs: l.group}
	h, err := l.b.emit(x)
	if err != nil {
		return err
	}
	ref := Ref{Hash: h, Size: l.groupLen}
	l.group = make([]Ref, 0, MaxRefs)
	l.groupLen = 0
	if l.next == nil {
		l.next = &levelBuilder{b: l.b, level: l.level + 1}
	}
	return l.next.add(ref)
}

func (l *levelBuilder) finish() (uint64, []Ref, error) {
	if !l.indexing {
		return l.level, l.pending, nil
	}
	if len(l.group) > 0 {
		if err := l.flushGroup(); err != nil {
			return 0, nil, err
		}
	}
	return l.next.finish()
}
