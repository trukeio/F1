package git

import (
	"bufio"
	"compress/zlib"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/pjbgf/sha1cd"
)

// Pack entry types beyond the object types.
const (
	packOfsDelta = 6
	packRefDelta = 7
)

// maxInlineObject bounds the size of a commit, tree or tag in a pushed pack
// (those are held in memory); blobs of any size are streamed.
const maxInlineObject = 64 << 20

// ErrCollision is returned for an object whose SHA-1 computation shows a
// collision attack (SHA-1DC, as Git uses).
var ErrCollision = errors.New("git: SHA-1 collision attack detected")

// collisionSum hashes r with SHA-1 collision detection.
func collisionSum(r io.Reader) (SHA, error) {
	h := sha1cd.New().(sha1cd.CollisionResistantHash)
	if _, err := io.Copy(h, r); err != nil {
		return SHA{}, err
	}
	sum, collision := h.CollisionResistantSum(nil)
	var s SHA
	copy(s[:], sum)
	if collision {
		return s, ErrCollision
	}
	return s, nil
}

// objectHasher computes a Git object id with collision detection as the
// content is written.
type objectHasher struct {
	h sha1cd.CollisionResistantHash
}

func newObjectHasher(t Type, size int64) *objectHasher {
	h := sha1cd.New().(sha1cd.CollisionResistantHash)
	h.Write(Header(t, size))
	return &objectHasher{h: h}
}

func (o *objectHasher) Write(p []byte) (int, error) { return o.h.Write(p) }

func (o *objectHasher) sum() (SHA, error) {
	sum, collision := o.h.CollisionResistantSum(nil)
	var s SHA
	copy(s[:], sum)
	if collision {
		return s, fmt.Errorf("%w in object %s", ErrCollision, s)
	}
	return s, nil
}

// packEntry is an entry of a pushed pack.
type packEntry struct {
	off     int64 // offset of the entry header
	dataOff int64 // offset of the zlib stream
	kind    int   // 1-4: object type; 6: ofs-delta; 7: ref-delta
	size    int64 // inflated size (of the delta data for deltas)
	baseOff int64 // ofs-delta base
	baseSHA SHA   // ref-delta base

	// Set once resolved.
	done bool
	typ  Type
	sha  SHA
}

