package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/trukeio/f1/internal/hash"
)

func TestBloom(t *testing.T) {
	b := newBloom(10000)
	var in []hash.Hash
	for i := range 10000 {
		h := hash.Plain([]byte(fmt.Sprint("in ", i)))
		b.add(h)
		in = append(in, h)
	}
	for _, h := range in {
		if !b.mayContain(h) {
			t.Fatal("false negative")
		}
	}
	fp := 0
	for i := range 100000 {
		if b.mayContain(hash.Plain([]byte(fmt.Sprint("out ", i)))) {
			fp++
		}
	}
	if fp > 1000 {
		t.Fatalf("%d false positives in 100000", fp)
	}
}

// waitFilter waits for a background filter build.
func waitFilter(t *testing.T, s *Store) {
	t.Helper()
	for range 500 {
		s.filter.mu.Lock()
		ready := s.filter.bloom != nil && !s.filter.building
		s.filter.mu.Unlock()
		if ready {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("filter not built")
}

// Every stored object is found, whether the filter is being built, built,
// or rebuilt after growing past its size; absent objects are not.
func TestFilterNoFalseNegatives(t *testing.T) {
	path := newDir(t)
	o := open(t, path, Options{})
	var hs []hash.Hash
	put := func(n int) {
		for range n {
			rec := chunkRecord(t, []byte(fmt.Sprint("object ", len(hs))))
			if err := o.s.Put(ctx, rec); err != nil {
				t.Fatal(err)
			}
			hs = append(hs, rec.Hash)
		}
	}
	check := func(when string) {
		t.Helper()
		for _, h := range hs {
			if ok, err := o.s.Has(ctx, h); err != nil || !ok {
				t.Fatalf("%s: stored object %s not found (%v)", when, h.Short(), err)
			}
		}
		for i := range 100 {
			if ok, _ := o.s.Has(ctx, hash.Plain([]byte(fmt.Sprint("absent ", i)))); ok {
				t.Fatalf("%s: absent object found", when)
			}
		}
	}
	put(1000)
	check("open pack")
	o.s.Flush(ctx)
	check("sealed")
	waitFilter(t, o.s)
	put(1000)
	o.s.Flush(ctx)
	check("sealed after the build")
	o.close(t)

	// Reopened: the filter is built in the background while checks run.
	o = open(t, path, Options{})
	check("during the build")
	waitFilter(t, o.s)
	check("after the build")
	// Outgrow the filter's size: it is rebuilt in the background.
	o.s.filter.mu.Lock()
	o.s.filter.bloom.cap = 100
	o.s.filter.mu.Unlock()
	put(500)
	o.s.Flush(ctx)
	check("while rebuilding")
	waitFilter(t, o.s)
	check("rebuilt")
	o.close(t)
}
