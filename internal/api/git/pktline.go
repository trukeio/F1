package git

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// Special pkt-lines.
const (
	pktFlush    = "0000"
	pktDelim    = "0001"
	pktResponse = "0002" // response-end (protocol v2 stateless)
)

// maxPktData is the largest payload of a pkt-line (65520 bytes in total).
const maxPktData = 65516

// pktWriter writes pkt-lines.
type pktWriter struct {
	w   io.Writer
	err error
}

func (p *pktWriter) write(data []byte) {
	if p.err != nil {
		return
	}
	if len(data) > maxPktData {
		p.err = errors.New("git: pkt-line too long")
		return
	}
	if _, err := fmt.Fprintf(p.w, "%04x", len(data)+4); err != nil {
		p.err = err
		return
	}
	_, p.err = p.w.Write(data)
}

// line writes a text pkt-line.
func (p *pktWriter) line(format string, args ...any) {
	p.write([]byte(fmt.Sprintf(format, args...)))
}

func (p *pktWriter) special(s string) {
	if p.err == nil {
		_, p.err = io.WriteString(p.w, s)
	}
}

func (p *pktWriter) flush() { p.special(pktFlush) }
func (p *pktWriter) delim() { p.special(pktDelim) }

// sideband sends data on band 1 of side-band-64k, in packets of at most
// maxPktData-1 bytes.
type sideband struct {
	p *pktWriter
}

func (s *sideband) Write(b []byte) (int, error) {
	n := 0
	for len(b) > 0 {
		chunk := b[:min(len(b), maxPktData-1)]
		s.p.write(append([]byte{1}, chunk...))
		if s.p.err != nil {
			return n, s.p.err
		}
		n += len(chunk)
		b = b[len(chunk):]
	}
	return n, nil
}

// pktReader reads pkt-lines.
type pktReader struct {
	r *bufio.Reader
}

// Packet kinds returned by next.
const (
	pktData = iota
	pktIsFlush
	pktIsDelim
	pktIsResponseEnd
	pktEOF
)

// maxRequest bounds the pkt-lines of a request (wants, haves, arguments):
// generous for large negotiations, finite for safety.
const maxRequestLines = 1 << 20

// next returns the next packet: its kind and, for data packets, the payload
// without a trailing newline.
func (p *pktReader) next() (int, []byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(p.r, hdr[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return pktEOF, nil, nil
		}
		return 0, nil, fmt.Errorf("git: reading pkt-line: %w", err)
	}
	n, err := strconv.ParseUint(string(hdr[:]), 16, 16)
	if err != nil {
		return 0, nil, fmt.Errorf("git: malformed pkt-line length %q", hdr[:])
	}
	switch n {
	case 0:
		return pktIsFlush, nil, nil
	case 1:
		return pktIsDelim, nil, nil
	case 2:
		return pktIsResponseEnd, nil, nil
	case 3:
		return 0, nil, errors.New("git: malformed pkt-line length 3")
	}
	data := make([]byte, n-4)
	if _, err := io.ReadFull(p.r, data); err != nil {
		return 0, nil, fmt.Errorf("git: reading pkt-line: %w", err)
	}
	if l := len(data); l > 0 && data[l-1] == '\n' {
		data = data[:l-1]
	}
	return pktData, data, nil
}
