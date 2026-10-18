package svndump

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
)

// svndiff, Subversion's delta format (subversion/libsvn_delta/svndiff.c):
// "SVN" and a version byte, then windows. Each window builds a target view
// from a source view (a range of the base), its own earlier output, and
// new data:
//
//	source view offset, source view length, target view length,
//	instructions length, new data length   (varints)
//	instructions, new data
//
// An instruction is a byte with the opcode in its two top bits (0: copy
// from the source view, 1: copy from the target view, 2: copy from new
// data) and the length in the other six, or 0 and the length as a varint.
// Copies from a view are followed by an offset varint. Version 1
// compresses the two sections with zlib, version 2 with LZ4.

// maxWindow bounds every length in a window, so a window holds at most a
// few times this in memory. Subversion writes windows of 100 KiB.
const maxWindow = 16 << 20

// ErrLZ4 is returned for svndiff version 2, which Subversion's dumps do
// not use.
var ErrLZ4 = errors.New("svndump: svndiff version 2 (LZ4) is not supported; dump without compression")

// Apply decodes the svndiff delta and writes its result to dst, reading
// source views from base, which may be nil for a delta that uses none.
// It returns the number of bytes written.
func Apply(dst io.Writer, delta io.Reader, base io.ReaderAt) (int64, error) {
	br := bufio.NewReaderSize(delta, 64<<10)
	var hdr [4]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return 0, fmt.Errorf("svndiff: header: %w", unexpected(err))
	}
	if string(hdr[:3]) != "SVN" {
		return 0, errors.New("svndiff: not an svndiff delta")
	}
	version := hdr[3]
	switch version {
	case 0, 1:
	case 2:
		return 0, ErrLZ4
	default:
		return 0, fmt.Errorf("svndiff: unknown version %d", version)
	}
	var (
		written                 int64
		source, target, ins, nd []byte
	)
	for w := 0; ; w++ {
		fail := func(format string, a ...any) error {
			return fmt.Errorf("svndiff: window %d: %s", w, fmt.Sprintf(format, a...))
		}
		sviewOff, err := readVarint(br)
		if err == io.EOF {
			return written, nil
		}
		if err != nil {
			return written, fail("%v", err)
		}
		var lens [4]uint64 // source view, target view, instructions, new data
		for i := range lens {
			if lens[i], err = readVarint(br); err != nil {
				return written, fail("%v", unexpected(err))
			}
			if lens[i] > maxWindow {
				return written, fail("a length of %d bytes is too large", lens[i])
			}
		}
		sviewLen, tviewLen := int(lens[0]), int(lens[1])
		if ins, err = readSection(br, ins, int(lens[2]), version); err != nil {
			return written, fail("instructions: %v", err)
		}
		if nd, err = readSection(br, nd, int(lens[3]), version); err != nil {
			return written, fail("new data: %v", err)
		}
		source = grow(source, sviewLen)
		if sviewLen > 0 {
			if base == nil {
				return written, fail("the delta needs a base")
			}
			if sviewOff > 1<<62 {
				return written, fail("invalid source view offset %d", sviewOff)
			}
			n, err := base.ReadAt(source, int64(sviewOff))
			if n < sviewLen {
				if err == nil || err == io.EOF {
					err = fmt.Errorf("the base ends before %d", sviewOff+uint64(sviewLen))
				}
				return written, fail("source view: %v", err)
			}
		}
		target = grow(target, tviewLen)
		if err := runWindow(target, source, ins, nd); err != nil {
			return written, fail("%v", err)
		}
		n, err := dst.Write(target)
		written += int64(n)
		if err != nil {
			return written, err
		}
	}
}

// runWindow fills target by running the instructions.
func runWindow(target, source, ins, nd []byte) error {
	r := bytes.NewReader(ins)
	tpos, npos := 0, 0
	for r.Len() > 0 {
		b, _ := r.ReadByte()
		op, n := b>>6, uint64(b&0x3f)
		var err error
		if n == 0 {
			if n, err = readVarint(r); err != nil {
				return fmt.Errorf("instruction length: %v", unexpected(err))
			}
		}
		if n > uint64(len(target)-tpos) {
			return errors.New("an instruction writes past the target view")
		}
		l := int(n)
		switch op {
		case 0, 1:
			off, err := readVarint(r)
			if err != nil {
				return fmt.Errorf("instruction offset: %v", unexpected(err))
			}
			if op == 0 {
				if off > uint64(len(source)) || n > uint64(len(source))-off {
					return errors.New("a copy reads past the source view")
				}
				copy(target[tpos:], source[off:off+n])
			} else {
				if off >= uint64(tpos) {
					return errors.New("a copy reads past the target written so far")
				}
				// The ranges may overlap, which repeats a pattern: copy
				// byte by byte.
				for i, o := 0, int(off); i < l; i++ {
					target[tpos+i] = target[o+i]
				}
			}
		case 2:
			if l > len(nd)-npos {
				return errors.New("a copy reads past the new data")
			}
			copy(target[tpos:], nd[npos:npos+l])
			npos += l
		default:
			return errors.New("invalid instruction")
		}
		tpos += l
	}
	if tpos != len(target) {
		return fmt.Errorf("the instructions fill %d of %d target bytes", tpos, len(target))
	}
	if npos != len(nd) {
		return fmt.Errorf("the instructions use %d of %d bytes of new data", npos, len(nd))
	}
	return nil
}

// readSection reads a section of n bytes into buf. In version 1 it starts
// with the original length, and holds the original bytes if their lengths
// match, zlib data otherwise.
func readSection(br *bufio.Reader, buf []byte, n int, version byte) ([]byte, error) {
	buf = grow(buf, n)
	if _, err := io.ReadFull(br, buf); err != nil {
		return nil, unexpected(err)
	}
	if version == 0 || n == 0 {
		return buf, nil
	}
	r := bytes.NewReader(buf)
	orig, err := readVarint(r)
	if err != nil {
		return nil, unexpected(err)
	}
	if orig > maxWindow {
		return nil, fmt.Errorf("an original length of %d bytes is too large", orig)
	}
	rest := buf[len(buf)-r.Len():]
	if uint64(len(rest)) == orig {
		return rest, nil
	}
	zr, err := zlib.NewReader(r)
	if err != nil {
		return nil, err
	}
	out := make([]byte, orig)
	if _, err := io.ReadFull(zr, out); err != nil {
		return nil, unexpected(err)
	}
	if n, _ := zr.Read(make([]byte, 1)); n > 0 {
		return nil, errors.New("compressed data is longer than its original length")
	}
	return out, nil
}

// readVarint reads a big-endian base-128 integer: seven bits per byte,
// high bit set on all but the last. It returns io.EOF only if no byte was
// read.
func readVarint(r io.ByteReader) (uint64, error) {
	var v uint64
	for i := 0; ; i++ {
		b, err := r.ReadByte()
		if err != nil {
			if i > 0 {
				return 0, io.ErrUnexpectedEOF
			}
			return 0, err
		}
		if v > (1<<64-1)>>7 {
			return 0, errors.New("varint overflows 64 bits")
		}
		v = v<<7 | uint64(b&0x7f)
		if b&0x80 == 0 {
			return v, nil
		}
	}
}

// grow returns buf resized to n bytes, reusing its storage when it can.
func grow(buf []byte, n int) []byte {
	if cap(buf) < n {
		return make([]byte, n)
	}
	return buf[:n]
}
