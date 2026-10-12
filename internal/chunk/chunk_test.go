package chunk

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
	"testing/iotest"

	"lukechampine.com/blake3"

	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/testutil"
)

func TestGearTable(t *testing.T) {
	h := blake3.New(32, nil)
	h.Write([]byte("F1 FastCDC gear table v1"))
	buf := make([]byte, 256*8)
	if _, err := h.XOF().Read(buf); err != nil {
		t.Fatal(err)
	}
	for i := range gear {
		if want := binary.LittleEndian.Uint64(buf[i*8:]); gear[i] != want {
			t.Fatalf("gear[%d] = %#x, want %#x", i, gear[i], want)
		}
	}
}

func chunkAll(t testing.TB, r io.Reader) [][]byte {
	t.Helper()
	c := New(r)
	var out [][]byte
	for {
		b, err := c.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, bytes.Clone(b))
	}
}

type goldenChunk struct {
	Offset int       `json:"offset"`
	Length int       `json:"length"`
	Hash   hash.Hash `json:"hash"` // chunk object hash, BLAKE3(0x01 ‖ bytes)
}

func TestGoldenChunks(t *testing.T) {
	inputs := []struct {
		name string
		data []byte
	}{
		{"empty", nil},
		{"short", testutil.Bytes("F1 chunk golden short", 1000)},
		{"exactly-min", testutil.Bytes("F1 chunk golden min", MinSize)},
		{"random-4MiB", testutil.Bytes("F1 chunk golden random", 4<<20)},
		{"zeros-1MiB", make([]byte, 1<<20)},
		{"text", bytes.Repeat([]byte("The quick brown fox jumps over the lazy dog.\n"), 20000)},
	}
	got := map[string][]goldenChunk{}
	for _, in := range inputs {
		chunks := chunkAll(t, bytes.NewReader(in.data))
		off := 0
		list := []goldenChunk{}
		for _, c := range chunks {
			list = append(list, goldenChunk{off, len(c), hash.Sum(0x01, c)})
			off += len(c)
		}
		if off != len(in.data) {
			t.Fatalf("%s: chunks cover %d bytes, want %d", in.name, off, len(in.data))
		}
		got[in.name] = list
	}
	testutil.Golden(t, "chunks", got)
}

func TestChunkSizeBounds(t *testing.T) {
	data := testutil.Bytes("F1 chunk bounds", 16<<20)
	chunks := chunkAll(t, bytes.NewReader(data))
	total := 0
	for i, c := range chunks {
		if len(c) > MaxSize {
			t.Fatalf("chunk %d has %d bytes > MaxSize", i, len(c))
		}
		if len(c) < MinSize && i != len(chunks)-1 {
			t.Fatalf("chunk %d has %d bytes < MinSize", i, len(c))
		}
		total += len(c)
	}
	avg := total / len(chunks)
	t.Logf("%d chunks, average %d bytes", len(chunks), avg)
	if avg < AvgSize/2 || avg > 2*AvgSize {
		t.Errorf("average chunk size %d is far from %d", avg, AvgSize)
	}
}

// Boundaries do not depend on how the reader splits its output.
func TestReaderIndependence(t *testing.T) {
	data := testutil.Bytes("F1 chunk reader", 3<<20+12345)
	want := chunkAll(t, bytes.NewReader(data))
	for _, r := range []io.Reader{
		iotest.OneByteReader(bytes.NewReader(data)),
		iotest.HalfReader(bytes.NewReader(data)),
		iotest.DataErrReader(bytes.NewReader(data)),
	} {
		got := chunkAll(t, r)
		if len(got) != len(want) {
			t.Fatalf("got %d chunks, want %d", len(got), len(want))
		}
		for i := range got {
			if !bytes.Equal(got[i], want[i]) {
				t.Fatalf("chunk %d differs", i)
			}
		}
	}
}

func TestReadError(t *testing.T) {
	boom := errors.New("boom")
	c := New(iotest.ErrReader(boom))
	if _, err := c.Next(); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
}

// Inserting or deleting bytes in the middle of a file changes at most 3
// chunks around the edit.
func TestEditLocality(t *testing.T) {
	base := testutil.Bytes("F1 chunk edit", 8<<20)
	edits := []struct {
		name string
		at   int
		del  int
		ins  []byte
	}{
		{"insert-1", 4 << 20, 0, []byte{0x42}},
		{"insert-100", 4<<20 + 777, 0, testutil.Bytes("ins", 100)},
		{"insert-300KiB", 3 << 20, 0, testutil.Bytes("ins big", 300<<10)},
		{"delete-1", 5 << 20, 1, nil},
		{"delete-5000", 2<<20 + 3, 5000, nil},
		{"overwrite-10", 6 << 20, 10, testutil.Bytes("ovw", 10)},
	}
	old := map[hash.Hash]bool{}
	for _, c := range chunkAll(t, bytes.NewReader(base)) {
		old[hash.Sum(0x01, c)] = true
	}
	for _, e := range edits {
		changed := 0
		for _, c := range chunkAll(t, bytes.NewReader(splice(base, e.at, e.del, e.ins))) {
			if !old[hash.Sum(0x01, c)] {
				changed++
			}
		}
		// Inserted data needs chunks of its own (at most one per MinSize
		// bytes); beyond those, at most 3 chunks around the edit change.
		limit := 3 + len(e.ins)/MinSize
		if changed > limit {
			t.Errorf("%s: %d new chunks, want at most %d", e.name, changed, limit)
		}
	}
}

func splice(base []byte, at, del int, ins []byte) []byte {
	out := make([]byte, 0, len(base)-del+len(ins))
	out = append(out, base[:at]...)
	out = append(out, ins...)
	return append(out, base[at+del:]...)
}

func BenchmarkChunker(b *testing.B) {
	data := testutil.Bytes("F1 chunk bench", 64<<20)
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for b.Loop() {
		c := New(bytes.NewReader(data))
		for {
			if _, err := c.Next(); err != nil {
				break
			}
		}
	}
}
