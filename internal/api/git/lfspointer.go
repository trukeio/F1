package git

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"

	"github.com/trukeio/f1/internal/object"
)

// maxPointerSize is the largest Git LFS pointer file (the spec limit).
const maxPointerSize = 1024

// Pointer is a parsed Git LFS pointer.
type Pointer struct {
	OID  [32]byte // SHA-256 of the content
	Size int64
}

// ErrLFSMissing is returned when a pushed tree references an LFS object
// that was not uploaded.
var ErrLFSMissing = errors.New("git: LFS object missing")

// ParsePointer parses a Git LFS pointer file in the exact pointer syntax:
// "key value" lines ending in LF, "version" first, then the other keys in
// order, with "oid sha256:<hex>" and "size <n>".
func ParsePointer(data []byte) (Pointer, bool) {
	var p Pointer
	if len(data) > maxPointerSize || !bytes.HasSuffix(data, []byte("\n")) {
		return p, false
	}
	lines := bytes.Split(data[:len(data)-1], []byte("\n"))
	if len(lines) < 3 {
		return p, false
	}
	v, ok := bytes.CutPrefix(lines[0], []byte("version "))
	if !ok || !(string(v) == "https://git-lfs.github.com/spec/v1" || string(v) == "https://hawser.github.com/spec/v1") {
		return p, false
	}
	var haveOID, haveSize bool
	prev := ""
	for _, l := range lines[1:] {
		k, val, ok := bytes.Cut(l, []byte(" "))
		if !ok || len(k) == 0 || string(k) <= prev {
			return p, false
		}
		prev = string(k)
		switch string(k) {
		case "oid":
			h, ok := bytes.CutPrefix(val, []byte("sha256:"))
			if !ok || len(h) != 64 {
				return p, false
			}
			if _, err := hex.Decode(p.OID[:], h); err != nil {
				return p, false
			}
			haveOID = true
		case "size":
			n, err := strconv.ParseInt(string(val), 10, 64)
			if err != nil || n < 0 {
				return p, false
			}
			p.Size, haveSize = n, true
		}
	}
	return p, haveOID && haveSize
}

// lfsEntry turns a pointer blob in a pushed tree into an lfs entry whose
// target is the uploaded content (doc/plan.md, "Object mapping", import).
func (im *Importer) lfsEntry(ctx context.Context, ge TreeEntry, kind object.Kind, ptr []byte) (object.Entry, error) {
	p, _ := ParsePointer(ptr)
	f, ok, err := im.rp.Store().Cache().FileBySHA256(ctx, p.OID)
	if err != nil {
		return object.Entry{}, err
	}
	if ok {
		size, err := im.rp.FileSize(ctx, f)
		if err != nil {
			return object.Entry{}, err
		}
		ok = int64(size) == p.Size
	}
	if !ok {
		return object.Entry{}, fmt.Errorf("%w: %q needs LFS object %x (%d bytes), which is not on the server; run \"git lfs push --all\" and push again",
			ErrLFSMissing, ge.Name, p.OID, p.Size)
	}
	return object.Entry{Name: ge.Name, Kind: object.KindLFS, Target: f,
		Attrs: &object.Attrs{LFSPointer: append([]byte{}, ptr...), Exec: kind == object.KindExec}}, nil
}