// countingReader counts the bytes consumed from a bufio.Reader. It
// implements io.ByteReader, so that the zlib reader consumes exactly the
// compressed stream.
type countingReader struct {
	r *bufio.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (c *countingReader) ReadByte() (byte, error) {
	b, err := c.r.ReadByte()
	if err == nil {
		c.n++
	}
	return b, err
}

// packSink receives resolved objects: blobs as streams of known size,
// other objects as bytes.
type packSink interface {
	blob(ctx context.Context, size int64, r io.Reader) (SHA, error)
	object(t Type, sha SHA, data []byte)
	// base returns a reader for an object outside the pack (thin packs).
	base(ctx context.Context, s SHA) (Type, int64, io.ReaderAt, error)
}

// readPack spools a pack from r to a file in dir, then indexes and resolves
// it: non-delta objects in one sequential pass, deltas against their bases
// afterwards. Blobs stream into the sink; delta bases are read with random
// access (from the spool or the sink), never loaded whole, so memory stays
// bounded for objects of any size (doc/plan.md, "Streaming in the Git
// layer").
func readPack(ctx context.Context, r io.Reader, dir string, sink packSink) (int, error) {
	f, err := os.CreateTemp(dir, "receive-*.pack")
	if err != nil {
		return 0, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	sum := sha1.New()
	if _, err := io.Copy(io.MultiWriter(f, sum), r); err != nil {
		return 0, fmt.Errorf("git: receiving pack: %w", err)
	}
	size, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	if size < 32 {
		return 0, errors.New("git: pack too short")
	}
	// The trailer is the SHA-1 of everything before it.
	var trailer [20]byte
	if _, err := f.ReadAt(trailer[:], size-20); err != nil {
		return 0, err
	}
	check := sha1.New()
	if _, err := io.Copy(check, io.NewSectionReader(f, 0, size-20)); err != nil {
		return 0, err
	}
	if string(check.Sum(nil)) != string(trailer[:]) {
		return 0, errors.New("git: pack checksum mismatch")
	}
	p := &pack{f: f, size: size - 20, sink: sink, byOff: map[int64]*packEntry{}, bySHA: map[SHA]*packEntry{}}
	if err := p.index(ctx); err != nil {
		return 0, err
	}
	if err := p.resolveDeltas(ctx); err != nil {
		return 0, err
	}
	return len(p.entries), nil
}

type pack struct {
	f       *os.File
	size    int64 // without the trailer
	sink    packSink
	entries []*packEntry
	byOff   map[int64]*packEntry
	bySHA   map[SHA]*packEntry
	// inline holds the content of resolved non-blob objects (bases of
	// tree deltas, and what the sink needs).
	inline map[SHA][]byte
}

func (p *pack) index(ctx context.Context) error {
	cr := &countingReader{r: bufio.NewReaderSize(io.NewSectionReader(p.f, 0, p.size), 64<<10)}
	var hdr [12]byte
	if _, err := io.ReadFull(cr, hdr[:]); err != nil {
		return err
	}
	if string(hdr[:4]) != "PACK" {
		return errors.New("git: not a pack")
	}
	if v := binary.BigEndian.Uint32(hdr[4:8]); v != 2 && v != 3 {
		return fmt.Errorf("git: unsupported pack version %d", v)
	}
	n := binary.BigEndian.Uint32(hdr[8:12])
	p.inline = map[SHA][]byte{}
	for i := uint32(0); i < n; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		e := &packEntry{off: cr.n}
		c, err := cr.ReadByte()
		if err != nil {
			return fmt.Errorf("git: truncated pack: %w", err)
		}
		e.kind = int(c>>4) & 7
		e.size = int64(c & 0x0f)
		for shift := 4; c&0x80 != 0; shift += 7 {
			if c, err = cr.ReadByte(); err != nil {
				return err
			}
			if shift > 56 {
				return errors.New("git: pack entry size overflow")
			}
			e.size |= int64(c&0x7f) << shift
		}
		switch e.kind {
		case packOfsDelta:
			c, err := cr.ReadByte()
			if err != nil {
				return err
			}
			rel := int64(c & 0x7f)
			for c&0x80 != 0 {
				if c, err = cr.ReadByte(); err != nil {
					return err
				}
				if rel > 1<<55 {
					return errors.New("git: ofs-delta offset overflow")
				}
				rel = ((rel + 1) << 7) | int64(c&0x7f)
			}
			e.baseOff = e.off - rel
			if e.baseOff < 12 || p.byOff[e.baseOff] == nil {
				return errors.New("git: ofs-delta base is not an earlier entry")
			}
		case packRefDelta:
			if _, err := io.ReadFull(cr, e.baseSHA[:]); err != nil {
				return err
			}
		case int(TypeCommit), int(TypeTree), int(TypeBlob), int(TypeTag):
		default:
			return fmt.Errorf("git: unknown pack entry type %d", e.kind)
		}
		e.dataOff = cr.n
		zr, err := zlib.NewReader(cr)
		if err != nil {
			return fmt.Errorf("git: pack entry at %d: %w", e.off, err)
		}
		if e.kind == packOfsDelta || e.kind == packRefDelta {
			// Read past the delta data; it is applied later.
			if _, err := io.Copy(io.Discard, zr); err != nil {
				return fmt.Errorf("git: pack entry at %d: %w", e.off, err)
			}
		} else {
			e.typ = Type(e.kind)
			if err := p.resolved(ctx, e, io.LimitReader(zr, e.size+1)); err != nil {
				return err
			}
			if n, err := io.Copy(io.Discard, zr); err != nil || n != 0 {
				return fmt.Errorf("git: pack entry at %d: size does not match its header", e.off)
			}
		}
		if err := zr.Close(); err != nil {
			return fmt.Errorf("git: pack entry at %d: %w", e.off, err)
		}
		p.entries = append(p.entries, e)
		p.byOff[e.off] = e
	}
	if cr.n != p.size {
		return errors.New("git: trailing data after the pack's objects")
	}
	return nil
}

// resolved hands a resolved object's content to the sink and records it.
func (p *pack) resolved(ctx context.Context, e *packEntry, content io.Reader) error {
	var s SHA
	var err error
	if e.typ == TypeBlob {
		s, err = p.sink.blob(ctx, e.size, &exactReader{r: content, n: e.size})
	} else {
		if e.size > maxInlineObject {
			return fmt.Errorf("git: %s of %d bytes is too large", e.typ, e.size)
		}
		data := make([]byte, e.size)
		if _, err := io.ReadFull(content, data); err != nil {
			return fmt.Errorf("git: pack entry at %d: %w", e.off, err)
		}
		h := newObjectHasher(e.typ, e.size)
		h.Write(data)
		if s, err = h.sum(); err == nil {
			p.inline[s] = data
			p.sink.object(e.typ, s, data)
		}
	}
	if err != nil {
		return err
	}
	e.done, e.sha = true, s
	p.bySHA[s] = e
	return nil
}

// exactReader fails unless r yields exactly n bytes.
type exactReader struct {
	r io.Reader
	n int64
}

func (e *exactReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	e.n -= int64(n)
	if e.n < 0 || (err == io.EOF && e.n != 0) {
		return n, errors.New("git: object size does not match its header")
	}
	return n, err
}

// resolveDeltas applies deltas whose bases are resolved until none is
// left. Bases outside the pack (thin packs) come from the repository.
func (p *pack) resolveDeltas(ctx context.Context) error {
	for {
		progress, pending := false, 0
		for _, e := range p.entries {
			if e.done {
				continue
			}
			typ, size, base, err := p.baseOf(ctx, e)
			if err != nil {
				return err
			}
			if base == nil {
				pending++
				continue
			}
			e.typ = typ
			if err := p.applyDelta(ctx, e, size, base); err != nil {
				return fmt.Errorf("git: delta at %d: %w", e.off, err)
			}
			progress = true
		}
		if pending == 0 {
			return nil
		}
		if !progress {
			return fmt.Errorf("git: %d deltas have bases that are neither in the pack nor in the repository", pending)
		}
	}
}

// baseOf returns the type, size and content of a delta's base, or a nil
// reader if the base is a delta not resolved yet.
func (p *pack) baseOf(ctx context.Context, e *packEntry) (Type, int64, io.ReaderAt, error) {
	var b *packEntry
	if e.kind == packOfsDelta {
		b = p.byOff[e.baseOff]
	} else if b = p.bySHA[e.baseSHA]; b == nil {
		// Unresolved deltas are not in bySHA yet; outside the pack, the
		// repository has it, or a later round will.
		t, size, ra, err := p.sink.base(ctx, e.baseSHA)
		if err != nil || ra != nil {
			return t, size, ra, err
		}
		return 0, 0, nil, nil
	}
	if !b.done {
		return 0, 0, nil, nil
	}
	if data, ok := p.inline[b.sha]; ok {
		return b.typ, int64(len(data)), bytesReaderAt(data), nil
	}
	// A blob: read it back from where the sink stored it.
	return p.sink.base(ctx, b.sha)
}

func (p *pack) applyDelta(ctx context.Context, e *packEntry, baseSize int64, base io.ReaderAt) error {
	zr, err := zlib.NewReader(io.NewSectionReader(p.f, e.dataOff, p.size-e.dataOff))
	if err != nil {
		return err
	}
	defer zr.Close()
	dr := bufio.NewReader(io.LimitReader(zr, e.size))
	srcSize, err := readVarint(dr)
	if err != nil {
		return err
	}
	if srcSize != baseSize {
		return fmt.Errorf("delta base size %d, base has %d", srcSize, baseSize)
	}
	dstSize, err := readVarint(dr)
	if err != nil {
		return err
	}
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(runDelta(dr, base, baseSize, dstSize, pw))
	}()
	defer pr.Close()
	size := e.size
	e.size = dstSize
	err = p.resolved(ctx, e, pr)
	if err != nil {
		e.size = size
	}
	return err
}

