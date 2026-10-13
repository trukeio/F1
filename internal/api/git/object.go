// Package git implements Git smart HTTP on F1 namespaces (doc/plan.md,
// "Git compatibility"; ADR-0008): the mapping between F1 objects and Git
// objects (export for fetch, import for push and the test importer), pack
// generation, and the upload-pack protocol (v2 and v0/v1).
package git

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"

	"github.com/trukeio/f1/internal/object"
)

// SHA is a Git object id (SHA-1).
type SHA [20]byte

func (s SHA) String() string { return hex.EncodeToString(s[:]) }

// IsZero reports whether s is the zero id.
func (s SHA) IsZero() bool { return s == SHA{} }

// ParseSHA parses a 40-digit hex object id.
func ParseSHA(v string) (SHA, error) {
	var s SHA
	if len(v) != 40 {
		return s, fmt.Errorf("git: invalid object id %q", v)
	}
	if _, err := hex.Decode(s[:], []byte(v)); err != nil {
		return s, fmt.Errorf("git: invalid object id %q", v)
	}
	return s, nil
}

// Type is a Git object type, with the pack type codes.
type Type int

// Git object types.
const (
	TypeCommit Type = 1
	TypeTree   Type = 2
	TypeBlob   Type = 3
	TypeTag    Type = 4
)

var typeNames = map[Type]string{TypeCommit: "commit", TypeTree: "tree", TypeBlob: "blob", TypeTag: "tag"}

func (t Type) String() string { return typeNames[t] }

// TypeByName returns the type for a Git type name.
func TypeByName(name string) (Type, bool) {
	for t, n := range typeNames {
		if n == name {
			return t, true
		}
	}
	return 0, false
}

// Header returns the object header "<type> <size>\x00" hashed before the
// content.
func Header(t Type, size int64) []byte {
	return []byte(t.String() + " " + strconv.FormatInt(size, 10) + "\x00")
}

// Hash returns the object id of content of type t.
func Hash(t Type, data []byte) SHA {
	h := sha1.New()
	h.Write(Header(t, int64(len(data))))
	h.Write(data)
	var s SHA
	copy(s[:], h.Sum(nil))
	return s
}

// Git modes of tree entries, as written in canonical tree objects.
const (
	ModeFile    = "100644"
	ModeExec    = "100755"
	ModeSymlink = "120000"
	ModeDir     = "40000"
	ModeGitlink = "160000"
)

// TreeEntry is an entry of a Git tree object.
type TreeEntry struct {
	Mode string
	Name []byte
	SHA  SHA
}

// IsDir reports whether the entry is a subdirectory (mode 40000, or a
// zero-padded variant).
func (e *TreeEntry) IsDir() bool {
	m, err := strconv.ParseUint(e.Mode, 8, 32)
	return err == nil && m&0o170000 == 0o040000
}

// sortKey is the Git tree order: directories compare as "name/".
func (e *TreeEntry) sortKey() []byte {
	if e.IsDir() {
		return append(append([]byte{}, e.Name...), '/')
	}
	return e.Name
}

// SortTree sorts entries in Git tree order.
func SortTree(entries []TreeEntry) {
	sort.Slice(entries, func(i, j int) bool { return bytes.Compare(entries[i].sortKey(), entries[j].sortKey()) < 0 })
}

// EncodeTree encodes entries, which must be in Git tree order.
func EncodeTree(entries []TreeEntry) []byte {
	var b bytes.Buffer
	for _, e := range entries {
		b.WriteString(e.Mode)
		b.WriteByte(' ')
		b.Write(e.Name)
		b.WriteByte(0)
		b.Write(e.SHA[:])
	}
	return b.Bytes()
}

// ParseTree parses a tree object.
func ParseTree(data []byte) ([]TreeEntry, error) {
	var out []TreeEntry
	for len(data) > 0 {
		sp := bytes.IndexByte(data, ' ')
		if sp <= 0 {
			return nil, errors.New("git: malformed tree entry mode")
		}
		mode := string(data[:sp])
		if _, err := strconv.ParseUint(mode, 8, 32); err != nil {
			return nil, fmt.Errorf("git: malformed tree entry mode %q", mode)
		}
		data = data[sp+1:]
		nul := bytes.IndexByte(data, 0)
		if nul <= 0 || len(data) < nul+1+20 {
			return nil, errors.New("git: malformed tree entry name")
		}
		e := TreeEntry{Mode: mode, Name: append([]byte{}, data[:nul]...)}
		copy(e.SHA[:], data[nul+1:nul+21])
		out = append(out, e)
		data = data[nul+21:]
	}
	return out, nil
}

