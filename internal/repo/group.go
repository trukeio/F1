package repo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
	"github.com/trukeio/f1/internal/policy"
)

// Precondition checks the current entry at a change's path (nil if the path
// does not exist), as seen inside a group commit batch: it reflects the
// changes of earlier requests in the same batch. Conditional S3 writes
// (If-Match, If-None-Match) use it.
type Precondition func(cur *object.Entry) error

// SubmitRequest is one change for group commit.
type SubmitRequest struct {
	Ref          string
	Change       Change
	Precondition Precondition
	Author       Person
	Principal    string
	Message      string // used when the batch has only this change
}

type pendingReq struct {
	req  SubmitRequest
	done chan submitResult
}

type submitResult struct {
	commit hash.Hash
	err    error
}

type committer struct {
	r    *Repo
	ref  string
	reqs chan *pendingReq
}

// groupLinger is how long the committer waits for another request before
// committing a batch.
const groupLinger = time.Millisecond

// maxConflictRetries bounds how often a batch is retried after another
// writer moved the ref.
const maxConflictRetries = 20

// Submit queues a change for the ref's group committer and waits for the
// commit that contains it. Concurrent submissions share one commit: a batch
// takes the queued requests and those arriving less than groupLinger
// apart, for at most the group window (20 ms) and 1000 changes. Preconditions are
// evaluated in submission order against the state inside the batch.
func (r *Repo) Submit(ctx context.Context, req SubmitRequest) (hash.Hash, error) {
	req.Ref = policy.FullRefName(req.Ref)
	if err := policy.ValidRefName(req.Ref); err != nil {
		return hash.Zero, err
	}
	c, err := r.committerFor(req.Ref)
	if err != nil {
		return hash.Zero, err
	}
	p := &pendingReq{req: req, done: make(chan submitResult, 1)}
	select {
	case c.reqs <- p:
	case <-ctx.Done():
		return hash.Zero, ctx.Err()
	case <-r.stopCtx.Done():
		return hash.Zero, ErrClosed
	}
	select {
	case res := <-p.done:
		return res.commit, res.err
	case <-ctx.Done():
		// The change may still be committed.
		return hash.Zero, ctx.Err()
	}
}

func (r *Repo) committerFor(ref string) (*committer, error) {
	r.groupMu.Lock()
	defer r.groupMu.Unlock()
	if r.closed {
		return nil, ErrClosed
	}
	if c, ok := r.groups[ref]; ok {
		return c, nil
	}
	c := &committer{r: r, ref: ref, reqs: make(chan *pendingReq, r.opts.GroupMax)}
	r.groups[ref] = c
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		c.run()
	}()
	return c, nil
}

func (c *committer) run() {
	stop := c.r.stopCtx.Done()
	for {
		var batch []*pendingReq
		select {
		case p := <-c.reqs:
			batch = append(batch, p)
		case <-stop:
			c.failQueued()
			return
		}
		// Take what is queued, and what arrives within a short linger
		// (bounded by the window): requests that arrive while this batch
		// commits form the next one, so a lone writer does not wait for
		// the window and concurrent writers share commits.
		deadline := time.NewTimer(c.r.opts.GroupWindow)
		linger := time.NewTimer(groupLinger)
	collect:
		for len(batch) < c.r.opts.GroupMax {
			select {
			case p := <-c.reqs:
				batch = append(batch, p)
				linger.Reset(groupLinger)
			case <-linger.C:
				break collect
			case <-deadline.C:
				break collect
			case <-stop:
				break collect
			}
		}
		deadline.Stop()
		linger.Stop()
		c.process(batch)
	}
}

func (c *committer) failQueued() {
	for {
		select {
		case p := <-c.reqs:
			p.done <- submitResult{err: ErrClosed}
		default:
			return
		}
	}
}

