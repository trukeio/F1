package git

import (
	"compress/zlib"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"hash"
	"io"
)

// writePack writes a pack (version 2, without deltas, doc/plan.md "Pack
// generation") of objs to w. Blob content is streamed from the repository
// (Open with chunk-by-chunk reads), so memory stays bounded whatever the
// blob size (D19). Objects are zlib-compressed at the fastest level: most
// large content is already compressed.
func writePack(ctx context.Context, w io.Writer, x *Exporter, objs []Object) error {
	sum := sha1.New()
	pw := &hashWriter{w: w, h: sum}
	var hdr [12]byte
	copy(hdr[:4], "PACK")
	binary.BigEndian.PutUint32(hdr[4:8], 2)
	binary.BigEndian.PutUint32(hdr[8:12], uint32(len(objs)))
	if _, err := pw.Write(hdr[:]); err != nil {
		return err
	}
	for _, o := range objs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := writePackObject(ctx, pw, x, o); err != nil {
			return fmt.Errorf("git: pack object %s: %w", o.SHA, err)
		}
	}
	_, err := w.Write(sum.Sum(nil))
	return err
}

func writePackObject(ctx context.Context, w io.Writer, x *Exporter, o Object) error {
	var size int64
	var content io.Reader
	switch {
	case o.Type != TypeBlob:
		data, err := x.Content(ctx, o)
		if err != nil {
			return err
		}
		size, content = int64(len(data)), bytesReader(data)
	default:
		if ptr, ok := pointer(o.Blob); ok {
			size, content = int64(len(ptr)), bytesReader(ptr)
			break
		}
		rd, err := x.rp.OpenFile(ctx, o.Blob.Target)
		if err != nil {
			return err
		}
		size, content = rd.Size(), rd
	}
	if _, err := w.Write(objectHeader(o.Type, size)); err != nil {
		return err
	}
	zw, err := zlib.NewWriterLevel(w, zlib.BestSpeed)
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

type hashWriter struct {
	w io.Writer
	h hash.Hash
}

func (h *hashWriter) Write(p []byte) (int, error) {
	n, err := h.w.Write(p)
	h.h.Write(p[:n])
	return n, err
}

type byteReader struct {
	b []byte
}

func bytesReader(b []byte) io.Reader { return &byteReader{b} }

func (r *byteReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.b)
	r.b = r.b[n:]
	return n, nil
}
