package pack

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"

	"lukechampine.com/blake3"

	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
)

// Format constants. All integers are little-endian.
const (
	Magic        = "F1PK" // file header magic
	TrailerMagic = "F1PE" // trailer magic, the last 4 bytes of a pack
	Version      = 1

	HeaderSize       = 8  // magic, version u32
	RecordHeaderSize = 42 // type u8, flags u8, hash [32], size u32, stored u32
	EndOfRecords     = 0  // type byte that ends the record section
	IndexEntrySize   = 50 // hash [32], type u8, flags u8, offset u64, size u32, stored u32
	TrailerSize      = 48 // index offset u64, count u32, checksum [32], magic
)

// IndexEntry locates one record in a pack.
type IndexEntry struct {
	Hash   hash.Hash
	Type   object.Type
	Flags  uint8
	Offset uint64 // offset of the record header
	Size   uint32 // uncompressed payload length
	Stored uint32 // stored payload length
}

// Writer writes a pack sequentially.
type Writer struct {
	w       *bufio.Writer
	sum     *blake3.Hasher
	off     uint64
	index   []IndexEntry
	seen    map[hash.Hash]bool
	scratch [IndexEntrySize]byte
	done    bool
}

// NewWriter writes the pack header to w and returns a Writer.
func NewWriter(w io.Writer) (*Writer, error) {
	sum := blake3.New(hash.Size, nil)
	pw := &Writer{
		w:    bufio.NewWriterSize(io.MultiWriter(w, sum), 1<<20),
		sum:  sum,
		seen: map[hash.Hash]bool{},
	}
	var hdr [HeaderSize]byte
	copy(hdr[:], Magic)
	binary.LittleEndian.PutUint32(hdr[4:], Version)
	if err := pw.write(hdr[:]); err != nil {
		return nil, err
	}
	return pw, nil
}

func (w *Writer) write(b []byte) error {
	n, err := w.w.Write(b)
	w.off += uint64(n)
	return err
}

// Add appends rec unless an object with the same hash is already in this
// pack. It reports whether the record was written.
func (w *Writer) Add(rec Record) (bool, error) {
	if w.done {
		return false, errors.New("pack: writer finished")
	}
	if w.seen[rec.Hash] {
		return false, nil
	}
	if !rec.Type.Valid() || rec.Flags&^FlagZstd != 0 || rec.Size > MaxObjectSize || uint64(len(rec.Data)) > MaxObjectSize {
		return false, fmt.Errorf("pack: invalid record %s %s", rec.Type, rec.Hash.Short())
	}
	e := IndexEntry{Hash: rec.Hash, Type: rec.Type, Flags: rec.Flags, Offset: w.off, Size: rec.Size, Stored: uint32(len(rec.Data))}
	hdr := w.scratch[:RecordHeaderSize]
	putRecordHeader(hdr, e)
	if err := w.write(hdr); err != nil {
		return false, err
	}
	if err := w.write(rec.Data); err != nil {
		return false, err
	}
	w.index = append(w.index, e)
	w.seen[rec.Hash] = true
	return true, nil
}

// Has reports whether the pack being written contains h.
func (w *Writer) Has(h hash.Hash) bool { return w.seen[h] }

// Len returns the number of bytes written so far.
func (w *Writer) Len() uint64 { return w.off }

// Count returns the number of records written.
func (w *Writer) Count() int { return len(w.index) }

// Finish writes the end-of-records marker, the index and the trailer, flushes,
// and returns the index sorted by hash.
func (w *Writer) Finish() ([]IndexEntry, error) {
	if w.done {
		return nil, errors.New("pack: writer finished")
	}
	w.done = true
	if err := w.write([]byte{EndOfRecords}); err != nil {
		return nil, err
	}
	indexOff := w.off
	sort.Slice(w.index, func(i, j int) bool { return hash.Compare(w.index[i].Hash, w.index[j].Hash) < 0 })
	for _, e := range w.index {
		putIndexEntry(w.scratch[:], e)
		if err := w.write(w.scratch[:]); err != nil {
			return nil, err
		}
	}
	var t [12]byte
	binary.LittleEndian.PutUint64(t[0:], indexOff)
	binary.LittleEndian.PutUint32(t[8:], uint32(len(w.index)))
	if err := w.write(t[:]); err != nil {
		return nil, err
	}
	// The checksum covers every byte before it.
	if err := w.w.Flush(); err != nil {
		return nil, err
	}
	var sum [hash.Size]byte
	w.sum.Sum(sum[:0])
	if err := w.write(sum[:]); err != nil {
		return nil, err
	}
	if err := w.write([]byte(TrailerMagic)); err != nil {
		return nil, err
	}
	if err := w.w.Flush(); err != nil {
		return nil, err
	}
	return w.index, nil
}

func putRecordHeader(b []byte, e IndexEntry) {
	b[0] = byte(e.Type)
	b[1] = e.Flags
	copy(b[2:34], e.Hash[:])
	binary.LittleEndian.PutUint32(b[34:], e.Size)
	binary.LittleEndian.PutUint32(b[38:], e.Stored)
}