// process commits a batch. On a CAS conflict it re-reads the ref and
// re-evaluates the preconditions; if the batch fails for another reason,
// each change is committed on its own so that one bad change does not fail
// the others.
func (c *committer) process(batch []*pendingReq) {
	ctx := c.r.stopCtx
	for attempt := 0; ; attempt++ {
		head := c.r.Ref(c.ref)
		accepted, changes, err := c.evaluate(ctx, head, batch)
		if err != nil {
			deliver(batch, hash.Zero, err)
			return
		}
		if len(accepted) == 0 {
			return
		}
		req := CommitRequest{
			Ref:       c.ref,
			Parent:    &head,
			Changes:   changes,
			Author:    accepted[0].req.Author,
			Principal: accepted[0].req.Principal,
			Message:   batchMessage(accepted),
			skipLocks: true, // checked per request in evaluate
		}
		h, err := c.r.Commit(ctx, req)
		switch {
		case err == nil:
			deliver(accepted, h, nil)
			return
		case errors.Is(err, ErrConflict) && attempt < maxConflictRetries:
			// Rejected requests have their answer; re-evaluate the rest
			// against the new head.
			batch = accepted
			continue
		case len(accepted) > 1 && !errors.Is(err, ErrConflict) && ctx.Err() == nil:
			for _, p := range accepted {
				c.process([]*pendingReq{p})
			}
			return
		default:
			deliver(accepted, hash.Zero, err)
			return
		}
	}
}

// evaluate checks preconditions in order against the base tree plus the
// changes accepted so far. Rejected requests get their error immediately.
func (c *committer) evaluate(ctx context.Context, head hash.Hash, batch []*pendingReq) ([]*pendingReq, []Change, error) {
	base, err := c.r.commitTree(ctx, head)
	if err != nil {
		return nil, nil, err
	}
	overlay := map[string]*object.Entry{}
	known := map[string]bool{}
	var accepted []*pendingReq
	var changes []Change
	for _, p := range batch {
		ch := p.req.Change
		if p.req.Precondition != nil {
			var cur *object.Entry
			if known[ch.Path] {
				cur = overlay[ch.Path]
			} else {
				e, err := c.r.statTree(ctx, base, ch.Path)
				switch {
				case err == nil:
					cur = &e
				case errors.Is(err, ErrNotFound):
				default:
					p.done <- submitResult{err: err}
					continue
				}
			}
			if err := p.req.Precondition(cur); err != nil {
				p.done <- submitResult{err: err}
				continue
			}
		}
		if err := c.checkLocks(head, p.req); err != nil {
			p.done <- submitResult{err: err}
			continue
		}
		accepted = append(accepted, p)
		changes = append(changes, ch)
		switch ch.Op {
		case OpPut:
			e := ch.Entry
			overlay[ch.Path], known[ch.Path] = &e, true
		case OpDelete:
			overlay[ch.Path], known[ch.Path] = nil, true
		case OpRename:
			overlay[ch.From], known[ch.From] = nil, true
			delete(known, ch.Path) // re-read from the base if needed
		}
	}
	return accepted, changes, nil
}

// checkLocks checks one request's paths against the lock policy with the
// request's own principal: a batch may mix principals, so the commit itself
// skips the lock check.
func (c *committer) checkLocks(head hash.Hash, req SubmitRequest) error {
	mode, _ := policy.ParseLocks(c.r.meta.Locks)
	if mode.Mode == "off" || !isBranch(c.ref) {
		return nil
	}
	refs := []string{c.ref}
	if def := policy.FullRefName(c.r.meta.DefaultRef); head.IsZero() && def != c.ref {
		refs = append(refs, def)
	}
	c.r.locksMu.Lock()
	defer c.r.locksMu.Unlock()
	now := time.Now()
	for _, p := range changePaths(req.Change) {
		if err := c.r.lockCheck(refs, p, req.Principal, mode, now); err != nil {
			return err
		}
	}
	return nil
}

func batchMessage(accepted []*pendingReq) string {
	if len(accepted) == 1 {
		if m := accepted[0].req.Message; m != "" {
			return m
		}
		ch := accepted[0].req.Change
		switch ch.Op {
		case OpDelete:
			return "delete " + ch.Path
		case OpRename:
			return "rename " + ch.From + " to " + ch.Path
		default:
			return "put " + ch.Path
		}
	}
	return fmt.Sprintf("autocommit: %d changes", len(accepted))
}

func deliver(ps []*pendingReq, h hash.Hash, err error) {
	for _, p := range ps {
		p.done <- submitResult{commit: h, err: err}
	}
}