// KindForMode maps a Git mode to the nearest F1 entry kind; canonical
// reports whether the mode is written as Git writes it (non-canonical
// modes such as 100664 or 040000 need the raw tree bytes to round-trip).
func KindForMode(mode string) (kind object.Kind, canonical bool, err error) {
	m, err := strconv.ParseUint(mode, 8, 32)
	if err != nil {
		return 0, false, fmt.Errorf("git: invalid mode %q", mode)
	}
	switch m & 0o170000 {
	case 0o040000:
		return object.KindDir, mode == ModeDir, nil
	case 0o120000:
		return object.KindSymlink, mode == ModeSymlink, nil
	case 0o160000:
		return object.KindGitlink, mode == ModeGitlink, nil
	case 0o100000:
		if m&0o111 != 0 {
			return object.KindExec, mode == ModeExec, nil
		}
		return object.KindFile, mode == ModeFile, nil
	}
	return 0, false, fmt.Errorf("git: unsupported mode %q", mode)
}

// ModeForKind returns the canonical Git mode of an F1 entry kind; LFS
// entries use their exec attribute.
func ModeForKind(e *object.Entry) string {
	switch e.Kind {
	case object.KindExec:
		return ModeExec
	case object.KindSymlink:
		return ModeSymlink
	case object.KindDir:
		return ModeDir
	case object.KindGitlink:
		return ModeGitlink
	case object.KindLFS:
		if e.Attrs != nil && e.Attrs.Exec {
			return ModeExec
		}
	}
	return ModeFile
}

// --- commits and tags ---

// header is a header line of a commit or tag; continuation lines of a
// multi-line value are joined with "\n".
type header struct {
	key, value []byte
}

// parseHeaders splits a commit or tag object into headers and message.
func parseHeaders(data []byte) ([]header, []byte, error) {
	var hs []header
	for {
		if len(data) == 0 {
			return nil, nil, errors.New("git: object has no message separator")
		}
		if data[0] == '\n' {
			return hs, data[1:], nil
		}
		nl := bytes.IndexByte(data, '\n')
		if nl < 0 {
			return nil, nil, errors.New("git: truncated header")
		}
		line := data[:nl]
		data = data[nl+1:]
		if line[0] == ' ' {
			if len(hs) == 0 {
				return nil, nil, errors.New("git: continuation line without header")
			}
			last := &hs[len(hs)-1]
			last.value = append(append(last.value, '\n'), line[1:]...)
			continue
		}
		k, v, ok := bytes.Cut(line, []byte{' '})
		if !ok {
			return nil, nil, fmt.Errorf("git: malformed header %q", line)
		}
		hs = append(hs, header{key: append([]byte{}, k...), value: append([]byte{}, v...)})
	}
}

func writeHeader(b *bytes.Buffer, key, value []byte) {
	b.Write(key)
	b.WriteByte(' ')
	b.Write(bytes.ReplaceAll(value, []byte("\n"), []byte("\n ")))
	b.WriteByte('\n')
}

// ParseIdent parses "Name <email> time tz".
func ParseIdent(v []byte) (object.Person, error) {
	lt := bytes.IndexByte(v, '<')
	gt := bytes.LastIndexByte(v, '>')
	if lt < 0 || gt < lt {
		return object.Person{}, fmt.Errorf("git: malformed ident %q", v)
	}
	p := object.Person{Name: bytes.TrimSuffix(append([]byte{}, v[:lt]...), []byte(" ")), Email: append([]byte{}, v[lt+1:gt]...)}
	rest := bytes.TrimPrefix(v[gt+1:], []byte(" "))
	ts, tz, ok := bytes.Cut(rest, []byte(" "))
	if !ok {
		return object.Person{}, fmt.Errorf("git: ident without time zone %q", v)
	}
	t, err := strconv.ParseInt(string(ts), 10, 64)
	if err != nil {
		return object.Person{}, fmt.Errorf("git: malformed ident time %q", v)
	}
	p.Time, p.TZ = t, string(tz)
	return p, nil
}

// FormatIdent formats a person as Git writes it.
func FormatIdent(p *object.Person) []byte {
	var b bytes.Buffer
	b.Write(p.Name)
	b.WriteString(" <")
	b.Write(p.Email)
	b.WriteString("> ")
	b.WriteString(strconv.FormatInt(p.Time, 10))
	b.WriteByte(' ')
	b.WriteString(p.TZ)
	return b.Bytes()
}

