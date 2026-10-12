// Package chunk implements content-defined chunking with FastCDC (D2).
//
// The algorithm is FastCDC with normalized chunking level 2 (Xia et al.,
// "The Design of Fast Content-Defined Chunking for Data Deduplication Based
// Storage Systems", 2020). Every constant here is part of the on-disk format:
// changing any of them changes chunk boundaries and breaks deduplication.
// See doc/format.md.
package chunk

import (
	"errors"
	"io"
)

// Chunk size parameters (D2).
const (
	MinSize = 16 << 10
	AvgSize = 64 << 10
	MaxSize = 256 << 10
)

// Normalized chunking masks: before AvgSize a cut needs the 18 most
// significant bits of the gear hash to be zero (harder), from AvgSize on the
// 14 most significant bits (easier). The most significant bits of the gear
// hash depend on the last 64 input bytes.
const (
	maskS uint64 = 0xFFFF_C000_0000_0000 // 18 most significant bits
	maskL uint64 = 0xFFFC_0000_0000_0000 // 14 most significant bits
)

// Cut returns the length of the first chunk of data. data must hold at least
// MaxSize bytes unless it is the final part of the input.
func Cut(data []byte) int {
	n := len(data)
	if n <= MinSize {
		return n
	}
	if n > MaxSize {
		n = MaxSize
	}
	normal := AvgSize
	if n < normal {
		normal = n
	}
	var fp uint64
	i := MinSize
	for ; i < normal; i++ {
		fp = (fp << 1) + gear[data[i]]
		if fp&maskS == 0 {
			return i + 1
		}
	}
	for ; i < n; i++ {
		fp = (fp << 1) + gear[data[i]]
		if fp&maskL == 0 {
			return i + 1
		}
	}
	return n
}

// bufSize is the Chunker's read buffer. A multiple of MaxSize amortizes the
// copy of the unconsumed tail on refill.
const bufSize = 4 * MaxSize

// Chunker splits a stream into chunks.
type Chunker struct {
	r          io.Reader
	buf        []byte
	start, end int
	eof        bool
}

// New returns a Chunker reading from r.
func New(r io.Reader) *Chunker {
	return &Chunker{r: r, buf: make([]byte, bufSize)}
}

// Next returns the next chunk. The slice is valid until the next call. At the
// end of the input it returns nil, io.EOF. An empty input yields no chunks.
func (c *Chunker) Next() ([]byte, error) {
	if c.end-c.start < MaxSize && !c.eof {
		if err := c.fill(); err != nil {
			return nil, err
		}
	}
	if c.start == c.end {
		return nil, io.EOF
	}
	n := Cut(c.buf[c.start:c.end])
	b := c.buf[c.start : c.start+n]
	c.start += n
	return b, nil
}

func (c *Chunker) fill() error {
	c.end = copy(c.buf, c.buf[c.start:c.end])
	c.start = 0
	for c.end < len(c.buf) {
		n, err := c.r.Read(c.buf[c.end:])
		c.end += n
		if errors.Is(err, io.EOF) {
			c.eof = true
			return nil
		}
		if err != nil {
			return err
		}
	}
	return nil
}
