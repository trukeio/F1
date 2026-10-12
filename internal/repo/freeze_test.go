package repo

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/trukeio/f1/internal/policy"
)

// slowReader blocks until released, simulating an upload in progress.
type slowReader struct {
	r       io.Reader
	started chan struct{}
	release chan struct{}
	once    bool
}

func (s *slowReader) Read(p []byte) (int, error) {
	if !s.once {
		s.once = true
		close(s.started)
		<-s.release
	}
	return s.r.Read(p)
}

// Freeze rejects new writes at once, but waits for writes in progress.
func TestFreezeWaitsForInflightWrites(t *testing.T) {
	r := newRepo(t, policy.Default())
	put(t, r, "main", map[string]string{"a": "1"})
	sr := &slowReader{r: strings.NewReader("upload in progress"), started: make(chan struct{}), release: make(chan struct{})}
	ingestDone := make(chan error, 1)
	go func() {
		_, err := r.Ingest(ctx, sr, -1)
		ingestDone <- err
	}()
	<-sr.started

	frozen := make(chan error, 1)
	go func() { frozen <- r.Freeze(ctx) }()
	// New writes fail fast while Freeze waits.
	deadline := time.Now().Add(5 * time.Second)
	for !r.Frozen() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if _, err := r.Ingest(ctx, strings.NewReader("new"), 3); !errors.Is(err, ErrFrozen) {
		t.Fatalf("new write while freezing: %v", err)
	}
	select {
	case <-frozen:
		t.Fatal("Freeze returned while a write was in progress")
	case <-time.After(50 * time.Millisecond):
	}
	close(sr.release)
	if err := <-ingestDone; err != nil {
		t.Fatalf("in-flight write failed: %v", err)
	}
	if err := <-frozen; err != nil {
		t.Fatal(err)
	}
	if !r.Store().Held() {
		t.Fatal("frozen repository has no store hold")
	}
	if err := r.UpdateRefs(ctx, []RefUpdate{{Ref: "tags/t", New: r.Ref("main")}}, "", ""); !errors.Is(err, ErrFrozen) {
		t.Fatalf("ref update while frozen: %v", err)
	}
	r.Unfreeze()
	if r.Store().Held() || r.Frozen() {
		t.Fatal("unfreeze left the repository frozen or held")
	}
	put(t, r, "main", map[string]string{"b": "2"})
}