func parseRecordHeader(b []byte) IndexEntry {
	var e IndexEntry
	e.Type = object.Type(b[0])
	e.Flags = b[1]
	copy(e.Hash[:], b[2:34])
	e.Size = binary.LittleEndian.Uint32(b[34:])
	e.Stored = binary.LittleEndian.Uint32(b[38:])
	return e
}

func putIndexEntry(b []byte, e IndexEntry) {
	copy(b[0:32], e.Hash[:])
	b[32] = byte(e.Type)
	b[33] = e.Flags
	binary.LittleEndian.PutUint64(b[34:], e.Offset)
	binary.LittleEndian.PutUint32(b[42:], e.Size)
	binary.LittleEndian.PutUint32(b[46:], e.Stored)
}

func parseIndexEntry(b []byte) IndexEntry {
	var e IndexEntry
	copy(e.Hash[:], b[0:32])
	e.Type = object.Type(b[32])
	e.Flags = b[33]
	e.Offset = binary.LittleEndian.Uint64(b[34:])
	e.Size = binary.LittleEndian.Uint32(b[42:])
	e.Stored = binary.LittleEndian.Uint32(b[46:])
	return e
}

func (e IndexEntry) check() error {
	if !e.Type.Valid() || e.Flags&^FlagZstd != 0 || e.Size > MaxObjectSize || e.Stored > MaxObjectSize {
		return fmt.Errorf("%w: invalid entry for %s", ErrCorrupt, e.Hash.Short())
	}
	if e.Flags == 0 && e.Stored != e.Size {
		return fmt.Errorf("%w: uncompressed entry %s with stored %d != size %d", ErrCorrupt, e.Hash.Short(), e.Stored, e.Size)
	}
	return nil
}

// Reader reads a pack through random access.
type Reader struct {
	r        io.ReaderAt
	size     int64
	indexOff uint64
	index    []IndexEntry
}

// Open reads and validates the trailer and index of a pack of the given
// size. It does not read the records; use Verify for a full check.
func Open(r io.ReaderAt, size int64) (*Reader, error) {
	if size < HeaderSize+1+TrailerSize {
		return nil, fmt.Errorf("%w: %d bytes is too small", ErrCorrupt, size)
	}
	var hdr [HeaderSize]byte
	if _, err := r.ReadAt(hdr[:], 0); err != nil {
		return nil, fmt.Errorf("pack: read header: %w", err)
	}
	if string(hdr[:4]) != Magic {
		return nil, fmt.Errorf("%w: bad magic", ErrCorrupt)
	}
	if v := binary.LittleEndian.Uint32(hdr[4:]); v != Version {
		return nil, fmt.Errorf("pack: unsupported version %d", v)
	}
	var tr [TrailerSize]byte
	if _, err := r.ReadAt(tr[:], size-TrailerSize); err != nil {
		return nil, fmt.Errorf("pack: read trailer: %w", err)
	}
	if string(tr[44:]) != TrailerMagic {
		return nil, fmt.Errorf("%w: bad trailer magic", ErrCorrupt)
	}
	indexOff := binary.LittleEndian.Uint64(tr[0:])
	count := uint64(binary.LittleEndian.Uint32(tr[8:]))
	indexEnd := uint64(size) - TrailerSize // the index ends where the trailer starts
	if indexOff < HeaderSize+1 || indexOff > indexEnd || (indexEnd-indexOff) != count*IndexEntrySize {
		return nil, fmt.Errorf("%w: index offset %d and count %d do not fit", ErrCorrupt, indexOff, count)
	}
	buf := make([]byte, count*IndexEntrySize)
	if _, err := r.ReadAt(buf, int64(indexOff)); err != nil {
		return nil, fmt.Errorf("pack: read index: %w", err)
	}
	index := make([]IndexEntry, count)
	for i := range index {
		e := parseIndexEntry(buf[i*IndexEntrySize:])
		if err := e.check(); err != nil {
			return nil, err
		}
		if i > 0 && hash.Compare(index[i-1].Hash, e.Hash) >= 0 {
			return nil, fmt.Errorf("%w: index not strictly sorted", ErrCorrupt)
		}
		// The record must lie between the header and the end-of-records byte.
		end := e.Offset + RecordHeaderSize + uint64(e.Stored)
		if e.Offset < HeaderSize || end > indexOff-1 {
			return nil, fmt.Errorf("%w: record %s out of bounds", ErrCorrupt, e.Hash.Short())
		}
		index[i] = e
	}
	return &Reader{r: r, size: size, indexOff: indexOff, index: index}, nil
}

// Entries returns the index sorted by hash. The slice must not be modified.
func (p *Reader) Entries() []IndexEntry { return p.index }

// Lookup returns the index entry for h.
func (p *Reader) Lookup(h hash.Hash) (IndexEntry, bool) {
	i := sort.Search(len(p.index), func(i int) bool { return hash.Compare(p.index[i].Hash, h) >= 0 })
	if i < len(p.index) && p.index[i].Hash == h {
		return p.index[i], true
	}
	return IndexEntry{}, false
}

