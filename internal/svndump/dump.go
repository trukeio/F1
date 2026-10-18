// Package svndump reads Subversion dump streams, as written by
// "svnadmin dump" and "svnrdump dump", and applies the svndiff deltas they
// carry (ADR-0019). It reads formats 2 (full texts) and 3 (deltas).
//
// A dump is a sequence of records, each a block of "Key: value" headers,
// a blank line, and content: a property block, then the text. Text is
// streamed: Node.Text reads it from the dump, and is valid until the next
// call to Next.
package svndump

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"strconv"
	"strings"
)

// Limits on what one record may hold in memory. Texts are streamed and
// have no limit.
const (
	maxLine  = 1 << 20  // one header line
	maxProps = 64 << 20 // one property block
)

// Record is a *Revision or a *Node.
type Record interface{ record() }

// Revision starts a revision: the nodes that follow, up to the next
// Revision, are its changes.
type Revision struct {
	Number int
	Props  map[string]string // revision properties: svn:log, svn:author, svn:date
}

// Node actions.
const (
	ActionAdd     = "add"
	ActionDelete  = "delete"
	ActionChange  = "change"
	ActionReplace = "replace"
)

// Node kinds. A delete may have no kind.
const (
	KindFile = "file"
	KindDir  = "dir"
)

// Node is one change to one path.
type Node struct {
	Path   string // without a leading "/"
	Kind   string // KindFile, KindDir or ""
	Action string

	// CopyFromPath and CopyFromRev name the copy source; CopyFromRev is -1
	// if the node is not a copy.
	CopyFromPath string
	CopyFromRev  int

	// Props is nil if the node has no property block. Otherwise it holds
	// the node's properties; with PropDelta, only the changed ones, and
	// DeletedProps lists those removed.
	Props        map[string]string
	PropDelta    bool
	DeletedProps []string

	// HasText says whether the node has a text. Text reads it: the full
	// text, or with TextDelta an svndiff delta against the node's previous
	// text (the copy source's for a copy). TextLength is the length of
	// what Text reads.
	HasText    bool
	TextDelta  bool
	TextLength int64
	Text       io.Reader

	// Checksums, as hexadecimal, of the delta base and of the result. Any
	// of them may be empty.
	TextDeltaBaseMD5, TextDeltaBaseSHA1 string
	TextContentMD5, TextContentSHA1     string

	// Headers holds every header of the record, including those above.
	Headers map[string]string
}

func (*Revision) record() {}
func (*Node) record()     {}

// Reader reads the records of a dump.
type Reader struct {
	Version int    // the dump format version: 2 or 3
	UUID    string // the repository UUID, once its record has been read

	br   *bufio.Reader
	skip int64 // bytes of the current record's content not read yet
	text *io.LimitedReader
}

// NewReader reads the dump's format header.
func NewReader(r io.Reader) (*Reader, error) {
	d := &Reader{br: bufio.NewReaderSize(r, 64<<10)}
	h, err := d.headers()
	if err == io.EOF {
		return nil, errors.New("svndump: empty dump")
	}
	if err != nil {
		return nil, err
	}
	v, ok := h["SVN-fs-dump-format-version"]
	if !ok {
		return nil, errors.New("svndump: not a dump: no SVN-fs-dump-format-version header")
	}
	switch v {
	case "2", "3":
		d.Version = int(v[0] - '0')
	default:
		return nil, fmt.Errorf("svndump: dump format version %s is not supported (2 and 3 are)", v)
	}
	return d, nil
}

// Next returns the next record, or io.EOF at the end of the dump. It
// skips whatever the caller did not read of the previous node's text.
func (d *Reader) Next() (Record, error) {
	if err := d.discard(); err != nil {
		return nil, err
	}
	for {
		h, err := d.headers()
		if err != nil {
			return nil, err
		}
		switch {
		case hasKey(h, "Revision-number"):
			return d.revision(h)
		case hasKey(h, "Node-path"):
			return d.node(h)
		case hasKey(h, "UUID"):
			d.UUID = h["UUID"]
		default:
			return nil, fmt.Errorf("svndump: unknown record with headers %v", keys(h))
		}
	}
}

func (d *Reader) revision(h map[string]string) (*Revision, error) {
	n, err := number(h, "Revision-number")
	if err != nil {
		return nil, err
	}
	propLen, textLen, err := d.lengths(h)
	if err != nil {
		return nil, err
	}
	if textLen > 0 {
		return nil, fmt.Errorf("svndump: revision %d has a text", n)
	}
	rev := &Revision{Number: n, Props: map[string]string{}}
	if propLen > 0 {
		if rev.Props, _, err = d.props(propLen); err != nil {
			return nil, fmt.Errorf("svndump: revision %d: %w", n, err)
		}
	}
	return rev, nil
}

