package content

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"io"

	f1hash "github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
)

// Concat stores a File whose content is the concatenation of files, by
// concatenating their chunk lists: no content is read or copied, only the
// File and FileIndex objects of the parts. S3 multipart uploads use it.
// The new File has no DigestRecord; see Digest.
func Concat(ctx context.Context, store Store, files []f1hash.Hash) (f1hash.Hash, uint64, error) {
	b := object.NewFileBuilder(func(x *object.FileIndex) (f1hash.Hash, error) {
		return putObject(ctx, store, x)
	})
	for _, h := range files {
		payload, err := getPayload(ctx, store, h, object.TypeFile)
		if err != nil {
			return f1hash.Zero, 0, err
		}
		f, err := object.DecodeAs[*object.File](payload)
		if err != nil {
			return f1hash.Zero, 0, err
		}
		if err := addChunks(ctx, store, b, f.Level, f.Refs); err != nil {
			return f1hash.Zero, 0, err
		}
	}
	f, err := b.Finish()
	if err != nil {
		return f1hash.Zero, 0, err
	}
	h, err := putObject(ctx, store, f)
	return h, f.Size, err
}

// addChunks adds the chunk refs below refs (of the given level) in order.
func addChunks(ctx context.Context, store Store, b *object.FileBuilder, level uint64, refs []object.Ref) error {
	for _, r := range refs {
		if level == 0 {
			if err := b.Add(r); err != nil {
				return err
			}
			continue
		}
		payload, err := getPayload(ctx, store, r.Hash, object.TypeFileIndex)
		if err != nil {
			return err
		}
		x, err := object.DecodeAs[*object.FileIndex](payload)
		if err != nil {
			return err
		}
		if err := addChunks(ctx, store, b, x.Level, x.Refs); err != nil {
			return err
		}
	}
	return nil
}

// Digest reads a File's content once, computes its digests and stores its
// DigestRecord (unless one is stored already). Used for Files assembled
// without a single ingest stream (S3 multipart).
func Digest(ctx context.Context, store Store, file f1hash.Hash) (Digests, error) {
	var d Digests
	r, err := Open(ctx, store, file)
	if err != nil {
		return d, err
	}
	s256, m5, s1 := sha256.New(), md5.New(), sha1.New()
	s1.Write(blobHeader(uint64(r.Size())))
	if _, err := io.Copy(io.MultiWriter(s256, m5, s1), r); err != nil {
		return d, err
	}
	copy(d.SHA256[:], s256.Sum(nil))
	copy(d.MD5[:], m5.Sum(nil))
	copy(d.SHA1Git[:], s1.Sum(nil))
	_, err = putObject(ctx, store, &object.DigestRecord{File: file, SHA1Git: d.SHA1Git, SHA256: d.SHA256, MD5: d.MD5})
	return d, err
}
