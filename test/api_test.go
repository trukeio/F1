package test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/trukeio/f1/internal/auth"
	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
	"github.com/trukeio/f1/internal/oidctest"
	"github.com/trukeio/f1/internal/pack"
	"github.com/trukeio/f1/pkg/f1client"
)

func TestFilesRefsLogDiff(t *testing.T) {
	e := start(t, envOpts{})
	e.createNS(t, "proj", "")
	e.setACL(t, "proj", "group:eng", "", "read,write,lock")

	r1 := put(t, e.alice, "proj", "docs/readme.md", "hello", "new")
	sum := sha256.Sum256([]byte("hello"))
	if r1.SHA256 != hex.EncodeToString(sum[:]) || r1.Size != 5 || r1.SHA1Git != "b6fc4c620b67d95f953a5c1c1230aaab5db5a1b0" {
		t.Fatalf("put result = %+v", r1)
	}
	if _, err := e.alice.Put(ctx, "proj", "main", "x", strings.NewReader("x"), 1, nil); f1client.Code(err) != f1client.CodeParentRequired {
		t.Fatalf("put without F1-Parent: %v", err)
	}
	if _, err := e.alice.Put(ctx, "proj", "main", "x", strings.NewReader("x"), 1, &f1client.PutOptions{Parent: "new"}); f1client.Code(err) != f1client.CodeConflict {
		t.Fatalf("stale parent: %v", err)
	}
	r2 := put(t, e.alice, "proj", "src/main.go", "package main\n", r1.Commit)

	if got := get(t, e.alice, "proj", "main", "docs/readme.md"); got != "hello" {
		t.Fatalf("get = %q", got)
	}
	if got := get(t, e.alice, "proj", r1.Commit, "docs/readme.md"); got != "hello" {
		t.Fatalf("get by commit = %q", got)
	}
	rc, fi, err := e.alice.Get(ctx, "proj", "main", "src/main.go", &f1client.GetOptions{Range: "bytes=8-11"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "main" || fi.Commit != r2.Commit || fi.Size != 13 {
		t.Fatalf("range = %q, info %+v", b, fi)
	}
	if _, _, err := e.alice.Get(ctx, "proj", "main", "src/main.go", &f1client.GetOptions{IfNoneMatch: `"` + r2.File + `"`}); err != nil {
		t.Fatalf("conditional get: %v", err)
	}
	st, err := e.alice.Stat(ctx, "proj", "main", "docs/readme.md")
	if err != nil || st.File != r1.File || st.Kind != "file" {
		t.Fatalf("stat = %+v, %v", st, err)
	}
	if _, err := e.alice.Stat(ctx, "proj", "main", "nope"); f1client.Code(err) != f1client.CodeNotFound {
		t.Fatalf("stat missing: %v", err)
	}

	// Branch names with "/" are escaped by the client.
	if err := e.alice.UpdateRef(ctx, "proj", "heads/feature/x", "", r2.Commit); err != nil {
		t.Fatal(err)
	}
	if got := get(t, e.alice, "proj", "feature/x", "src/main.go"); got != "package main\n" {
		t.Fatal("read through a ref with /")
	}
	refs, err := e.alice.Refs(ctx, "proj")
	if err != nil || len(refs) != 2 || refs[0].Name != "heads/feature/x" {
		t.Fatalf("refs = %+v, %v", refs, err)
	}

	list, err := e.alice.List(ctx, "proj", "main", "", &f1client.ListOptions{Delimiter: "/"})
	if err != nil || len(list.Items) != 2 || !list.Items[0].Prefix || list.Items[0].Path != "docs/" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	list, err = e.alice.List(ctx, "proj", "main", "", &f1client.ListOptions{Limit: 1})
	if err != nil || len(list.Items) != 1 || list.Next != "docs/readme.md" || list.Items[0].Size != 5 {
		t.Fatalf("paged list = %+v, %v", list, err)
	}
	log, err := e.alice.Log(ctx, "proj", "main", "", 0)
	if err != nil || len(log) != 2 || log[0].Hash != r2.Commit || log[0].Author.Name != "alice" {
		t.Fatalf("log = %+v, %v", log, err)
	}
	diff, err := e.alice.Diff(ctx, "proj", r1.Commit, "main", "")
	if err != nil || len(diff) != 1 || diff[0].Path != "src/main.go" || diff[0].Old != nil {
		t.Fatalf("diff = %+v, %v", diff, err)
	}
	dr, err := e.alice.Delete(ctx, "proj", "main", "docs/readme.md", &f1client.PutOptions{Parent: r2.Commit})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.alice.Delete(ctx, "proj", "main", "docs/readme.md", &f1client.PutOptions{Parent: dr.Commit}); f1client.Code(err) != f1client.CodeNotFound {
		t.Fatalf("delete missing: %v", err)
	}
	me, err := e.alice.Me(ctx)
	if err != nil || me.Principal != "user:alice" || len(me.Groups) != 1 {
		t.Fatalf("me = %+v, %v", me, err)
	}
}

func TestAutocommitNamespace(t *testing.T) {
	e := start(t, envOpts{})
	e.createNS(t, "bucket", "s3")
	e.setACL(t, "bucket", "user:alice", "", "read,write")
	for i := range 5 {
		put(t, e.alice, "bucket", fmt.Sprintf("logs/2026/%d.txt", i), fmt.Sprint(i), "")
	}
	list, err := e.alice.List(ctx, "bucket", "main", "logs/", nil)
	if err != nil || len(list.Items) != 5 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if _, err := e.alice.Delete(ctx, "bucket", "main", "logs/2026/0.txt", nil); err != nil {
		t.Fatal(err)
	}
}

// Read and write are allowed and denied by prefix; the longest prefix
// wins; hash-addressed endpoints need whole-namespace read (D13).
func TestACLs(t *testing.T) {
	e := start(t, envOpts{})
	e.createNS(t, "proj", "")
	e.setACL(t, "proj", "group:eng", "", "read,write")
	r := put(t, e.alice, "proj", "docs/guide.md", "guide", "new")
	r = put(t, e.alice, "proj", "docs/internal/plan.md", "plan", r.Commit)
	r = put(t, e.alice, "proj", "src/main.go", "code", r.Commit)

	e.setACL(t, "proj", "user:bob", "docs/", "read")
	e.setACL(t, "proj", "user:bob", "docs/internal/", "") // narrower deny

	if got := get(t, e.bob, "proj", "main", "docs/guide.md"); got != "guide" {
		t.Fatal("bob cannot read docs/")
	}
	for _, p := range []string{"src/main.go", "docs/internal/plan.md"} {
		if _, _, err := e.bob.Get(ctx, "proj", "main", p, nil); f1client.Code(err) != f1client.CodeForbidden {
			t.Errorf("bob reads %s: %v", p, err)
		}
	}
	if _, err := e.bob.Put(ctx, "proj", "main", "docs/new.md", strings.NewReader("x"), 1, &f1client.PutOptions{Parent: r.Commit}); f1client.Code(err) != f1client.CodeForbidden {
		t.Errorf("bob writes: %v", err)
	}
	list, err := e.bob.List(ctx, "proj", "main", "", nil)
	if err != nil || len(list.Items) != 1 || list.Items[0].Path != "docs/guide.md" {
		t.Fatalf("bob's listing = %+v, %v", list, err)
	}
	top, err := e.bob.List(ctx, "proj", "main", "", &f1client.ListOptions{Delimiter: "/"})
	if err != nil || len(top.Items) != 1 || top.Items[0].Path != "docs/" {
		t.Fatalf("bob's top listing = %+v, %v", top, err)
	}
	log, err := e.alice.Log(ctx, "proj", "main", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	first := log[len(log)-1].Hash
	diff, err := e.bob.Diff(ctx, "proj", first, "main", "")
	if err != nil || len(diff) != 0 {
		t.Fatalf("bob's diff shows unreadable paths: %+v, %v", diff, err)
	}

	// D13: hash-addressed access needs whole-namespace read.
	if _, err := e.bob.Log(ctx, "proj", "main", "", 0); f1client.Code(err) != f1client.CodeForbidden {
		t.Errorf("log without path: %v", err)
	}
	if l, err := e.bob.Log(ctx, "proj", "main", "docs/guide.md", 0); err != nil || len(l) != 1 {
		t.Errorf("log of a readable path: %v", err)
	}
	if _, err := e.bob.MissingObjects(ctx, "proj", []string{r.File}); f1client.Code(err) != f1client.CodeForbidden {
		t.Errorf("objects/missing: %v", err)
	}
	if _, _, err := e.bob.GetObject(ctx, "proj", r.File); f1client.Code(err) != f1client.CodeForbidden {
		t.Errorf("objects/{hash}: %v", err)
	}
	if _, err := e.bob.FetchObjects(ctx, "proj", []string{r.File}); f1client.Code(err) != f1client.CodeForbidden {
		t.Errorf("objects/fetch: %v", err)
	}
	if _, err := e.bob.Commit(ctx, "proj", f1client.CommitRequest{Ref: "main", Parent: r.Commit}); f1client.Code(err) != f1client.CodeForbidden {
		t.Errorf("commits: %v", err)
	}
	if err := e.bob.UpdateRef(ctx, "proj", "tags/v1", "", r.Commit); f1client.Code(err) != f1client.CodeForbidden {
		t.Errorf("ref update: %v", err)
	}
	// A user with no grant does not see the namespace at all.
	e2 := f1client.New(e.url, "")
	if _, err := e2.Refs(ctx, "proj"); f1client.Code(err) != f1client.CodeUnauthenticated {
		t.Errorf("no token: %v", err)
	}
	ns, err := e.bob.Namespaces(ctx)
	if err != nil || len(ns) != 1 {
		t.Fatalf("bob's namespaces = %+v, %v", ns, err)
	}
	if err := e.bob.UpdateRef(ctx, "nope", "x", "", r.Commit); f1client.Code(err) != f1client.CodeNotFound {
		t.Errorf("unknown namespace: %v", err)
	}
	// Commits through the object API: write ACL on every changed path.
	e.setACL(t, "proj", "user:bob", "", "read")
	e.setACL(t, "proj", "user:bob", "docs/internal/", "read")
	e.setACL(t, "proj", "user:bob", "docs/", "read,write")
	_, err = e.bob.Commit(ctx, "proj", f1client.CommitRequest{Ref: "main", Parent: r.Commit, Message: "m",
		Changes: []f1client.CommitChange{{Op: "put", Path: "src/x.go", Entry: &f1client.Entry{Kind: "file", Target: r.File}}}})
	if f1client.Code(err) != f1client.CodeForbidden {
		t.Errorf("commit outside the writable prefix: %v", err)
	}
	if _, err := e.bob.Commit(ctx, "proj", f1client.CommitRequest{Ref: "main", Parent: r.Commit, Message: "m",
		Changes: []f1client.CommitChange{{Op: "put", Path: "docs/copy.md", Entry: &f1client.Entry{Kind: "file", Target: r.File}}}}); err != nil {
		t.Errorf("commit inside the writable prefix: %v", err)
	}
}

// Objects can be uploaded as a pack stream and committed by hash.
func TestObjectAPI(t *testing.T) {
	e := start(t, envOpts{})
	e.createNS(t, "proj", "")
	e.setACL(t, "proj", "user:alice", "", "read,write")
	data := []byte("uploaded through the object API")
	chunkHash := object.ChunkHash(data)
	file := &object.File{Size: uint64(len(data)), Refs: []object.Ref{{Hash: chunkHash, Size: uint64(len(data))}}}
	fileHash, fileBytes, err := object.Hash(file)
	if err != nil {
		t.Fatal(err)
	}
	missing, err := e.alice.MissingObjects(ctx, "proj", []string{chunkHash.String(), fileHash.String()})
	if err != nil || len(missing) != 2 {
		t.Fatalf("missing = %v, %v", missing, err)
	}
	var buf bytes.Buffer
	w, _ := pack.NewWriter(&buf)
	for _, o := range []struct {
		typ     object.Type
		h       hash.Hash
		payload []byte
	}{{object.TypeChunk, chunkHash, data}, {object.TypeFile, fileHash, fileBytes}} {
		rec, _ := pack.NewRecord(o.typ, o.h, o.payload, nil)
		w.Add(rec)
	}
	w.Finish()
	// Out of order (file before chunk) violates D9 and is refused.
	var bad bytes.Buffer
	bw, _ := pack.NewWriter(&bad)
	frec, _ := pack.NewRecord(object.TypeFile, fileHash, fileBytes, nil)
	bw.Add(frec)
	bw.Finish()
	if _, err := e.alice.UploadObjects(ctx, "proj", &bad); f1client.Code(err) != f1client.CodeMissingObjects {
		t.Fatalf("upload violating D9: %v", err)
	}
	res, err := e.alice.UploadObjects(ctx, "proj", &buf)
	if err != nil || res.Stored != 2 {
		t.Fatalf("upload = %+v, %v", res, err)
	}
	c, err := e.alice.Commit(ctx, "proj", f1client.CommitRequest{Ref: "main", Message: "via objects",
		Changes: []f1client.CommitChange{{Op: "put", Path: "a/b.txt", Entry: &f1client.Entry{Kind: "file", Target: fileHash.String()}}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := get(t, e.alice, "proj", c.Commit, "a/b.txt"); got != string(data) {
		t.Fatalf("content = %q", got)
	}
	// Digest records are written by the server only: a forged one would
	// make Git, LFS and S3 trust false digests.
	forged := &object.DigestRecord{File: fileHash, SHA256: [32]byte{1}}
	dh, dbytes, _ := object.Hash(forged)
	var fb bytes.Buffer
	fw, _ := pack.NewWriter(&fb)
	drec, _ := pack.NewRecord(object.TypeDigestRecord, dh, dbytes, nil)
	fw.Add(drec)
	fw.Finish()
	if _, err := e.alice.UploadObjects(ctx, "proj", &fb); f1client.Code(err) != f1client.CodeBadRequest {
		t.Fatalf("digest record upload: %v", err)
	}
	typ, payload, err := e.alice.GetObject(ctx, "proj", fileHash.String())
	if err != nil || typ != "file" || !bytes.Equal(payload, fileBytes) {
		t.Fatalf("get object: %s %v", typ, err)
	}
	rc, err := e.alice.FetchObjects(ctx, "proj", []string{chunkHash.String(), fileHash.String()})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	err = pack.Scan(rc, func(r pack.Record) error {
		if _, err := r.Payload(nil); err != nil {
			return err
		}
		got = append(got, r.Hash.String())
		return nil
	})
	rc.Close()
	if err != nil || len(got) != 2 {
		t.Fatalf("fetch = %v, %v", got, err)
	}
}

func TestLocksAPI(t *testing.T) {
	e := start(t, envOpts{})
	if _, err := e.admin.CreateNamespace(ctx, f1client.CreateNamespaceRequest{Name: "hw", Preset: "svn"}); err != nil {
		t.Fatal(err)
	}
	e.setACL(t, "hw", "group:eng", "", "read,write,lock")
	e.setACL(t, "hw", "user:bob", "", "read,write,lock")
	r := put(t, e.alice, "hw", "notes.txt", "free", "new")
	if _, err := e.alice.Put(ctx, "hw", "main", "board/top.sch", strings.NewReader("v1"), 2, &f1client.PutOptions{Parent: r.Commit}); f1client.Code(err) != f1client.CodeLocked {
		t.Fatalf("required lock missing: %v", err)
	}
	l, err := e.alice.Lock(ctx, "hw", "main", "board/top.sch", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	r = put(t, e.alice, "hw", "board/top.sch", "v1", r.Commit)
	_, err = e.bob.Put(ctx, "hw", "main", "board/top.sch", strings.NewReader("v2"), 2, &f1client.PutOptions{Parent: r.Commit})
	var ae *f1client.APIError
	if !errors.As(err, &ae) || ae.Error_.Code != f1client.CodeLocked || ae.Error_.Owner != "user:alice" {
		t.Fatalf("bob on alice's lock: %v", err)
	}
	if _, err := e.bob.Lock(ctx, "hw", "main", "board/top.sch", 0); f1client.Code(err) != f1client.CodeLocked {
		t.Fatalf("second lock: %v", err)
	}
	locks, err := e.bob.Locks(ctx, "hw", "", "")
	if err != nil || len(locks) != 1 || locks[0].Owner != "user:alice" || locks[0].Expires == nil {
		t.Fatalf("locks = %+v, %v", locks, err)
	}
	if err := e.bob.Unlock(ctx, "hw", l.ID, false); f1client.Code(err) != f1client.CodeForbidden {
		t.Fatalf("unlock by another user: %v", err)
	}
	if err := e.bob.Unlock(ctx, "hw", l.ID, true); f1client.Code(err) != f1client.CodeForbidden {
		t.Fatalf("forced unlock without admin: %v", err)
	}
	if err := e.admin.Unlock(ctx, "hw", l.ID, true); err != nil {
		t.Fatalf("forced unlock by admin: %v", err)
	}
	if _, err := e.bob.Lock(ctx, "hw", "main", "board/top.sch", 0); err != nil {
		t.Fatal(err)
	}
	put(t, e.bob, "hw", "board/top.sch", "v2", r.Commit)
}

// An OIDC token from the enterprise provider works end to end; its group
// claim drives ACL decisions.
func TestOIDCEndToEnd(t *testing.T) {
	p := oidctest.New(t)
	e := start(t, envOpts{oidc: []auth.OIDCConfig{{Name: "corp", Issuer: p.Issuer(), Audiences: []string{"f1"}}}})
	e.createNS(t, "proj", "")
	e.setACL(t, "proj", "group:designers", "", "read,write")
	carol := f1client.New(e.url, p.Token(t, "rsa1", jose.RS256, p.Claims("carol", "f1", []string{"designers"}, nil)))
	put(t, carol, "proj", "logo.svg", "<svg/>", "new")
	me, err := carol.Me(ctx)
	if err != nil || me.Principal != "user:carol" || len(me.Groups) != 1 || me.Groups[0] != "group:designers" {
		t.Fatalf("me = %+v, %v", me, err)
	}
	dave := f1client.New(e.url, p.Token(t, "rsa1", jose.RS256, p.Claims("dave", "f1", []string{"sales"}, nil)))
	if _, err := dave.Refs(ctx, "proj"); f1client.Code(err) != f1client.CodeNotFound {
		t.Fatalf("other group: %v", err)
	}
	g1Token := f1client.New(e.url, p.Token(t, "rsa1", jose.RS256, p.Claims("carol", "g1", []string{"designers"}, nil)))
	if _, err := g1Token.Me(ctx); f1client.Code(err) != f1client.CodeUnauthenticated {
		t.Fatalf("a token for G1's audience: %v", err)
	}
	// S3 keys for an external user; listed without secrets.
	k, err := carol.CreateS3Key(ctx)
	if err != nil || k.Secret == "" || !strings.HasPrefix(k.AccessKey, "F1") {
		t.Fatalf("s3 key = %+v, %v", k, err)
	}
	keys, err := carol.S3Keys(ctx)
	if err != nil || len(keys) != 1 || keys[0].Secret != "" {
		t.Fatalf("s3 keys = %+v, %v", keys, err)
	}
	if err := carol.DeleteS3Key(ctx, k.AccessKey); err != nil {
		t.Fatal(err)
	}
}

func TestNamespaceAdmin(t *testing.T) {
	e := start(t, envOpts{})
	if _, err := e.alice.CreateNamespace(ctx, f1client.CreateNamespaceRequest{Name: "x"}); f1client.Code(err) != f1client.CodeForbidden {
		t.Fatalf("create by non-admin: %v", err)
	}
	e.createNS(t, "a", "git")
	if _, err := e.admin.CreateNamespace(ctx, f1client.CreateNamespaceRequest{Name: "a"}); f1client.Code(err) != f1client.CodeExists {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := e.admin.CreateNamespace(ctx, f1client.CreateNamespaceRequest{Name: "bad",
		Config: &f1client.NamespaceConfig{Layout: "flat", Retention: "all", Git: true, Locks: "off", DefaultRef: "main"}}); f1client.Code(err) != f1client.CodeBadRequest {
		t.Fatalf("D12 violation: %v", err)
	}
	ns, err := e.admin.Namespaces(ctx)
	if err != nil || len(ns) != 1 || !ns[0].Config.Git {
		t.Fatalf("namespaces = %+v, %v", ns, err)
	}
}

// /metrics reports requests per API, writes and storage.
func TestMetrics(t *testing.T) {
	e := start(t, envOpts{})
	e.createNS(t, "m", "")
	put(t, e.admin, "m", "a.txt", "metrics", "new")
	resp, err := http.Get(e.url + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	body := string(b)
	for _, want := range []string{
		`f1_http_requests_total{api="native",code="2xx"}`,
		`f1_http_request_duration_seconds_bucket{api="native",le="+Inf"}`,
		"f1_ref_updates_total ",
		"f1_pack_bytes_written_total ",
		"f1_repositories 1",
		"# TYPE f1_pack_bytes gauge",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics lacks %q", want)
		}
	}
	if strings.Contains(body, `namespace="m"`) {
		t.Error("per-namespace metrics are off by default")
	}
}

// TestHTTPCaching checks the cache headers of immutable reads (ADR-0016).
func TestHTTPCaching(t *testing.T) {
	e := start(t, envOpts{})
	e.createNS(t, "proj", "")
	e.setACL(t, "proj", "user:alice", "", "read,write")
	r1 := put(t, e.alice, "proj", "a.txt", "one", "new")
	do := func(path, inm string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("GET", e.url+"/api/v1/ns/proj/"+path, nil)
		req.Header.Set("Authorization", "Bearer "+e.tokens["alice"])
		if inm != "" {
			req.Header.Set("If-None-Match", inm)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp
	}
	const immutable = "private, max-age=31536000, immutable"
	for _, c := range []struct {
		path, cc string
	}{
		{"files/" + r1.Commit + "/a.txt", immutable},
		{"files/main/a.txt", "private, no-cache"},
		{"objects/" + r1.File, immutable},
	} {
		resp := do(c.path, "")
		if resp.StatusCode != 200 || resp.Header.Get("Cache-Control") != c.cc {
			t.Fatalf("%s: %d, Cache-Control %q", c.path, resp.StatusCode, resp.Header.Get("Cache-Control"))
		}
		etag := resp.Header.Get("ETag")
		if etag != `"`+r1.File+`"` {
			t.Fatalf("%s: ETag %q", c.path, etag)
		}
		if resp := do(c.path, etag); resp.StatusCode != http.StatusNotModified {
			t.Fatalf("%s revalidation: %d", c.path, resp.StatusCode)
		}
		if resp := do(c.path, `"other"`); resp.StatusCode != 200 {
			t.Fatalf("%s with another ETag: %d", c.path, resp.StatusCode)
		}
	}
	// A changed file fails revalidation through the ref name.
	put(t, e.alice, "proj", "a.txt", "two", r1.Commit)
	if resp := do("files/main/a.txt", `"`+r1.File+`"`); resp.StatusCode != 200 {
		t.Fatalf("revalidation of a changed file: %d", resp.StatusCode)
	}
	// A branch named like the commit it points to can still move.
	if err := e.alice.UpdateRef(ctx, "proj", "heads/"+r1.Commit, "", r1.Commit); err != nil {
		t.Fatal(err)
	}
	if resp := do("files/"+r1.Commit+"/a.txt", ""); resp.Header.Get("Cache-Control") != "private, no-cache" {
		t.Fatalf("ref named like a hash: %q", resp.Header.Get("Cache-Control"))
	}
	// Permission is checked before a 304.
	if resp := func() *http.Response {
		req, _ := http.NewRequest("GET", e.url+"/api/v1/ns/proj/objects/"+r1.File, nil)
		req.Header.Set("Authorization", "Bearer "+e.tokens["bob"])
		req.Header.Set("If-None-Match", `"`+r1.File+`"`)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}(); resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusNotFound {
		t.Fatalf("object without permission: %d", resp.StatusCode)
	}
}

// TestRebaseAPI checks server-side rebase through the native API
// (ADR-0014).
func TestRebaseAPI(t *testing.T) {
	e := start(t, envOpts{})
	e.createNS(t, "proj", "")
	e.setACL(t, "proj", "group:eng", "", "read,write,lock")
	e.setACL(t, "proj", "user:bob", "", "read,write,lock")
	base := put(t, e.alice, "proj", "a.txt", "a", "new")
	r2 := put(t, e.alice, "proj", "b.txt", "b", base.Commit)

	// Bob, still at base, writes another file: rebased onto Alice's commit.
	res, err := e.bob.Put(ctx, "proj", "main", "c.txt", strings.NewReader("c"), 1, &f1client.PutOptions{Parent: base.Commit, Rebase: true})
	if err != nil || res.Parent != r2.Commit {
		t.Fatalf("rebased put = %+v, %v", res, err)
	}
	if got := get(t, e.bob, "proj", "main", "b.txt"); got != "b" {
		t.Fatal("rebase lost the other write")
	}
	// Without the option, the same write is a conflict as before.
	if _, err := e.bob.Put(ctx, "proj", "main", "d.txt", strings.NewReader("d"), 1, &f1client.PutOptions{Parent: base.Commit}); f1client.Code(err) != f1client.CodeConflict {
		t.Fatalf("strict put: %v", err)
	}
	// A path changed since the parent is a conflict that names it.
	_, err = e.bob.Put(ctx, "proj", "main", "b.txt", strings.NewReader("B"), 1, &f1client.PutOptions{Parent: base.Commit, Rebase: true})
	var ae *f1client.APIError
	if !errors.As(err, &ae) || ae.Error_.Code != f1client.CodeConflict || fmt.Sprint(ae.Error_.Paths) != "[b.txt]" || ae.Error_.Current != res.Commit {
		t.Fatalf("overlapping rebase: %v", err)
	}
	// Deletes and POST /commits rebase too.
	d, err := e.bob.Delete(ctx, "proj", "main", "a.txt", &f1client.PutOptions{Parent: base.Commit, Rebase: true})
	if err != nil || d.Parent != res.Commit {
		t.Fatalf("rebased delete = %+v, %v", d, err)
	}
	c, err := e.alice.Commit(ctx, "proj", f1client.CommitRequest{Ref: "main", Parent: r2.Commit, Rebase: true, Message: "copy",
		Changes: []f1client.CommitChange{{Op: "put", Path: "copy.txt", Entry: &f1client.Entry{Kind: "file", Target: r2.File}}}})
	if err != nil || c.Parent != d.Commit {
		t.Fatalf("rebased commit = %+v, %v", c, err)
	}
	// Locks taken after the parent still apply.
	if _, err := e.alice.Lock(ctx, "proj", "main", "c.txt", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := e.bob.Put(ctx, "proj", "main", "c.txt", strings.NewReader("C"), 1, &f1client.PutOptions{Parent: res.Commit, Rebase: true}); f1client.Code(err) != f1client.CodeLocked {
		t.Fatalf("rebased write to a locked file: %v", err)
	}
	// A bad header value is refused.
	req, _ := http.NewRequest("PUT", e.url+"/api/v1/ns/proj/files/main/x.txt", strings.NewReader("x"))
	req.Header.Set("Authorization", "Bearer "+e.tokens["bob"])
	req.Header.Set("F1-Parent", base.Commit)
	req.Header.Set("F1-Rebase", "yes")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("F1-Rebase: yes: %d", resp.StatusCode)
	}
}