func (d *Reader) node(h map[string]string) (*Node, error) {
	n := &Node{
		Path:              strings.TrimPrefix(h["Node-path"], "/"),
		Kind:              h["Node-kind"],
		Action:            h["Node-action"],
		CopyFromPath:      strings.TrimPrefix(h["Node-copyfrom-path"], "/"),
		CopyFromRev:       -1,
		PropDelta:         h["Prop-delta"] == "true",
		TextDelta:         h["Text-delta"] == "true",
		TextDeltaBaseMD5:  h["Text-delta-base-md5"],
		TextDeltaBaseSHA1: h["Text-delta-base-sha1"],
		TextContentMD5:    h["Text-content-md5"],
		TextContentSHA1:   h["Text-content-sha1"],
		Headers:           h,
	}
	fail := func(format string, a ...any) error {
		return fmt.Errorf("svndump: node %q: %s", n.Path, fmt.Sprintf(format, a...))
	}
	switch n.Action {
	case ActionAdd, ActionDelete, ActionChange, ActionReplace:
	default:
		return nil, fail("unknown action %q", n.Action)
	}
	switch n.Kind {
	case KindFile, KindDir:
	case "":
		if n.Action != ActionDelete {
			return nil, fail("no kind")
		}
	default:
		return nil, fail("unknown kind %q", n.Kind)
	}
	if hasKey(h, "Node-copyfrom-rev") {
		r, err := number(h, "Node-copyfrom-rev")
		if err != nil {
			return nil, fail("%v", err)
		}
		if !hasKey(h, "Node-copyfrom-path") {
			return nil, fail("Node-copyfrom-rev without Node-copyfrom-path")
		}
		n.CopyFromRev = r
	}
	propLen, textLen, err := d.lengths(h)
	if err != nil {
		return nil, fail("%v", err)
	}
	if hasKey(h, "Prop-content-length") {
		if n.Props, n.DeletedProps, err = d.props(propLen); err != nil {
			return nil, fail("%v", err)
		}
	}
	if hasKey(h, "Text-content-length") {
		if n.Kind == KindDir {
			return nil, fail("a directory has a text")
		}
		n.HasText = true
		n.TextLength = textLen
		d.text = &io.LimitedReader{R: d.br, N: textLen}
		d.skip -= textLen
		n.Text = d.text
	}
	return n, nil
}

// lengths returns the property and text lengths of a record, and sets
// d.skip to the content length that follows the headers.
func (d *Reader) lengths(h map[string]string) (propLen, textLen int64, err error) {
	get := func(k string) (int64, error) {
		v, ok := h[k]
		if !ok {
			return 0, nil
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("invalid %s %q", k, v)
		}
		return n, nil
	}
	if propLen, err = get("Prop-content-length"); err != nil {
		return 0, 0, err
	}
	if textLen, err = get("Text-content-length"); err != nil {
		return 0, 0, err
	}
	if propLen > maxProps {
		return 0, 0, fmt.Errorf("property block of %d bytes is too large", propLen)
	}
	content := propLen + textLen
	if hasKey(h, "Content-length") {
		if content, err = get("Content-length"); err != nil {
			return 0, 0, err
		}
		if content < propLen+textLen || propLen+textLen < 0 {
			return 0, 0, fmt.Errorf("the content length %d is less than the property and text lengths", content)
		}
	}
	d.skip = content
	return propLen, textLen, nil
}

// props reads a property block of n bytes: "K <len>\n<key>\nV <len>\n<value>\n"
// pairs and, in deltas, "D <len>\n<key>\n" deletions, ending in PROPS-END.
func (d *Reader) props(n int64) (map[string]string, []string, error) {
	// Grow with the data read, not with the length the dump claims.
	var bb bytes.Buffer
	if _, err := io.CopyN(&bb, d.br, n); err != nil {
		return nil, nil, fmt.Errorf("property block: %w", unexpected(err))
	}
	buf := bb.Bytes()
	d.skip -= n
	props := map[string]string{}
	var deleted []string
	field := func(tag byte) (string, bool) {
		line, rest, ok := bytes.Cut(buf, []byte("\n"))
		if !ok || len(line) < 3 || line[0] != tag || line[1] != ' ' {
			return "", false
		}
		l, err := strconv.Atoi(string(line[2:]))
		if err != nil || l < 0 || l >= len(rest) || rest[l] != '\n' {
			return "", false
		}
		buf = rest[l+1:]
		return string(rest[:l]), true
	}
	for {
		if bytes.HasPrefix(buf, []byte("PROPS-END\n")) {
			return props, deleted, nil
		}
		if len(buf) == 0 {
			return nil, nil, errors.New("property block: no PROPS-END")
		}
		switch buf[0] {
		case 'K':
			k, ok := field('K')
			if !ok {
				return nil, nil, errors.New("property block: invalid key")
			}
			v, ok := field('V')
			if !ok {
				return nil, nil, fmt.Errorf("property block: invalid value of %q", k)
			}
			props[k] = v
		case 'D':
			k, ok := field('D')
			if !ok {
				return nil, nil, errors.New("property block: invalid deletion")
			}
			deleted = append(deleted, k)
		default:
			return nil, nil, fmt.Errorf("property block: unexpected %q", buf[:min(len(buf), 16)])
		}
	}
}

