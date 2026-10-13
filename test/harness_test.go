// Package test holds integration tests that run a real server in-process
// and talk to it through the public Go client (pkg/f1client).
package test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trukeio/f1/internal/auth"
	"github.com/trukeio/f1/internal/config"
	"github.com/trukeio/f1/internal/server"
	"github.com/trukeio/f1/internal/serverdb"
	"github.com/trukeio/f1/pkg/f1client"
)

var ctx = context.Background()

// env is a running server with an administrator and two users.
type env struct {
	dataDir string
	url     string
	s3url   string
	s3keys  map[string]serverdb.S3Key // per user name
	tokens  map[string]string         // native tokens per user name
	admin   *f1client.Client          // user:admin, server administrator
	alice   *f1client.Client          // user:alice, in group:eng
	bob     *f1client.Client          // user:bob, no groups
	db      *serverdb.DB              // a second handle on server.db, for ACL setup
	stop    func()
}

type envOpts struct {
	oidc    []auth.OIDCConfig
	aliases map[string][2]string // S3 bucket alias → namespace, ref
	dataDir string               // reuse a data directory (users and keys are created once)
	setup   func(dataDir string) // runs before the server starts
}

func start(t *testing.T, opts envOpts) *env {
	t.Helper()
	dataDir := opts.dataDir
	fresh := dataDir == ""
	if fresh {
		dataDir = t.TempDir()
	}
	db, err := serverdb.Open(ctx, filepath.Join(dataDir, server.ServerDB))
	if err != nil {
		t.Fatal(err)
	}
	e := &env{dataDir: dataDir, db: db, s3keys: map[string]serverdb.S3Key{}, tokens: map[string]string{}}
	tokens := e.tokens
	for user, groups := range map[string][]string{"admin": nil, "alice": {"group:eng"}, "bob": nil} {
		if fresh {
			if err := db.AddUser(ctx, "user:"+user, groups); err != nil {
				t.Fatal(err)
			}
		}
		_, tok, err := db.CreateToken(ctx, "user:"+user, "test", 0)
		if err != nil {
			t.Fatal(err)
		}
		tokens[user] = tok
		k, err := db.CreateS3Key(ctx, "user:"+user)
		if err != nil {
			t.Fatal(err)
		}
		e.s3keys[user] = k
	}
	if fresh {
		e.setACL(t, serverdb.ServerNamespace, "user:admin", "", "admin")
	}
	if opts.setup != nil {
		opts.setup(dataDir)
	}

	cfg := config.Default()
	cfg.Server.DataDir = dataDir
	cfg.Server.Listen = "127.0.0.1:0"
	cfg.Server.S3.Listen = "127.0.0.1:0"
	cfg.Auth.OIDC = opts.oidc
	for b, a := range opts.aliases {
		cfg.Server.S3.Aliases = append(cfg.Server.S3.Aliases, config.S3Alias{Bucket: b, Namespace: a[0], Ref: a[1]})
	}
	runCtx, cancel := context.WithCancel(ctx)
	addr := make(chan server.Addrs, 1)
	done := make(chan error, 1)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() { done <- server.Run(runCtx, cfg, log, func(a server.Addrs) { addr <- a }) }()
	select {
	case a := <-addr:
		e.url = "http://" + a.Main.String()
		e.s3url = "http://" + a.S3.String()
	case err := <-done:
		t.Fatalf("server: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("server did not start")
	}
	var once sync.Once
	e.stop = func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("server: %v", err)
			}
			db.Close()
		})
	}
	t.Cleanup(e.stop)
	e.admin = f1client.New(e.url, tokens["admin"])
	e.alice = f1client.New(e.url, tokens["alice"])
	e.bob = f1client.New(e.url, tokens["bob"])
	return e
}

func (e *env) setACL(t *testing.T, ns, principal, prefix, perms string) {
	t.Helper()
	var p []string
	if perms != "" {
		p = strings.Split(perms, ",")
	}
	if err := e.db.SetACL(ctx, serverdb.ACLEntry{Namespace: ns, Principal: principal, Prefix: prefix, Perms: p}); err != nil {
		t.Fatal(err)
	}
}

func (e *env) createNS(t *testing.T, name, preset string) {
	t.Helper()
	if _, err := e.admin.CreateNamespace(ctx, f1client.CreateNamespaceRequest{Name: name, Preset: preset}); err != nil {
		t.Fatal(err)
	}
}

func put(t *testing.T, c *f1client.Client, ns, path, data, parent string) f1client.PutResult {
	t.Helper()
	res, err := c.Put(ctx, ns, "main", path, strings.NewReader(data), int64(len(data)), &f1client.PutOptions{Parent: parent})
	if err != nil {
		t.Fatalf("put %s: %v", path, err)
	}
	return res
}

func get(t *testing.T, c *f1client.Client, ns, ref, path string) string {
	t.Helper()
	rc, _, err := c.Get(ctx, ns, ref, path, nil)
	if err != nil {
		t.Fatalf("get %s: %v", path, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
