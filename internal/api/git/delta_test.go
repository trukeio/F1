package git

import (
	"bufio"
	"bytes"
	"math/rand/v2"
	"testing"

	"github.com/trukeio/f1/internal/testutil"
)

// applyTestDelta applies a delta with the push code's decoder.
func applyTestDelta(t *testing.T, base, delta []byte) []byte {
	t.Helper()
	dr := bufio.NewReader(bytes.NewReader(delta))
	src, err := readVarint(dr)
	if err != nil || src != int64(len(base)) {
		t.Fatalf("base size %d, %v", src, err)
	}
	dst, err := readVarint(dr)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runDelta(dr, bytes.NewReader(base), int64(len(base)), dst, &out); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestDeltaRoundTrip(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 1))
	text := func(n int) []byte {
		var b bytes.Buffer
		for i := 0; b.Len() < n; i++ {
			b.WriteString("a line of source code, number ")
			b.WriteByte(byte('0' + i%10))
			b.WriteByte('\n')
		}
		return b.Bytes()[:n]
	}
	for iter := range 200 {
		base := text(100 + r.IntN(200000))
		if iter%3 == 0 {
			base = testutil.Bytes(string(rune(iter)), 100+r.IntN(200000))
		}
		target := append([]byte(nil), base...)
		for range 1 + r.IntN(10) {
			p := r.IntN(len(target))
			switch r.IntN(3) {
			case 0: // insert
				target = append(target[:p], append([]byte("INSERTED TEXT"), target[p:]...)...)
			case 1: // delete
				q := min(len(target), p+r.IntN(500))
				target = append(target[:p], target[q:]...)
			default: // replace
				for i := p; i < min(len(target), p+20); i++ {
					target[i] = byte(r.IntN(256))
				}
			}
		}
		d := makeDelta(base, target)
		if d == nil {
			if len(target) > 1000 {
				t.Fatalf("iteration %d: no delta for a small edit of %d bytes", iter, len(target))
			}
			continue
		}
		if len(d) > len(target)/2 {
			t.Fatalf("delta %d bytes for %d", len(d), len(target))
		}
		if got := applyTestDelta(t, base, d); !bytes.Equal(got, target) {
			t.Fatalf("iteration %d: delta result differs", iter)
		}
	}
	// Unrelated content gives no delta.
	if d := makeDelta(testutil.Bytes("a", 50000), testutil.Bytes("b", 50000)); d != nil {
		t.Fatal("delta between unrelated content")
	}
	// Long copies are split at the instruction limit, 0x10000 included.
	big := testutil.Bytes("big", maxCopy+0x10000+100)
	if got := applyTestDelta(t, big, makeDelta(big, big)); !bytes.Equal(got, big) {
		t.Fatal("long copy")
	}
}

func TestOfsOffset(t *testing.T) {
	// Decode as the push reader does.
	for _, d := range []int64{1, 127, 128, 16511, 16512, 1 << 30} {
		b := ofsOffset(d)
		var v int64
		v = int64(b[0] & 0x7f)
		for i := 1; i < len(b); i++ {
			v = ((v + 1) << 7) | int64(b[i]&0x7f)
		}
		if v != d {
			t.Fatalf("offset %d encodes to %x, decodes to %d", d, b, v)
		}
	}
}

func FuzzDelta(f *testing.F) {
	f.Add([]byte("hello world, hello world, hello world"), []byte("hello world, HELLO world, hello world!"))
	f.Fuzz(func(t *testing.T, base, target []byte) {
		if d := makeDelta(base, target); d != nil {
			if got := applyTestDelta(t, base, d); !bytes.Equal(got, target) {
				t.Fatal("delta result differs")
			}
		}
	})
}
