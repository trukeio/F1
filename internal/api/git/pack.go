package git

import (
	"bytes"
	"compress/zlib"
	"container/list"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"hash"
	"io"
)

// Pack object types of deltas.
const (
	typeOfsDelta Type = 6
	typeRefDelta Type = 7
)

const (
	// baseCacheBytes bounds the delta bases kept in memory while a pack is
	// written.
	baseCacheBytes = 32 << 20
	// maxDeltaDepth bounds delta chains, as Git's pack.depth does.
	maxDeltaDepth = 50
)

// writePack writes a pack (version 2) of objs to w (doc/plan.md, "Pack
// generation"; ADR-0012). Trees and blobs of at most deltaMaxSize bytes
// are sent as deltas against the previously sent version of the same path
// when that halves them; the walk sends newer versions first, as Git does.
// Deltas are ofs-deltas if the client accepts them, ref-deltas otherwise;
// their bases are always earlier in the same pack. Larger blob content is
// streamed from the repository, so memory stays bounded whatever the blob
// size (D19). Objects are zlib-compressed at the fastest level: most large
// content is already compressed.
func writePack(ctx context.Context, w io.Writer, x *Exporter, objs []Object, ofsDelta bool) error {
	sum := sha1.New()
	pw := &packWriter{hw: &hashWriter{w: w, h: sum}, x: x, ofs: ofsDelta, bases: map[string]*deltaBase{}, lru: list.New()}
	var hdr [12]byte
	copy(hdr[:4], "PACK")
	binary.BigEndian.PutUint32(hdr[4:8], 2)
	binary.BigEndian.PutUint32(hdr[8:12], uint32(len(objs)))
	if _, err := pw.hw.Write(hdr[:]); err != nil {
		return err
	}
	for _, o := range objs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := pw.object(ctx, o); err != nil {
			return fmt.Errorf("git: pack object %s: %w", o.SHA, err)
		}
	}
	_, err := w.Write(sum.Sum(nil))
	return err
}

type packWriter struct {
	hw    *hashWriter
	x     *Exporter
	ofs   bool
	bases map[string]*deltaBase // by type and path
	lru   *list.List            // of *deltaBase, most recent first
	bytes int64
}

// deltaBase is an object already in the pack that later versions of its
// path can be deltas against.
type deltaBase struct {
	key   string
	off   int64
	sha   SHA
	data  []byte
	depth int
	elem  *list.Element
}

func (pw *packWriter) object(ctx context.Context, o Object) error {
	start := pw.hw.n
	data, err := pw.small(ctx, o)
	if err != nil {
		return err
	}
	if data == nil {
		return pw.whole(ctx, o)
	}
	key := fmt.Sprintf("%d %s", o.Type, o.Path)
	depth := 0
	if b := pw.bases[key]; b != nil && b.depth < maxDeltaDepth {
		if delta := makeDelta(b.data, data); delta != nil {
			if err := pw.delta(b, delta); err != nil {
				return err
			}
			depth = b.depth + 1
			pw.remember(&deltaBase{key: key, off: start, sha: o.SHA, data: data, depth: depth})
			return nil
		}
	}
	if err := pw.write(o.Type, bytes.NewReader(data), int64(len(data))); err != nil {
		return err
	}
	pw.remember(&deltaBase{key: key, off: start, sha: o.SHA, data: data})
	return nil
}

// small returns the content of a tree or blob that may be deltified (at
// most deltaMaxSize bytes), or nil.
func (pw *packWriter) small(ctx context.Context, o Object) ([]byte, error) {
	switch o.Type {
	case TypeTree:
		data, err := pw.x.Content(ctx, o)
		if err != nil || len(data) > deltaMaxSize {
			return nil, err
		}
		return data, nil
	case TypeBlob:
		if ptr, ok := pointer(o.Blob); ok {
			return ptr, nil
		}
		size, err := pw.x.rp.FileSize(ctx, o.Blob.Target)
		if err != nil || size > deltaMaxSize {
			return nil, err
		}
		rd, err := pw.x.rp.OpenFile(ctx, o.Blob.Target)
		if err != nil {
			return nil, err
		}
		data := make([]byte, size)
		if _, err := io.ReadFull(rd, data); err != nil {
			return nil, err
		}
		return data, nil
	}
	return nil, nil
}

