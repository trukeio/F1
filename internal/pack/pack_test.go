package pack

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
	"github.com/trukeio/f1/internal/testutil"
)

type obj struct {
	typ     object.Type
	payload []byte
}

func (o obj) hash() hash.Hash { return hash.Sum(byte(o.typ), o.payload) }

func sampleObjs(t testing.TB) []obj {
	file := &object.File{Size: 5, Refs: []object.Ref{{Hash: object.ChunkHash([]byte("hello")), Size: 5}}}
	_, fb, err := object.Hash(file)
	if err != nil {
		t.Fatal(err)
	}
	return []obj{
		{object.TypeChunk, []byte("hello")},
		{object.TypeChunk, testutil.Bytes("incompressible", 200<<10)},
		{object.TypeChunk, bytes.Repeat([]byte("compressible "), 10000)},
		{object.TypeChunk, nil},
		{object.TypeFile, fb},
	}
}

func writePack(t testing.TB, objs []obj) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		rec, err := NewRecord(o.typ, o.hash(), o.payload, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Add(rec); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Finish(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestRoundTrip(t *testing.T) {
	objs := sampleObjs(t)
	data := writePack(t, objs)
	p, err := Open(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Verify(); err != nil {
		t.Fatal(err)
	}
	if len(p.Entries()) != len(objs) {
		t.Fatalf("%d entries, want %d", len(p.Entries()), len(objs))
	}
	for _, o := range objs {
		typ, payload, err := p.Get(o.hash())
		if err != nil {
			t.Fatal(err)
		}
		if typ != o.typ || !bytes.Equal(payload, o.payload) {
			t.Errorf("object %s differs", o.hash().Short())
		}
	}
	e, _ := p.Lookup(objs[1].hash())
	if e.Flags != 0 {
		t.Error("incompressible chunk was stored compressed")
	}
	e, _ = p.Lookup(objs[2].hash())
	if e.Flags != FlagZstd || e.Stored >= e.Size {
		t.Error("compressible chunk was stored uncompressed")
	}
	if _, ok := p.Lookup(object.ChunkHash([]byte("missing"))); ok {
		t.Error("lookup of missing object succeeded")
	}

	var scanned []hash.Hash
	if err := Scan(bytes.NewReader(data), func(r Record) error {
		if _, err := r.Payload(nil); err != nil {
			return err
		}
		scanned = append(scanned, r.Hash)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for i, o := range objs {
		if scanned[i] != o.hash() {
			t.Fatalf("scan order differs at %d", i)
		}
	}
}

func TestWriterDeduplicates(t *testing.T) {
	o := obj{object.TypeChunk, []byte("same")}
	data := writePack(t, []obj{o, o, o})
	p, err := Open(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(p.Entries()); n != 1 {
		t.Fatalf("%d entries, want 1", n)
	}
}

func TestEmptyPack(t *testing.T) {
	data := writePack(t, nil)
	p, err := Open(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Verify(); err != nil {
		t.Fatal(err)
	}
}

// The golden pack holds only records too small to compress, so its bytes do
// not depend on the zstd implementation.
func TestGoldenPack(t *testing.T) {
	data := writePack(t, []obj{
		{object.TypeChunk, []byte("hello")},
		{object.TypeChunk, []byte("world")},
		{object.TypeGitRaw, []byte{0x43, 'a', 'b', 'c'}},
	})
	testutil.Golden(t, "pack", map[string]string{"pack": hex.EncodeToString(data)})
}

func TestCorruption(t *testing.T) {
	objs := sampleObjs(t)
	good := writePack(t, objs)
	target := objs[2].hash() // compressed record
	p, err := Open(bytes.NewReader(good), int64(len(good)))
	if err != nil {
		t.Fatal(err)
	}
	e, _ := p.Lookup(target)
	tests := []struct {
		name string
		at   int
	}{
		{"header magic", 0},
		{"version", 4},
		{"record type", int(e.Offset)},
		{"record hash", int(e.Offset) + 2},
		{"record size", int(e.Offset) + 34},
		{"payload", int(e.Offset) + RecordHeaderSize + 10},
		{"index", len(good) - TrailerSize - 10},
		{"index offset", len(good) - TrailerSize},
		{"checksum", len(good) - 10},
		{"trailer magic", len(good) - 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bad := bytes.Clone(good)
			bad[tt.at] ^= 0x40
			p, err := Open(bytes.NewReader(bad), int64(len(bad)))
			if err != nil {
				return // detected at open
			}
			if err := p.Verify(); err == nil {
				t.Fatal("Verify accepted a corrupt pack")
			}
		})
	}
	t.Run("payload detected by Get", func(t *testing.T) {
		bad := bytes.Clone(good)
		bad[int(e.Offset)+RecordHeaderSize+10] ^= 0x40
		p, err := Open(bytes.NewReader(bad), int64(len(bad)))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := p.Get(target); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("Get = %v, want ErrCorrupt", err)
		}
	})
	t.Run("truncated", func(t *testing.T) {
		for _, n := range []int{0, 7, HeaderSize + TrailerSize, len(good) - 1} {
			if p, err := Open(bytes.NewReader(good[:n]), int64(n)); err == nil {
				if err := p.Verify(); err == nil {
					t.Fatalf("truncated pack of %d bytes accepted", n)
				}
			}
		}
	})
}

func TestNewRecordLimits(t *testing.T) {
	if _, err := NewRecord(0, hash.Zero, nil, nil); err == nil {
		t.Error("type 0 accepted")
	}
	if _, err := NewRecord(object.TypeChunk, hash.Zero, make([]byte, MaxObjectSize+1), nil); err == nil {
		t.Error("oversized object accepted")
	}
}

func FuzzPack(f *testing.F) {
	f.Add(writePack(f, sampleObjs(f)))
	f.Add(writePack(f, nil))
	f.Fuzz(func(t *testing.T, data []byte) {
		if p, err := Open(bytes.NewReader(data), int64(len(data))); err == nil {
			_ = p.Verify()
			for _, e := range p.Entries() {
				_, _, _ = p.Get(e.Hash)
			}
		}
		_ = Scan(bytes.NewReader(data), func(r Record) error {
			_, err := r.Payload(nil)
			return err
		})
	})
}

func BenchmarkNewRecordChunk(b *testing.B) {
	data := testutil.Bytes("bench", 64<<10)
	// Make it partly compressible, like typical files.
	copy(data[32<<10:], bytes.Repeat([]byte("text "), 32<<10/5))
	h := object.ChunkHash(data)
	dst := make([]byte, 0, 64<<10)
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		if _, err := NewRecord(object.TypeChunk, h, data, dst); err != nil {
			b.Fatal(err)
		}
	}
}
