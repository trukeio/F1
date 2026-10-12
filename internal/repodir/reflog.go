package repodir

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/policy"
)

// LogRecord is one line of refs.log: one ref update within an update group
// (a multi-ref update is one group). Line format, fields separated by one
// space and escaped as in escape():
//
//	<time> <tx> <i>/<n> <principal> <refname> <old> <new> <reason>
//
// time is RFC 3339 UTC with nanoseconds; tx identifies the group; i counts
// from 1 to n, the number of lines in the group; old and new are hex hashes,
// all zeros for a ref that did not exist or is deleted.
type LogRecord struct {
	Time      time.Time
	Tx        string
	Principal string
	Ref       string
	Old, New  hash.Hash
	Reason    string
}

func formatGroup(recs []LogRecord) []byte {
	var buf bytes.Buffer
	for i, r := range recs {
		fmt.Fprintf(&buf, "%s %s %d/%d %s %s %s %s %s\n",
			r.Time.UTC().Format(time.RFC3339Nano), escape(r.Tx), i+1, len(recs),
			escape(r.Principal), r.Ref, r.Old, r.New, escape(r.Reason))
	}
	return buf.Bytes()
}

type logLine struct {
	LogRecord
	i, n int
}

func parseLogLine(line string) (logLine, error) {
	f := strings.Split(line, " ")
	if len(f) != 8 {
		return logLine{}, fmt.Errorf("malformed refs.log line %q", line)
	}
	var l logLine
	var err error
	if l.Time, err = time.Parse(time.RFC3339Nano, f[0]); err != nil {
		return logLine{}, err
	}
	if l.Tx, err = unescape(f[1]); err != nil {
		return logLine{}, err
	}
	is, ns, ok := strings.Cut(f[2], "/")
	if !ok {
		return logLine{}, fmt.Errorf("malformed group counter %q", f[2])
	}
	if l.i, err = strconv.Atoi(is); err != nil {
		return logLine{}, err
	}
	if l.n, err = strconv.Atoi(ns); err != nil {
		return logLine{}, err
	}
	if l.i < 1 || l.i > l.n {
		return logLine{}, fmt.Errorf("bad group counter %q", f[2])
	}
	if l.Principal, err = unescape(f[3]); err != nil {
		return logLine{}, err
	}
	l.Ref = f[4]
	if err := policy.ValidRefName(l.Ref); err != nil {
		return logLine{}, err
	}
	if l.Old, err = hash.Parse(f[5]); err != nil {
		return logLine{}, err
	}
	if l.New, err = hash.Parse(f[6]); err != nil {
		return logLine{}, err
	}
	if l.Reason, err = unescape(f[7]); err != nil {
		return logLine{}, err
	}
	return l, nil
}

// AppendLog appends one update group to refs.log in a single write and
// fsyncs it. A group is complete in the log exactly when all its lines are.
func (d *Dir) AppendLog(recs []LogRecord) error {
	if len(recs) == 0 {
		return nil
	}
	f, err := os.OpenFile(d.Join(LogFile), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("repodir: %w", err)
	}
	if _, err := f.Write(formatGroup(recs)); err != nil {
		f.Close()
		return fmt.Errorf("repodir: append refs.log: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("repodir: sync refs.log: %w", err)
	}
	return f.Close()
}

// ReadLog calls fn for every record in refs.log, oldest first. It reads
// the whole file; use it for audit and fsck, not on hot paths.
func (d *Dir) ReadLog(fn func(LogRecord) error) error {
	data, err := os.ReadFile(d.Join(LogFile))
	if err != nil {
		return fmt.Errorf("repodir: %w", err)
	}
	for n, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		l, err := parseLogLine(line)
		if err != nil {
			return fmt.Errorf("repodir: refs.log line %d: %w", n+1, err)
		}
		if err := fn(l.LogRecord); err != nil {
			return err
		}
	}
	return nil
}

// Recovery reports what Recover did.
type Recovery struct {
	TruncatedBytes int64  // bytes of an incomplete trailing group removed from refs.log
	RolledForward  int    // refs updated from the last logged group
	Warning        string // inconsistency that recovery could not resolve
}