// EncodeCommit encodes a commit with the given Git ids of its tree and
// parents, in the order Git writes headers: tree, parents, author,
// committer, extra headers.
func EncodeCommit(c *object.Commit, tree SHA, parents []SHA) []byte {
	var b bytes.Buffer
	writeHeader(&b, []byte("tree"), []byte(tree.String()))
	for _, p := range parents {
		writeHeader(&b, []byte("parent"), []byte(p.String()))
	}
	writeHeader(&b, []byte("author"), FormatIdent(&c.Author))
	writeHeader(&b, []byte("committer"), FormatIdent(&c.Committer))
	for _, h := range c.GitExtra {
		writeHeader(&b, h.Key, h.Value)
	}
	b.WriteByte('\n')
	b.Write(c.Message)
	return b.Bytes()
}

// ParsedCommit is a Git commit with the Git ids it references.
type ParsedCommit struct {
	Tree    SHA
	Parents []SHA
	Commit  object.Commit // without Tree and Parents (F1 hashes)
}

// ParseCommit parses a commit object. Fields it cannot model make the
// re-encoding differ; the importer detects that and keeps the raw bytes.
func ParseCommit(data []byte) (*ParsedCommit, error) {
	hs, msg, err := parseHeaders(data)
	if err != nil {
		return nil, err
	}
	pc := &ParsedCommit{}
	pc.Commit.Message = append([]byte{}, msg...)
	seen := map[string]bool{}
	for _, h := range hs {
		switch k := string(h.key); {
		case k == "tree" && !seen[k]:
			if pc.Tree, err = ParseSHA(string(h.value)); err != nil {
				return nil, err
			}
		case k == "parent":
			p, err := ParseSHA(string(h.value))
			if err != nil {
				return nil, err
			}
			pc.Parents = append(pc.Parents, p)
		case (k == "author" || k == "committer") && !seen[k]:
			p, err := ParseIdent(h.value)
			if err != nil {
				// Keep it as an extra header; the commit round-trips through
				// its raw bytes.
				pc.Commit.GitExtra = append(pc.Commit.GitExtra, object.ExtraHeader{Key: h.key, Value: h.value})
				continue
			}
			if k == "author" {
				pc.Commit.Author = p
			} else {
				pc.Commit.Committer = p
			}
		default:
			pc.Commit.GitExtra = append(pc.Commit.GitExtra, object.ExtraHeader{Key: h.key, Value: h.value})
		}
		seen[string(h.key)] = true
	}
	if pc.Tree.IsZero() {
		return nil, errors.New("git: commit without tree")
	}
	return pc, nil
}

// EncodeTag encodes a tag with the Git id of its target.
func EncodeTag(t *object.Tag, target SHA) []byte {
	var b bytes.Buffer
	writeHeader(&b, []byte("object"), []byte(target.String()))
	writeHeader(&b, []byte("type"), []byte(t.TargetType))
	writeHeader(&b, []byte("tag"), t.Name)
	if t.Tagger != nil {
		writeHeader(&b, []byte("tagger"), FormatIdent(t.Tagger))
	}
	for _, h := range t.GitExtra {
		writeHeader(&b, h.Key, h.Value)
	}
	b.WriteByte('\n')
	b.Write(t.Message)
	return b.Bytes()
}

// ParsedTag is a Git tag with the Git id of its target.
type ParsedTag struct {
	Object SHA
	Tag    object.Tag // without Object (an F1 hash)
}

// ParseTag parses a tag object.
func ParseTag(data []byte) (*ParsedTag, error) {
	hs, msg, err := parseHeaders(data)
	if err != nil {
		return nil, err
	}
	pt := &ParsedTag{}
	pt.Tag.Message = append([]byte{}, msg...)
	seen := map[string]bool{}
	for _, h := range hs {
		switch k := string(h.key); {
		case k == "object" && !seen[k]:
			if pt.Object, err = ParseSHA(string(h.value)); err != nil {
				return nil, err
			}
		case k == "type" && !seen[k]:
			pt.Tag.TargetType = string(h.value)
		case k == "tag" && !seen[k]:
			pt.Tag.Name = h.value
		case k == "tagger" && !seen[k]:
			p, err := ParseIdent(h.value)
			if err != nil {
				pt.Tag.GitExtra = append(pt.Tag.GitExtra, object.ExtraHeader{Key: h.key, Value: h.value})
				continue
			}
			pt.Tag.Tagger = &p
		default:
			pt.Tag.GitExtra = append(pt.Tag.GitExtra, object.ExtraHeader{Key: h.key, Value: h.value})
		}
		seen[string(h.key)] = true
	}
	if pt.Object.IsZero() || pt.Tag.TargetType == "" || len(pt.Tag.Name) == 0 {
		return nil, errors.New("git: tag without object, type or name")
	}
	return pt, nil
}
