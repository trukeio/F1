package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
	"github.com/trukeio/f1/internal/pack"
	"github.com/trukeio/f1/pkg/f1client"
)

// ErrRemoteChanged is returned by Push when the branch has commits that
// the working copy does not have.
var ErrRemoteChanged = errors.New("the branch has new commits on the server; run f1 pull, then push again")

// pushRetries bounds the uploads of one commit: objects/missing may
// report an object present that garbage collection removes before the
// commit (doc/plan.md, "Native client").
const pushRetries = 3

// missingBatch is the number of hashes per objects/missing request.
const missingBatch = 10000

// PushResult reports what a push did.
type PushResult struct {
	Pushed int
	// Pull is set when the server rebased commits onto changes made by
	// others (ADR-0014): the working copy was then updated to the new
	// head as by Pull.
	Pull *PullResult
}

// Push sends the local commits to the server in order. Each commit's new
// content is uploaded (only the objects the server lacks), then the
// commit is created on the previous one.
//
// With rebase, the server applies a commit on top of changes made by
// others if they changed none of its paths (ADR-0014); the working copy is
// then updated as by Pull. Without it, any change on the server makes the
// push fail with ErrRemoteChanged.
func (wc *WC) Push(ctx context.Context, rebase bool) (*PushResult, error) {
	if wc.detached() {
		return nil, errors.New("cannot push from a detached working copy (check out a branch)")
	}
	res := &PushResult{}
	parent := wc.State.Base
	rebased := false
	for len(wc.State.Commits) > 0 {
		c := wc.State.Commits[0]
		r, err := wc.pushOne(ctx, &c, parent, rebase)
		if err != nil {
			return res, err
		}
		rebased = rebased || r.Parent != "" && r.Parent != parent // servers before ADR-0014 send no parent
		parent = r.Commit
		// After a rebase, the base stays where the working files are; the
		// pull below moves it. Pushed commits are dropped either way: a
		// later pull finds their changes already in the index.
		if !rebased {
			wc.State.Base = r.Commit
		}
		wc.State.Commits = wc.State.Commits[1:]
		res.Pushed++
		if err := wc.save(); err != nil {
			return res, err
		}
		wc.printf("pushed %s %s\n", r.Commit[:12], firstLine(c.Message))
	}
	if rebased {
		pr, err := wc.Pull(ctx)
		if err != nil {
			return res, fmt.Errorf("pushed; updating the working copy failed (run f1 pull): %w", err)
		}
		res.Pull = pr
		return res, nil
	}
	if err := wc.objects.removeAll(); err != nil {
		return res, err
	}
	return res, nil
}

func (wc *WC) pushOne(ctx context.Context, c *LocalCommit, parent string, rebase bool) (f1client.CommitResult, error) {
	req := f1client.CommitRequest{Ref: wc.Config.Ref, Parent: parent, Message: c.Message, Rebase: rebase}
	var files []hash.Hash
	for _, ch := range c.Changes {
		cc := f1client.CommitChange{Path: ch.Path}
		if ch.New == nil {
			cc.Op = "delete"
		} else {
			cc.Op = "put"
			cc.Entry = &f1client.Entry{Kind: ch.New.Kind, Target: ch.New.File}
			h, err := hash.Parse(ch.New.File)
			if err != nil {
				return f1client.CommitResult{}, err
			}
			files = append(files, h)
		}
		req.Changes = append(req.Changes, cc)
	}
	for attempt := 0; ; attempt++ {
		if err := wc.upload(ctx, files); err != nil {
			return f1client.CommitResult{}, err
		}
		res, err := wc.api.Commit(ctx, wc.Config.Namespace, req)
		var ae *f1client.APIError
		errors.As(err, &ae)
		switch f1client.Code(err) {
		case "":
			return res, nil
		case f1client.CodeConflict:
			if len(ae.Error_.Paths) > 0 {
				return res, fmt.Errorf("%w (changed there: %s)", ErrRemoteChanged, strings.Join(ae.Error_.Paths, ", "))
			}
			return res, ErrRemoteChanged
		case f1client.CodeMissingObjects:
			if attempt+1 < pushRetries {
				wc.printf("objects disappeared from the server before the commit; uploading again\n")
				continue
			}
		}
		return res, fmt.Errorf("push %q: %w", firstLine(c.Message), err)
	}
}

// upload sends the objects of files (and everything they reference) that
// the server does not have, in dependency order. Files whose content is
// not in the local cache came from the server and are not uploaded.
func (wc *WC) upload(ctx context.Context, files []hash.Hash) error {
	// The closure of the files, each object after its references.
	var order []hash.Hash
	seen := map[hash.Hash]bool{}
	for _, f := range files {
		if seen[f] {
			continue
		}
		if ok, _ := wc.objects.Has(ctx, f); !ok {
			continue
		}
		err := wc.objects.walk(ctx, f, func(h hash.Hash, _ bool) error {
			if !seen[h] {
				seen[h] = true
				order = append(order, h)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	if len(order) == 0 {
		return nil
	}
	missing := map[hash.Hash]bool{}
	for i := 0; i < len(order); i += missingBatch {
		batch := order[i:min(i+missingBatch, len(order))]
		list := make([]string, len(batch))
		for j, h := range batch {
			list[j] = h.String()
		}
		m, err := wc.api.MissingObjects(ctx, wc.Config.Namespace, list)
		if err != nil {
			return err
		}
		for _, s := range m {
			h, err := hash.Parse(s)
			if err != nil {
				return err
			}
			missing[h] = true
		}
	}
	if len(missing) == 0 {
		return nil
	}
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(wc.writePack(ctx, pw, order, missing))
	}()
	_, err := wc.api.UploadObjects(ctx, wc.Config.Namespace, pr)
	pr.CloseWithError(err)
	return err
}

// writePack writes the missing objects as a pack stream, in order.
func (wc *WC) writePack(ctx context.Context, w io.Writer, order []hash.Hash, missing map[hash.Hash]bool) error {
	pw, err := pack.NewWriter(w)
	if err != nil {
		return err
	}
	for _, h := range order {
		if !missing[h] {
			continue
		}
		rec, err := wc.objects.Get(ctx, h)
		if err != nil {
			return err
		}
		if rec.Type == object.TypeDigestRecord {
			continue
		}
		if _, err := pw.Add(rec); err != nil {
			return err
		}
	}
	_, err = pw.Finish()
	return err
}

func firstLine(s string) string {
	for i, c := range s {
		if c == '\n' {
			return s[:i]
		}
	}
	return s
}