// discard skips the rest of the current record's content.
func (d *Reader) discard() error {
	n := d.skip
	if d.text != nil {
		n += d.text.N
		d.text.N = 0
		d.text = nil
	}
	d.skip = 0
	if n > 0 {
		if _, err := io.CopyN(io.Discard, d.br, n); err != nil {
			return fmt.Errorf("svndump: %w", unexpected(err))
		}
	}
	return nil
}

// headers reads one header block, skipping blank lines before it. It
// returns io.EOF if the dump ends before a header.
func (d *Reader) headers() (map[string]string, error) {
	h := map[string]string{}
	for {
		line, err := d.line()
		if err == io.EOF && len(h) == 0 {
			return nil, io.EOF
		}
		if err != nil {
			return nil, fmt.Errorf("svndump: %w", unexpected(err))
		}
		if line == "" {
			if len(h) == 0 {
				continue // a blank line between records
			}
			return h, nil
		}
		k, v, ok := strings.Cut(line, ": ")
		if !ok {
			if !strings.HasSuffix(line, ":") {
				return nil, fmt.Errorf("svndump: invalid header line %q", truncate(line))
			}
			k = line[:len(line)-1] // an empty value
		}
		if _, dup := h[k]; dup {
			return nil, fmt.Errorf("svndump: header %s appears twice", k)
		}
		h[k] = v
	}
}

// line reads one line without its newline. It returns io.EOF only at the
// end of the dump, and io.ErrUnexpectedEOF for a last line without a
// newline.
func (d *Reader) line() (string, error) {
	var b []byte
	for {
		chunk, err := d.br.ReadSlice('\n')
		b = append(b, chunk...)
		if len(b) > maxLine {
			return "", errors.New("header line too long")
		}
		switch {
		case err == bufio.ErrBufferFull:
			continue
		case err == io.EOF && len(b) > 0:
			return "", io.ErrUnexpectedEOF
		case err != nil:
			return "", err
		}
		return string(b[:len(b)-1]), nil
	}
}

func number(h map[string]string, k string) (int, error) {
	n, err := strconv.Atoi(h[k])
	if err != nil || n < 0 {
		return 0, fmt.Errorf("svndump: invalid %s %q", k, h[k])
	}
	return n, nil
}

func hasKey(h map[string]string, k string) bool {
	_, ok := h[k]
	return ok
}

func keys(h map[string]string) []string {
	var ks []string
	for k := range h {
		ks = append(ks, k)
	}
	return ks
}

func truncate(s string) string {
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}

func unexpected(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}

// Checker verifies a text against the MD5 and SHA-1 checksums of a dump:
// write the text to it, then call Check. An empty checksum is not
// checked.
type Checker struct {
	wantMD5, wantSHA1 string
	md5, sha1         hash.Hash
}

// NewChecker returns a Checker for the given hexadecimal checksums.
func NewChecker(md5Hex, sha1Hex string) *Checker {
	return &Checker{wantMD5: md5Hex, wantSHA1: sha1Hex, md5: md5.New(), sha1: sha1.New()}
}

func (c *Checker) Write(p []byte) (int, error) {
	c.md5.Write(p)
	c.sha1.Write(p)
	return len(p), nil
}

// Check reports whether the text written matches the checksums.
func (c *Checker) Check() error {
	if c.wantMD5 != "" {
		if got := hex.EncodeToString(c.md5.Sum(nil)); got != c.wantMD5 {
			return fmt.Errorf("svndump: MD5 is %s, the dump says %s", got, c.wantMD5)
		}
	}
	if c.wantSHA1 != "" {
		if got := hex.EncodeToString(c.sha1.Sum(nil)); got != c.wantSHA1 {
			return fmt.Errorf("svndump: SHA-1 is %s, the dump says %s", got, c.wantSHA1)
		}
	}
	return nil
}
