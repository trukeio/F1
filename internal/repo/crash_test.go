package repo

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trukeio/f1/internal/gc"
	"github.com/trukeio/f1/internal/object"
	"github.com/trukeio/f1/internal/policy"
	"github.com/trukeio/f1/internal/store"
	"github.com/trukeio/f1/internal/testutil"
)

// TestCrashChild is the writer process of TestCrashRecovery. It writes until
// it is killed and prints "ACK <ref> <path> <sha256>" for every write whose
// commit has returned (is acknowledged).
func TestCrashChild(t *testing.T) {
	path := os.Getenv("F1_CRASH_DIR")
	if path == "" {
		t.Skip("helper process for TestCrashRecovery")
	}
	cycle := os.Getenv("F1_CRASH_CYCLE")
	r, _, err := Open(ctx, path, Options{Store: storeOptsForCrash()})
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(3)
	}
	var mu sync.Mutex
	ack := func(ref, p string, data []byte) {
		sum := sha256.Sum256(data)
		mu.Lock()
		fmt.Fprintf(os.Stdout, "ACK %s %s %s\n", ref, p, hex.EncodeToString(sum[:]))
		mu.Unlock()
	}
	fail := func(err error) {
		fmt.Fprintln(os.Stderr, "writer:", err)
		os.Exit(3)
	}
	data := func(seed string) []byte {
		n := 100 + int(sha256.Sum256([]byte(seed))[0])*1200 // up to ~300 KB: multi-chunk files
		return testutil.Bytes(seed, n)
	}
	entry := func(d []byte) object.Entry {
		res, err := r.Ingest(ctx, bytes.NewReader(d), int64(len(d)))
		if err != nil {
			fail(err)
		}
		return object.Entry{Kind: object.KindFile, Target: res.File}
	}
	// Group-commit writers on main.
	for w := range 4 {
		go func() {
			for j := 0; ; j++ {
				p := fmt.Sprintf("c%s/w%d/f%d", cycle, w, j)
				d := data(p)
				e := entry(d)
				if _, err := r.Submit(ctx, SubmitRequest{Ref: "main", Change: Change{Op: OpPut, Path: p, Entry: e}, Author: alice}); err != nil {
					fail(err)
				}
				ack("main", p, d)
			}
		}()
	}
	// A CAS writer on another branch.
	go func() {
		for j := 0; ; j++ {
			p := fmt.Sprintf("c%s/side/f%d", cycle, j)
			d := data(p)
			parent := r.Ref("side")
			e := entry(d)
			_, err := r.Commit(ctx, CommitRequest{Ref: "side", Parent: &parent, Author: alice,
				Changes: []Change{{Op: OpPut, Path: p, Entry: e}}})
			if err != nil {
				fail(err)
			}
			ack("side", p, d)
		}
	}()
	select {}
}

// storeOptsForCrash makes packs small so that crashes hit seals,
// consolidation and pack rollover often.
func storeOptsForCrash() store.Options {
	return store.Options{PackTarget: 512 << 10, SmallPackLimit: 16, DeleteGrace: -1}
}

type ackRecord struct{ ref, path, sum string }

// TestCrashRecovery kills a writer process at random points. After each
// crash: fsck reports no errors, every acknowledged write is present with
// its content, and after a reopen fsck is completely clean.
func TestCrashRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess crash test")
	}
	path := filepath.Join(t.TempDir(), "ns")
	if err := Create(path, policy.Default()); err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(11, 12))
	var acked []ackRecord
	for cycle := range 4 {
		cmd := exec.Command(os.Args[0], "-test.run=^TestCrashChild$")
		cmd.Env = append(os.Environ(), "F1_CRASH_DIR="+path, fmt.Sprintf("F1_CRASH_CYCLE=%d", cycle))
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		want := 20 + rng.IntN(80)
		lines := make(chan string)
		go func() {
			sc := bufio.NewScanner(out)
			for sc.Scan() {
				lines <- sc.Text()
			}
			close(lines)
		}()
		n := 0
		deadline := time.After(2 * time.Minute)
	read:
		for n < want {
			select {
			case l, ok := <-lines:
				if !ok {
					t.Fatalf("writer exited early: %s", stderr.String())
				}
				if rec, ok := parseAck(l); ok {
					acked = append(acked, rec)
					n++
				}
			case <-deadline:
				break read
			}
		}
		// Kill without warning, then collect acks printed before the kill.
		cmd.Process.Kill()
		for l := range lines {
			if rec, ok := parseAck(l); ok {
				acked = append(acked, rec)
			}
		}
		cmd.Wait()
		if strings.Contains(stderr.String(), "writer:") {
			t.Fatalf("writer failed: %s", stderr.String())
		}

		rep, err := gc.CheckDir(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		if !rep.OK() {
			t.Fatalf("cycle %d: fsck after crash: %v", cycle, rep.Findings)
		}
		verifyAcked(t, path, acked)
		if rep, err = gc.CheckDir(ctx, path); err != nil || !rep.Clean() {
			t.Fatalf("cycle %d: fsck after reopen: %v %v", cycle, err, rep.Findings)
		}
		t.Logf("cycle %d: %d acknowledged writes verified, %d objects, %d packs", cycle, len(acked), rep.Objects, rep.Packs)
	}
}

func parseAck(line string) (ackRecord, bool) {
	f := strings.Fields(line)
	if len(f) != 4 || f[0] != "ACK" {
		return ackRecord{}, false
	}
	return ackRecord{f[1], f[2], f[3]}, true
}

func verifyAcked(t *testing.T, path string, acked []ackRecord) {
	t.Helper()
	r, _, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(ctx)
	for _, a := range acked {
		head := r.Ref(a.ref)
		rd, _, err := r.Open(ctx, head, a.path)
		if errors.Is(err, ErrNotFound) {
			t.Fatalf("acknowledged write %s on %s is missing", a.path, a.ref)
		}
		if err != nil {
			t.Fatal(err)
		}
		h := sha256.New()
		if _, err := io.Copy(h, rd); err != nil {
			t.Fatal(err)
		}
		if hex.EncodeToString(h.Sum(nil)) != a.sum {
			t.Fatalf("acknowledged write %s has wrong content", a.path)
		}
	}
}