// Read reads the record for an index entry and checks that its header
// matches the entry. The payload is not verified; use Record.Payload.
func (p *Reader) Read(e IndexEntry) (Record, error) {
	buf := make([]byte, RecordHeaderSize+int(e.Stored))
	if _, err := p.r.ReadAt(buf, int64(e.Offset)); err != nil {
		return Record{}, fmt.Errorf("pack: read record: %w", err)
	}
	got := parseRecordHeader(buf)
	got.Offset = e.Offset
	if got != e {
		return Record{}, fmt.Errorf("%w: record header for %s does not match index", ErrCorrupt, e.Hash.Short())
	}
	return Record{Type: e.Type, Hash: e.Hash, Flags: e.Flags, Size: e.Size, Data: buf[RecordHeaderSize:]}, nil
}

// Get returns the type and verified payload of object h.
func (p *Reader) Get(h hash.Hash) (object.Type, []byte, error) {
	e, ok := p.Lookup(h)
	if !ok {
		return 0, nil, fmt.Errorf("pack: %s not found", h.Short())
	}
	rec, err := p.Read(e)
	if err != nil {
		return 0, nil, err
	}
	payload, err := rec.Payload(nil)
	if err != nil {
		return 0, nil, err
	}
	return e.Type, payload, nil
}

// Verify reads the whole pack and checks the checksum, that the records
// form a contiguous sequence matching the index, and that every payload
// hashes to its record's hash.
func (p *Reader) Verify() error {
	sr := io.NewSectionReader(p.r, 0, p.size)
	sum := blake3.New(hash.Size, nil)
	br := bufio.NewReaderSize(io.TeeReader(io.LimitReader(sr, p.size-TrailerSize+12), sum), 1<<20)
	seen := 0
	end, err := scan(br, func(rec Record, off uint64) error {
		e, ok := p.Lookup(rec.Hash)
		if !ok || e.Offset != off {
			return fmt.Errorf("%w: record %s at %d not in index", ErrCorrupt, rec.Hash.Short(), off)
		}
		if _, err := rec.Payload(nil); err != nil {
			return err
		}
		seen++
		return nil
	})
	if err != nil {
		return err
	}
	if seen != len(p.index) {
		return fmt.Errorf("%w: %d records, index has %d", ErrCorrupt, seen, len(p.index))
	}
	if end+1 != p.indexOff {
		return fmt.Errorf("%w: index does not follow the end-of-records marker", ErrCorrupt)
	}
	// Consume the index and the first 12 trailer bytes into the checksum.
	if _, err := io.Copy(io.Discard, br); err != nil {
		return err
	}
	var want [hash.Size]byte
	if _, err := p.r.ReadAt(want[:], p.size-TrailerSize+12); err != nil {
		return err
	}
	var got [hash.Size]byte
	sum.Sum(got[:0])
	if got != want {
		return fmt.Errorf("%w: checksum mismatch", ErrCorrupt)
	}
	return nil
}

// Scan reads a pack stream sequentially and calls fn for each record, in
// stream order, with its payload still stored (compressed or not). It stops
// at the end-of-records marker and does not read the index or trailer;
// callers that need integrity guarantees verify each Record.Payload.
func Scan(r io.Reader, fn func(Record) error) error {
	_, err := scan(bufio.NewReaderSize(r, 1<<20), func(rec Record, _ uint64) error { return fn(rec) })
	return err
}

// scan returns the offset of the end-of-records marker.
func scan(br *bufio.Reader, fn func(Record, uint64) error) (uint64, error) {
	var hdr [HeaderSize]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return 0, fmt.Errorf("pack: read header: %w", err)
	}
	if string(hdr[:4]) != Magic {
		return 0, fmt.Errorf("%w: bad magic", ErrCorrupt)
	}
	if v := binary.LittleEndian.Uint32(hdr[4:]); v != Version {
		return 0, fmt.Errorf("pack: unsupported version %d", v)
	}
	off := uint64(HeaderSize)
	var rh [RecordHeaderSize]byte
	for {
		t, err := br.ReadByte()
		if err != nil {
			return 0, fmt.Errorf("%w: missing end-of-records marker: %v", ErrCorrupt, err)
		}
		if t == EndOfRecords {
			return off, nil
		}
		rh[0] = t
		if _, err := io.ReadFull(br, rh[1:]); err != nil {
			return 0, fmt.Errorf("%w: truncated record header: %v", ErrCorrupt, err)
		}
		e := parseRecordHeader(rh[:])
		if err := e.check(); err != nil {
			return 0, err
		}
		data := make([]byte, e.Stored)
		if _, err := io.ReadFull(br, data); err != nil {
			return 0, fmt.Errorf("%w: truncated record: %v", ErrCorrupt, err)
		}
		if err := fn(Record{Type: e.Type, Hash: e.Hash, Flags: e.Flags, Size: e.Size, Data: data}, off); err != nil {
			return 0, err
		}
		off += RecordHeaderSize + uint64(e.Stored)
	}
}
