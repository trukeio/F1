package s3

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"hash"
	"io"
	"strconv"
	"strings"

	"github.com/trukeio/f1/internal/auth"
)

// maxChunkLine bounds a chunk header or trailer line.
const maxChunkLine = 4096

// chunkedReader decodes an aws-chunked request body:
//
//	<hex size>[;chunk-signature=<sig>]\r\n<data>\r\n ... 0[;chunk-signature=<sig>]\r\n
//
// followed by "\r\n" or, with trailers, by "<name>:<value>\r\n" lines
// (with signed trailers, ending with x-amz-trailer-signature) and "\r\n".
// With a signature, every chunk signature is verified at the end of its
// chunk, before the next chunk is read, chained from the request
// signature; a mismatch fails the read, so the caller never commits a
// partly verified body. Memory use is bounded by the bufio buffer.
type chunkedReader struct {
	br      *bufio.Reader
	sig     *auth.SigV4Result // nil: unsigned chunks
	prev    string            // previous signature
	trailer bool              // trailers follow the last chunk

	left     int64     // bytes left in the current chunk
	chunkSig string    // expected signature of the current chunk
	h        hash.Hash // SHA-256 of the current chunk's data
	done     bool
	err      error

	// Trailers holds the trailer values once Read returned io.EOF.
	Trailers map[string]string
}

var (
	errChunkFormat = errCode("IncompleteBody", "malformed aws-chunked body")
	errChunkSig    = errCode("SignatureDoesNotMatch", "the chunk signature does not match")
)

func newChunkedReader(r io.Reader, sig *auth.SigV4Result, trailer bool) *chunkedReader {
	c := &chunkedReader{br: bufio.NewReaderSize(r, 64<<10), sig: sig, trailer: trailer, Trailers: map[string]string{}}
	if sig != nil {
		c.prev = sig.Signature
		c.h = sha256.New()
	}
	return c
}

func (c *chunkedReader) Read(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	for c.left == 0 {
		if c.done {
			return 0, io.EOF
		}
		if err := c.next(); err != nil {
			c.err = err
			return 0, err
		}
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.br.Read(p)
	c.left -= int64(n)
	if c.h != nil {
		c.h.Write(p[:n])
	}
	if err == io.EOF {
		err = errChunkFormat
	}
	if err != nil {
		c.err = err
		return n, err
	}
	if c.left == 0 {
		if err := c.endChunk(); err != nil {
			c.err = err
			return n, err
		}
	}
	return n, nil
}

// next reads a chunk header. A zero-size chunk ends the data.
func (c *chunkedReader) next() error {
	line, err := c.line()
	if errors.Is(err, io.EOF) {
		return errChunkFormat // the body ended before the last chunk
	}
	if err != nil {
		return err
	}
	sizeStr, ext, _ := strings.Cut(line, ";")
	size, err := strconv.ParseInt(strings.TrimSpace(sizeStr), 16, 64)
	if err != nil || size < 0 {
		return errChunkFormat
	}
	if c.sig != nil {
		s, ok := strings.CutPrefix(ext, "chunk-signature=")
		if !ok {
			return errChunkFormat
		}
		c.chunkSig = s
		c.h.Reset()
	}
	if size > 0 {
		c.left = size
		return nil
	}
	// Last chunk.
	if c.sig != nil {
		if err := c.checkChunkSig(); err != nil {
			return err
		}
	}
	c.done = true
	if c.trailer {
		return c.readTrailers()
	}
	// The final CRLF; some clients omit it.
	if line, err := c.line(); err == nil && line != "" {
		return errChunkFormat
	}
	return nil
}

// endChunk consumes the CRLF after a chunk's data and checks its signature.
func (c *chunkedReader) endChunk() error {
	var crlf [2]byte
	if _, err := io.ReadFull(c.br, crlf[:]); err != nil || crlf != [2]byte{'\r', '\n'} {
		return errChunkFormat
	}
	if c.sig != nil {
		return c.checkChunkSig()
	}
	return nil
}

func (c *chunkedReader) checkChunkSig() error {
	want := c.sig.ChunkSignature(c.prev, c.h.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(c.chunkSig)) {
		return errChunkSig
	}
	c.prev = c.chunkSig
	return nil
}

func (c *chunkedReader) readTrailers() error {
	var canonical bytes.Buffer
	trailerSig := ""
	for {
		line, err := c.line()
		if errors.Is(err, io.EOF) && line == "" {
			break // no final CRLF
		}
		if err != nil {
			return err
		}
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return errChunkFormat
		}
		name = strings.ToLower(strings.TrimSpace(name))
		value = strings.TrimSpace(value)
		if name == "x-amz-trailer-signature" {
			trailerSig = value
			continue
		}
		c.Trailers[name] = value
		canonical.WriteString(name + ":" + value + "\n")
	}
	if c.sig != nil {
		sum := sha256.Sum256(canonical.Bytes())
		if trailerSig == "" || !hmac.Equal([]byte(c.sig.TrailerSignature(c.prev, sum[:])), []byte(trailerSig)) {
			return errCode("SignatureDoesNotMatch", "the trailer signature does not match")
		}
	}
	return nil
}

// line reads a CRLF-terminated line of at most maxChunkLine bytes.
func (c *chunkedReader) line() (string, error) {
	var b []byte
	for {
		frag, err := c.br.ReadSlice('\n')
		b = append(b, frag...)
		if len(b) > maxChunkLine {
			return "", errChunkFormat
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err == io.EOF {
			if len(b) == 0 {
				return "", io.EOF
			}
			return "", errChunkFormat
		}
		if err != nil {
			return "", err
		}
		break
	}
	s := string(b)
	if !strings.HasSuffix(s, "\r\n") {
		return "", errChunkFormat
	}
	return s[:len(s)-2], nil
}
