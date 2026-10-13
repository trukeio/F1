package s3

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"hash"
	"hash/crc32"
	"hash/crc64"
	"net/http"
	"strings"
)

// Checksum algorithms (x-amz-checksum-<name>), in the lower-case form used
// in header names and in attrs.cks.
const (
	CRC32     = "crc32"
	CRC32C    = "crc32c"
	CRC64NVME = "crc64nvme"
	SHA1      = "sha1"
	SHA256    = "sha256"
)

// Checksum types of multipart uploads.
const (
	Composite  = "COMPOSITE"
	FullObject = "FULL_OBJECT"
)

var checksumAlgorithms = []string{CRC32, CRC32C, CRC64NVME, SHA1, SHA256}

// crc64NVMEPoly is the reflected CRC-64/NVME polynomial.
const crc64NVMEPoly = 0x9a6c9329ac4bc9b5

var (
	castagnoli = crc32.MakeTable(crc32.Castagnoli)
	nvme       = crc64.MakeTable(crc64NVMEPoly)
)

func newChecksum(alg string) hash.Hash {
	switch alg {
	case CRC32:
		return crc32.NewIEEE()
	case CRC32C:
		return crc32.New(castagnoli)
	case CRC64NVME:
		return crc64.New(nvme)
	case SHA1:
		return sha1.New()
	case SHA256:
		return sha256.New()
	}
	return nil
}

// validAlgorithm normalizes an algorithm name ("CRC32C" → "crc32c").
func validAlgorithm(name string) (string, bool) {
	a := strings.ToLower(name)
	return a, newChecksum(a) != nil
}

func checksumHeader(alg string) string { return "x-amz-checksum-" + alg }

// requestChecksums returns the checksums sent as headers, decoded, and the
// algorithm a trailer will carry (x-amz-trailer), if any.
func requestChecksums(h http.Header) (map[string][]byte, string, error) {
	sent := map[string][]byte{}
	for _, alg := range checksumAlgorithms {
		v := h.Get(checksumHeader(alg))
		if v == "" {
			continue
		}
		b, err := base64.StdEncoding.DecodeString(v)
		if err != nil || len(b) != newChecksum(alg).Size() {
			return nil, "", errCode("BadDigest", "The %s you specified is not a valid %s checksum.", checksumHeader(alg), alg)
		}
		sent[alg] = b
	}
	if len(sent) > 1 {
		return nil, "", errCode("InvalidRequest", "expecting a single x-amz-checksum- header")
	}
	trailer := ""
	if t := strings.ToLower(strings.TrimSpace(h.Get("X-Amz-Trailer"))); t != "" {
		alg, ok := strings.CutPrefix(t, "x-amz-checksum-")
		if !ok || newChecksum(alg) == nil {
			return nil, "", errCode("InvalidRequest", "unsupported trailer %q", t)
		}
		trailer = alg
	}
	return sent, trailer, nil
}

// crcCombine returns the CRC of A‖B from crcA, crcB and len(B), for the
// reflected CRCs used here (initial value and final XOR all ones), with the
// reflected polynomial poly of width bits. It is zlib's crc32_combine,
// generalized: crcA is advanced over len(B) zero bytes by squaring the
// one-zero-bit operator matrix over GF(2).
func crcCombine(poly uint64, width int, crcA, crcB uint64, lenB int64) uint64 {
	if lenB <= 0 {
		return crcA
	}
	odd := make([]uint64, width)  // operator for one zero bit
	even := make([]uint64, width) // operator for two zero bits
	odd[0] = poly
	row := uint64(1)
	for n := 1; n < width; n++ {
		odd[n] = row
		row <<= 1
	}
	gf2Square(even, odd) // 2 zero bits
	gf2Square(odd, even) // 4 zero bits
	for {
		gf2Square(even, odd) // first pass: 8 zero bits = 1 byte
		if lenB&1 != 0 {
			crcA = gf2Times(even, crcA)
		}
		lenB >>= 1
		if lenB == 0 {
			break
		}
		gf2Square(odd, even)
		if lenB&1 != 0 {
			crcA = gf2Times(odd, crcA)
		}
		lenB >>= 1
		if lenB == 0 {
			break
		}
	}
	return crcA ^ crcB
}

func gf2Times(mat []uint64, vec uint64) uint64 {
	var sum uint64
	for i := 0; vec != 0; i, vec = i+1, vec>>1 {
		if vec&1 != 0 {
			sum ^= mat[i]
		}
	}
	return sum
}

func gf2Square(square, mat []uint64) {
	for n := range mat {
		square[n] = gf2Times(mat, mat[n])
	}
}

// combineFullObject returns the full-object CRC of the concatenation of
// parts from their CRCs and sizes. Only CRC algorithms combine.
func combineFullObject(alg string, sums [][]byte, sizes []int64) ([]byte, bool) {
	var poly uint64
	var width int
	switch alg {
	case CRC32:
		poly, width = uint64(crc32.IEEE), 32
	case CRC32C:
		poly, width = uint64(crc32.Castagnoli), 32
	case CRC64NVME:
		poly, width = crc64NVMEPoly, 64
	default:
		return nil, false
	}
	var crc uint64
	for i, s := range sums {
		var v uint64
		if width == 32 {
			v = uint64(binary.BigEndian.Uint32(s))
		} else {
			v = binary.BigEndian.Uint64(s)
		}
		if i == 0 {
			crc = v
		} else {
			crc = crcCombine(poly, width, crc, v, sizes[i])
		}
	}
	out := make([]byte, width/8)
	if width == 32 {
		binary.BigEndian.PutUint32(out, uint32(crc))
	} else {
		binary.BigEndian.PutUint64(out, crc)
	}
	return out, true
}

// composite returns the composite checksum of parts: the algorithm over the
// concatenated part checksums (the value is followed by "-<parts>").
func composite(alg string, sums [][]byte) []byte {
	h := newChecksum(alg)
	for _, s := range sums {
		h.Write(s)
	}
	return h.Sum(nil)
}
