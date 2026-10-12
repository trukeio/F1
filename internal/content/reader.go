package content

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"

	f1hash "github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
)

// Reader reads a File's content with random access. A read at any offset
// fetches only the FileIndex objects on the path to the chunks it overlaps
// and those chunks, located by the sizes recorded in the File (prefix sums).
// A Reader is not safe for concurrent use.
type Reader struct {
	ctx   context.Context
	store Store
	size  int64
	off   int64
	top   node
	path  []node // cached FileIndex nodes, path[i] at level File.Level-1-i

	chunkHash f1hash.Hash
	chunkData []byte
}

// node is a list of refs with their cumulative end offsets.
type node struct {
	hash f1hash.Hash
	refs []object.Ref
	ends []uint64 // ends[i] = sum of refs[0..i].Size
}

func newNode(h f1hash.Hash, refs []object.Ref) node {
	ends := make([]uint64, len(refs))
	var sum uint64
	for i, r := range refs {
		sum += r.Size
		ends[i] = sum
	}
	return node{hash: h, refs: refs, ends: ends}
}

// find returns the index of the ref containing offset off (relative to the
// node) and the ref's start offset.
func (n node) find(off uint64) (int, uint64) {
	i := sort.Search(len(n.ends), func(i int) bool { return n.ends[i] > off })
	if i == 0 {
		return 0, 0
	}
	return i, n.ends[i-1]
}

// Open returns a Reader for the File with hash h.
func Open(ctx context.Context, store Store, h f1hash.Hash) (*Reader, error) {
	payload, err := getPayload(ctx, store, h, object.TypeFile)
	if err != nil {
		return nil, err
	}
	f, err := object.DecodeAs[*object.File](payload)
	if err != nil {
		return nil, err
	}
	return OpenFile(ctx, store, f), nil
}

// OpenFile returns a Reader for a decoded File.
func OpenFile(ctx context.Context, store Store, f *object.File) *Reader {
	return &Reader{
		ctx:   ctx,
		store: store,
		size:  int64(f.Size),
		top:   newNode(f1hash.Zero, f.Refs),
		path:  make([]node, f.Level),
	}
}

// Size returns the content length.
func (r *Reader) Size() int64 { return r.size }

// Read implements io.Reader.
func (r *Reader) Read(p []byte) (int, error) {
	n, err := r.ReadAt(p, r.off)
	r.off += int64(n)
	if n > 0 && errors.Is(err, io.EOF) {
		err = nil
	}
	return n, err
}

// Seek implements io.Seeker.
func (r *Reader) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = r.off + offset
	case io.SeekEnd:
		abs = r.size + offset
	default:
		return 0, errors.New("content: invalid whence")
	}
	if abs < 0 {
		return 0, errors.New("content: negative position")
	}
	r.off = abs
	return abs, nil
}

// ReadAt implements io.ReaderAt.
func (r *Reader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("content: negative offset")
	}
	n := 0
	for n < len(p) {
		pos := off + int64(n)
		if pos >= r.size {
			return n, io.EOF
		}
		data, start, err := r.chunkAt(uint64(pos))
		if err != nil {
			return n, err
		}
		n += copy(p[n:], data[uint64(pos)-start:])
	}
	return n, nil
}

// chunkAt returns the chunk containing pos and the chunk's start offset.
func (r *Reader) chunkAt(pos uint64) ([]byte, uint64, error) {
	cur := r.top
	var base uint64
	for depth := 0; ; depth++ {
		i, start := cur.find(pos - base)
		ref := cur.refs[i]
		base += start
		if depth == len(r.path) {
			data, err := r.chunk(ref)
			return data, base, err
		}
		if r.path[depth].hash != ref.Hash {
			n, err := r.fileIndex(ref, uint64(len(r.path)-1-depth))
			if err != nil {
				return nil, 0, err
			}
			r.path[depth] = n
		}
		cur = r.path[depth]
	}
}

func (r *Reader) fileIndex(ref object.Ref, level uint64) (node, error) {
	payload, err := getPayload(r.ctx, r.store, ref.Hash, object.TypeFileIndex)
	if err != nil {
		return node{}, err
	}
	x, err := object.DecodeAs[*object.FileIndex](payload)
	if err != nil {
		return node{}, err
	}
	n := newNode(ref.Hash, x.Refs)
	if x.Level != level || n.ends[len(n.ends)-1] != ref.Size {
		return node{}, fmt.Errorf("content: file index %s does not match its ref", ref.Hash.Short())
	}
	return n, nil
}

func (r *Reader) chunk(ref object.Ref) ([]byte, error) {
	if ref.Hash == r.chunkHash && r.chunkData != nil {
		return r.chunkData, nil
	}
	data, err := getPayload(r.ctx, r.store, ref.Hash, object.TypeChunk)
	if err != nil {
		return nil, err
	}
	if uint64(len(data)) != ref.Size {
		return nil, fmt.Errorf("content: chunk %s has %d bytes, ref says %d", ref.Hash.Short(), len(data), ref.Size)
	}
	r.chunkHash, r.chunkData = ref.Hash, data
	return data, nil
}

func getPayload(ctx context.Context, store Store, h f1hash.Hash, want object.Type) ([]byte, error) {
	rec, err := store.Get(ctx, h)
	if err != nil {
		return nil, err
	}
	if rec.Hash != h {
		return nil, fmt.Errorf("content: store returned %s for %s", rec.Hash.Short(), h.Short())
	}
	if rec.Type != want {
		return nil, fmt.Errorf("content: object %s is a %s, want %s", h.Short(), rec.Type, want)
	}
	return rec.Payload(nil)
}