// runDelta writes the result of a delta: copy instructions read ranges of
// the base, insert instructions literal bytes.
func runDelta(dr *bufio.Reader, base io.ReaderAt, baseSize, dstSize int64, w io.Writer) error {
	var written int64
	buf := make([]byte, 64<<10)
	for written < dstSize {
		op, err := dr.ReadByte()
		if err != nil {
			return fmt.Errorf("truncated delta: %w", err)
		}
		if op&0x80 != 0 {
			var off, n int64
			for i := 0; i < 4; i++ {
				if op&(1<<i) != 0 {
					b, err := dr.ReadByte()
					if err != nil {
						return err
					}
					off |= int64(b) << (8 * i)
				}
			}
			for i := 0; i < 3; i++ {
				if op&(0x10<<i) != 0 {
					b, err := dr.ReadByte()
					if err != nil {
						return err
					}
					n |= int64(b) << (8 * i)
				}
			}
			if n == 0 {
				n = 0x10000
			}
			if off+n > baseSize || written+n > dstSize {
				return errors.New("delta copy out of range")
			}
			for n > 0 {
				k := min(n, int64(len(buf)))
				if _, err := base.ReadAt(buf[:k], off); err != nil && !(err == io.EOF && k > 0) {
					return err
				}
				if _, err := w.Write(buf[:k]); err != nil {
					return err
				}
				off, n, written = off+k, n-k, written+k
			}
			continue
		}
		if op == 0 {
			return errors.New("delta: reserved instruction 0")
		}
		n := int64(op)
		if written+n > dstSize {
			return errors.New("delta insert out of range")
		}
		if _, err := io.CopyN(w, dr, n); err != nil {
			return err
		}
		written += n
	}
	if _, err := dr.ReadByte(); err != io.EOF {
		return errors.New("delta has trailing data")
	}
	return nil
}

func readVarint(r io.ByteReader) (int64, error) {
	var v int64
	for shift := 0; ; shift += 7 {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		if shift > 56 {
			return 0, errors.New("varint overflow")
		}
		v |= int64(b&0x7f) << shift
		if b&0x80 == 0 {
			return v, nil
		}
	}
}

type byteReaderAt []byte

func bytesReaderAt(b []byte) io.ReaderAt { return byteReaderAt(b) }

func (b byteReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(b)) {
		return 0, io.EOF
	}
	n := copy(p, b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}