// remember makes an object the delta base of its path, within the cache
// bound.
func (pw *packWriter) remember(b *deltaBase) {
	if old := pw.bases[b.key]; old != nil {
		pw.lru.Remove(old.elem)
		pw.bytes -= int64(len(old.data))
	}
	b.elem = pw.lru.PushFront(b)
	pw.bases[b.key] = b
	pw.bytes += int64(len(b.data))
	for pw.bytes > baseCacheBytes {
		last := pw.lru.Back().Value.(*deltaBase)
		pw.lru.Remove(last.elem)
		delete(pw.bases, last.key)
		pw.bytes -= int64(len(last.data))
	}
}

// delta writes a delta object against base.
func (pw *packWriter) delta(base *deltaBase, delta []byte) error {
	start := pw.hw.n
	var hdr []byte
	if pw.ofs {
		hdr = objectHeader(typeOfsDelta, int64(len(delta)))
		hdr = append(hdr, ofsOffset(start-base.off)...)
	} else {
		hdr = objectHeader(typeRefDelta, int64(len(delta)))
		hdr = append(hdr, base.sha[:]...)
	}
	if _, err := pw.hw.Write(hdr); err != nil {
		return err
	}
	return pw.compress(bytes.NewReader(delta), int64(len(delta)))
}

// ofsOffset encodes the distance back to an ofs-delta's base: big-endian
// groups of 7 bits, each continuation adding one (Git's encoding).
func ofsOffset(d int64) []byte {
	var buf [10]byte
	i := len(buf) - 1
	buf[i] = byte(d & 0x7f)
	for d >>= 7; d > 0; d >>= 7 {
		d--
		i--
		buf[i] = 0x80 | byte(d&0x7f)
	}
	return buf[i:]
}

// whole writes an object that is not deltified, streaming blob content.
func (pw *packWriter) whole(ctx context.Context, o Object) error {
	if o.Type != TypeBlob {
		data, err := pw.x.Content(ctx, o)
		if err != nil {
			return err
		}
		return pw.write(o.Type, bytes.NewReader(data), int64(len(data)))
	}
	rd, err := pw.x.rp.OpenFile(ctx, o.Blob.Target)
	if err != nil {
		return err
	}
	return pw.write(o.Type, rd, rd.Size())
}

func (pw *packWriter) write(t Type, content io.Reader, size int64) error {
	if _, err := pw.hw.Write(objectHeader(t, size)); err != nil {
		return err
	}
	return pw.compress(content, size)
}

func (pw *packWriter) compress(content io.Reader, size int64) error {
	zw, err := zlib.NewWriterLevel(pw.hw, zlib.BestSpeed)
	if err != nil {
		return err
	}
	n, err := io.Copy(zw, content)
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("content is %d bytes, expected %d", n, size)
	}
	return zw.Close()
}

// objectHeader is the pack object header: type and size in a varint whose
// first byte holds the type and the low 4 bits of the size.
func objectHeader(t Type, size int64) []byte {
	b := []byte{byte(t)<<4 | byte(size&0x0f)}
	size >>= 4
	for size > 0 {
		b[len(b)-1] |= 0x80
		b = append(b, byte(size&0x7f))
		size >>= 7
	}
	return b
}

// hashWriter hashes and counts what it writes.
type hashWriter struct {
	w io.Writer
	h hash.Hash
	n int64
}

func (h *hashWriter) Write(p []byte) (int, error) {
	n, err := h.w.Write(p)
	h.h.Write(p[:n])
	h.n += int64(n)
	return n, err
}