// Recover makes refs and refs.log consistent after a crash. A ref update
// appends its group to refs.log (fsynced) before replacing refs, so after a
// crash:
//
//   - an incomplete trailing group (torn write) was never applied; it is
//     removed from refs.log;
//   - if refs still holds the old values of the last complete group, the
//     crash happened between the two steps; the group is applied (refs.log
//     is the commit point, and every object it references was made durable
//     before the log was written).
func (d *Dir) Recover() (Recovery, error) {
	var rec Recovery
	tail, err := d.readLastGroup()
	if err != nil {
		return rec, err
	}
	if tail.truncateAt >= 0 {
		if err := truncateFile(d.Join(LogFile), tail.truncateAt); err != nil {
			return rec, err
		}
		rec.TruncatedBytes = tail.size - tail.truncateAt
	}
	if len(tail.group) == 0 {
		return rec, nil
	}
	refs, err := d.ReadRefs()
	if err != nil {
		return rec, err
	}
	allOld, allNew := true, true
	for _, l := range tail.group {
		cur := refs[l.Ref]
		allOld = allOld && cur == l.Old
		allNew = allNew && cur == l.New
	}
	switch {
	case allNew:
	case allOld:
		for _, l := range tail.group {
			if l.New.IsZero() {
				delete(refs, l.Ref)
			} else {
				refs[l.Ref] = l.New
			}
		}
		if err := d.WriteRefs(refs); err != nil {
			return rec, err
		}
		rec.RolledForward = len(tail.group)
	default:
		rec.Warning = fmt.Sprintf("refs do not match the last refs.log group %s; refs kept as the source of truth", tail.group[0].Tx)
	}
	return rec, nil
}

type logTail struct {
	group      []logLine // last complete group
	truncateAt int64     // -1, or the offset where an incomplete tail starts
	size       int64
}

// readLastGroup reads refs.log backwards in growing windows until it holds
// the last complete group, so opening a repository does not read its whole
// history.
func (d *Dir) readLastGroup() (logTail, error) {
	f, err := os.Open(d.Join(LogFile))
	if errors.Is(err, os.ErrNotExist) {
		return logTail{truncateAt: -1}, nil
	}
	if err != nil {
		return logTail{}, fmt.Errorf("repodir: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return logTail{}, err
	}
	size := st.Size()
	for window := int64(64 << 10); ; window *= 4 {
		start := max(size-window, 0)
		buf := make([]byte, size-start)
		if _, err := f.ReadAt(buf, start); err != nil && !errors.Is(err, io.EOF) {
			return logTail{}, err
		}
		tail, ok, err := parseTail(buf, start, size)
		if err != nil {
			return logTail{}, fmt.Errorf("repodir: refs.log: %w", err)
		}
		if ok || start == 0 {
			return tail, nil
		}
	}
}

// parseTail finds the last complete group in buf, which holds the file from
// offset start to size. ok is false when the window may cut the group.
func parseTail(buf []byte, start, size int64) (logTail, bool, error) {
	t := logTail{truncateAt: -1, size: size}
	// A trailing partial line is a torn write.
	end := int64(len(buf))
	if end > 0 && buf[end-1] != '\n' {
		nl := bytes.LastIndexByte(buf, '\n')
		if nl < 0 && start > 0 {
			return t, false, nil
		}
		end = int64(nl + 1)
		t.truncateAt = start + end
	}
	// Skip a partial first line when the window starts mid-file.
	off := int64(0)
	if start > 0 {
		nl := bytes.IndexByte(buf[:end], '\n')
		if nl < 0 {
			return t, false, nil
		}
		off = int64(nl + 1)
	}
	var lines []logLine
	var offsets []int64
	for pos := off; pos < end; {
		nl := int64(bytes.IndexByte(buf[pos:end], '\n'))
		l, err := parseLogLine(string(buf[pos : pos+nl]))
		if err != nil {
			return t, false, err
		}
		lines = append(lines, l)
		offsets = append(offsets, start+pos)
		pos += nl + 1
	}
	if len(lines) == 0 {
		return t, start == 0, nil
	}
	// The last group: lines sharing the last line's tx. If it has fewer than n
	// lines, it is an incomplete torn group: truncate it and look at the one
	// before.
	for len(lines) > 0 {
		last := lines[len(lines)-1]
		first := len(lines) - 1
		for first > 0 && lines[first-1].Tx == last.Tx && lines[first].i > 1 {
			first--
		}
		g := lines[first:]
		if lines[first].i != 1 && first == 0 {
			// The group starts before the window.
			return t, false, nil
		}
		if len(g) == last.n && last.i == last.n {
			t.group = g
			return t, true, nil
		}
		t.truncateAt = offsets[first]
		lines, offsets = lines[:first], offsets[:first]
	}
	return t, start == 0, nil
}

func truncateFile(path string, size int64) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("repodir: %w", err)
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		return fmt.Errorf("repodir: truncate refs.log: %w", err)
	}
	return f.Sync()
}
