package repodir

import (
	"bytes"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// Lock is a file lock. The locks file holds one per line, fields escaped as
// in escape():
//
//	<id> <ref> <path> <owner> <created> <expires>
//
// Times are RFC 3339 UTC with nanoseconds; expires is "-" for no expiry.
type Lock struct {
	ID      string
	Ref     string
	Path    string
	Owner   string // issuer:subject
	Created time.Time
	Expires time.Time // zero: no expiry
}

// ReadLocks reads the locks file.
func (d *Dir) ReadLocks() ([]Lock, error) {
	data, err := os.ReadFile(d.Join(LocksFile))
	if err != nil {
		return nil, fmt.Errorf("repodir: %w", err)
	}
	var locks []Lock
	for n, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		l, err := parseLock(line)
		if err != nil {
			return nil, fmt.Errorf("repodir: locks line %d: %w", n+1, err)
		}
		locks = append(locks, l)
	}
	return locks, nil
}

func parseLock(line string) (Lock, error) {
	f := strings.Split(line, " ")
	if len(f) != 6 {
		return Lock{}, fmt.Errorf("malformed line %q", line)
	}
	var vals [6]string
	for i, s := range f {
		v, err := unescape(s)
		if err != nil {
			return Lock{}, err
		}
		vals[i] = v
	}
	l := Lock{ID: vals[0], Ref: vals[1], Path: vals[2], Owner: vals[3]}
	var err error
	if l.Created, err = time.Parse(time.RFC3339Nano, vals[4]); err != nil {
		return Lock{}, err
	}
	if vals[5] != "" {
		if l.Expires, err = time.Parse(time.RFC3339Nano, vals[5]); err != nil {
			return Lock{}, err
		}
	}
	if l.ID == "" || l.Path == "" {
		return Lock{}, fmt.Errorf("lock without id or path")
	}
	return l, nil
}

// WriteLocks replaces the locks file atomically, sorted by ref and path.
func (d *Dir) WriteLocks(locks []Lock) error {
	sorted := append([]Lock(nil), locks...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Ref != sorted[j].Ref {
			return sorted[i].Ref < sorted[j].Ref
		}
		return sorted[i].Path < sorted[j].Path
	})
	var buf bytes.Buffer
	for _, l := range sorted {
		expires := ""
		if !l.Expires.IsZero() {
			expires = l.Expires.UTC().Format(time.RFC3339Nano)
		}
		fmt.Fprintf(&buf, "%s %s %s %s %s %s\n", escape(l.ID), escape(l.Ref), escape(l.Path), escape(l.Owner),
			escape(l.Created.UTC().Format(time.RFC3339Nano)), escape(expires))
	}
	return writeAtomic(d.path, LocksFile, buf.Bytes())
}
