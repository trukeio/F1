package s3

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"hash/crc64"
	"io"
	"math/rand/v2"
	"strings"
	"testing"
)

func TestCRC64NVME(t *testing.T) {
	// Check value of CRC-64/NVME.
	if got := crc64.Checksum([]byte("123456789"), nvme); got != 0xae8b14860a799888 {
		t.Fatalf("CRC-64/NVME(123456789) = %x", got)
	}
}

// Combining part CRCs gives the CRC of the concatenation.
func TestCRCCombine(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for _, alg := range []string{CRC32, CRC32C, CRC64NVME} {
		for trial := range 50 {
			nparts := 1 + rng.IntN(5)
			var all []byte
			var sums [][]byte
			var sizes []int64
			for range nparts {
				p := make([]byte, rng.IntN(3000))
				for i := range p {
					p[i] = byte(rng.Uint32())
				}
				h := newChecksum(alg)
				h.Write(p)
				sums = append(sums, h.Sum(nil))
				sizes = append(sizes, int64(len(p)))
				all = append(all, p...)
			}
			h := newChecksum(alg)
			h.Write(all)
			got, ok := combineFullObject(alg, sums, sizes)
			if !ok || !bytes.Equal(got, h.Sum(nil)) {
				t.Fatalf("%s trial %d: combined %x, want %x", alg, trial, got, h.Sum(nil))
			}
		}
	}
	if _, ok := combineFullObject(SHA256, nil, nil); ok {
		t.Fatal("SHA-256 does not combine")
	}
	// A large length exercises many squarings.
	a, b := crc32.ChecksumIEEE([]byte("x")), crc32.ChecksumIEEE(nil)
	_ = crcCombine(uint64(crc32.IEEE), 32, uint64(a), uint64(b), 1<<40)
}

func TestParseRange(t *testing.T) {
	tests := []struct {
		h             string
		size          int64
		start, length int64
		partial       bool
		err           bool
	}{
		{"", 10, 0, 10, false, false},
		{"bytes=0-4", 10, 0, 5, true, false},
		{"bytes=5-", 10, 5, 5, true, false},
		{"bytes=-3", 10, 7, 3, true, false},
		{"bytes=-30", 10, 0, 10, true, false},
		{"bytes=8-100", 10, 8, 2, true, false},
		{"bytes=10-", 10, 0, 0, false, true},
		{"bytes=-0", 10, 0, 0, false, true},
		{"bytes=0-", 0, 0, 0, false, true},
		{"bytes=0-1,3-4", 10, 0, 10, false, false},
		{"bytes=5-2", 10, 0, 10, false, false},
		{"items=0-1", 10, 0, 10, false, false},
	}
	for _, tt := range tests {
		start, length, partial, err := parseRange(tt.h, tt.size)
		if (err != nil) != tt.err || (err == nil && (start != tt.start || length != tt.length || partial != tt.partial)) {
			t.Errorf("parseRange(%q, %d) = %d %d %v %v", tt.h, tt.size, start, length, partial, err)
		}
	}
}

func TestETagMatches(t *testing.T) {
	for _, tt := range []struct {
		h    string
		want bool
	}{
		{`"abc"`, true}, {`abc`, true}, {`*`, true}, {`"x", "abc"`, true}, {`W/"abc"`, true}, {`"abd"`, false}, {``, false},
	} {
		if got := etagMatches(tt.h, "abc"); got != tt.want {
			t.Errorf("etagMatches(%q) = %v", tt.h, got)
		}
	}
}

func encodeChunked(data []byte, chunk int, trailers string) string {
	var b strings.Builder
	for off := 0; off < len(data); off += chunk {
		p := data[off:min(off+chunk, len(data))]
		fmt.Fprintf(&b, "%x\r\n%s\r\n", len(p), p)
	}
	b.WriteString("0\r\n")
	b.WriteString(trailers)
	b.WriteString("\r\n")
	return b.String()
}

func TestChunkedUnsigned(t *testing.T) {
	data := bytes.Repeat([]byte("abcdefg"), 1000)
	for _, chunk := range []int{1, 100, 4096, len(data)} {
		cr := newChunkedReader(strings.NewReader(encodeChunked(data, chunk, "x-amz-checksum-crc32:AAAAAA==\r\n")), nil, true)
		got, err := io.ReadAll(cr)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("chunk %d: %d bytes, %v", chunk, len(got), err)
		}
		if cr.Trailers["x-amz-checksum-crc32"] != "AAAAAA==" {
			t.Fatalf("trailers %v", cr.Trailers)
		}
	}
	// No trailer, and a missing final CRLF.
	got, err := io.ReadAll(newChunkedReader(strings.NewReader("3\r\nabc\r\n0\r\n"), nil, false))
	if err != nil || string(got) != "abc" {
		t.Fatalf("%q %v", got, err)
	}
	for _, bad := range []string{
		"3\r\nab",                // truncated data
		"3\r\nabcX\r\n0\r\n\r\n", // no CRLF after data
		"zz\r\nabc\r\n",          // bad size
		"3\r\nabc\r\n",           // no last chunk
		strings.Repeat("1", 5000) + "\r\n",
	} {
		if _, err := io.ReadAll(newChunkedReader(strings.NewReader(bad), nil, false)); err == nil {
			t.Errorf("accepted %q", bad[:min(len(bad), 20)])
		}
	}
}

// FuzzChunked checks that the aws-chunked decoder never panics, never
// returns more bytes than declared, and round-trips valid encodings.
func FuzzChunked(f *testing.F) {
	f.Add([]byte("3\r\nabc\r\n0\r\n\r\n"), true)
	f.Add([]byte("0\r\nx-amz-checksum-crc32:AAAAAA==\r\n\r\n"), true)
	f.Add([]byte("ffffffffffffffff\r\n"), false)
	f.Add([]byte("1;chunk-signature=00\r\na\r\n"), false)
	f.Fuzz(func(t *testing.T, body []byte, trailer bool) {
		cr := newChunkedReader(bytes.NewReader(body), nil, trailer)
		n, err := io.Copy(io.Discard, cr)
		if err == nil && n > int64(len(body)) {
			t.Fatalf("decoded %d bytes from %d", n, len(body))
		}
		var e *Error
		if err != nil && !errors.As(err, &e) {
			t.Fatalf("unexpected error type %T: %v", err, err)
		}
		// Round trip: encoding the input as data decodes to it.
		enc := encodeChunked(body, 1+int(binary.LittleEndian.Uint16(append(body, 0, 0)[:2]))%64, "")
		got, err := io.ReadAll(newChunkedReader(strings.NewReader(enc), nil, false))
		if err != nil || !bytes.Equal(got, body) {
			t.Fatalf("round trip: %v", err)
		}
	})
}
