// Package gittest imports Git repositories into F1 for tests: it reads every
// object with the git command and converts it with the production
// importer (internal/api/git), dependencies first, then sets the refs.
package gittest

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"

	"github.com/trukeio/f1/internal/api/git"
	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/repo"
)

// Import copies every object of the Git repository at dir into rp and sets
// rp's refs to the repository's refs (refs/heads/x → heads/x).
func Import(ctx context.Context, rp *repo.Repo, dir string) error {
	cat, err := newCatFile(dir)
	if err != nil {
		return err
	}
	defer cat.close()
	out, err := exec.Command("git", "-C", dir, "cat-file", "--batch-all-objects", "--batch-check=%(objectname)").Output()
	if err != nil {
		return fmt.Errorf("gittest: listing objects: %w", err)
	}
	im := git.NewImporter(rp)
	d := &importer{im: im, cat: cat}
	for _, line := range strings.Fields(string(out)) {
		s, err := git.ParseSHA(line)
		if err != nil {
			return err
		}
		if err := d.object(ctx, s); err != nil {
			return err
		}
	}
	refsOut, err := exec.Command("git", "-C", dir, "for-each-ref", "--format=%(objectname) %(refname)").Output()
	if err != nil {
		return fmt.Errorf("gittest: listing refs: %w", err)
	}
	var updates []repo.RefUpdate
	for _, line := range strings.Split(strings.TrimSpace(string(refsOut)), "\n") {
		if line == "" {
			continue
		}
		sha, name, _ := strings.Cut(line, " ")
		s, err := git.ParseSHA(sha)
		if err != nil {
			return err
		}
		o, ok := im.Lookup(s)
		if !ok {
			return fmt.Errorf("gittest: ref %s points to %s, not imported", name, sha)
		}
		updates = append(updates, repo.RefUpdate{Ref: strings.TrimPrefix(name, "refs/"), Old: hash.Zero, New: o.F1})
	}
	if len(updates) == 0 {
		return nil
	}
	return rp.UpdateRefs(ctx, updates, repo.UpdateOptions{Principal: "user:gittest", Reason: "gittest: import"})
}

type importer struct {
	im  *git.Importer
	cat *catFile
}

// object imports s after everything it references.
func (d *importer) object(ctx context.Context, s git.SHA) error {
	if _, ok := d.im.Lookup(s); ok {
		return nil
	}
	t, data, err := d.cat.read(s)
	if err != nil {
		return err
	}
	switch t {
	case git.TypeBlob:
		return d.im.Blob(ctx, s, bytes.NewReader(data), int64(len(data)))
	case git.TypeTree:
		entries, err := git.ParseTree(data)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if e.Mode == git.ModeGitlink {
				continue // a commit of another repository
			}
			if err := d.object(ctx, e.SHA); err != nil {
				return err
			}
		}
		return d.im.Tree(ctx, s, data)
	case git.TypeCommit:
		pc, err := git.ParseCommit(data)
		if err != nil {
			return err
		}
		if err := d.object(ctx, pc.Tree); err != nil {
			return err
		}
		for _, p := range pc.Parents {
			if err := d.object(ctx, p); err != nil {
				return err
			}
		}
		return d.im.Commit(ctx, s, data)
	case git.TypeTag:
		pt, err := git.ParseTag(data)
		if err != nil {
			return err
		}
		if err := d.object(ctx, pt.Object); err != nil {
			return err
		}
		return d.im.Tag(ctx, s, data)
	}
	return fmt.Errorf("gittest: object %s has unknown type", s)
}

// catFile reads objects with "git cat-file --batch".
type catFile struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Reader
}

func newCatFile(dir string) (*catFile, error) {
	cmd := exec.Command("git", "-C", dir, "cat-file", "--batch")
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("gittest: %w", err)
	}
	return &catFile{cmd: cmd, in: in, out: bufio.NewReader(out)}, nil
}

func (c *catFile) read(s git.SHA) (git.Type, []byte, error) {
	if _, err := fmt.Fprintln(c.in, s); err != nil {
		return 0, nil, err
	}
	hdr, err := c.out.ReadString('\n')
	if err != nil {
		return 0, nil, err
	}
	f := strings.Fields(hdr)
	if len(f) != 3 {
		return 0, nil, fmt.Errorf("gittest: cat-file: %q", hdr)
	}
	t, ok := git.TypeByName(f[1])
	if !ok {
		return 0, nil, fmt.Errorf("gittest: cat-file type %q", f[1])
	}
	n, err := strconv.Atoi(f[2])
	if err != nil {
		return 0, nil, err
	}
	data := make([]byte, n+1) // content and newline
	if _, err := io.ReadFull(c.out, data); err != nil {
		return 0, nil, err
	}
	return t, data[:n], nil
}

func (c *catFile) close() {
	c.in.Close()
	c.cmd.Wait()
}
