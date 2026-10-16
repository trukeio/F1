package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/trukeio/f1/internal/cache"
	"github.com/trukeio/f1/internal/config"
	"github.com/trukeio/f1/internal/repo"
	"github.com/trukeio/f1/internal/repodir"
	"github.com/trukeio/f1/internal/server"
	"github.com/trukeio/f1/internal/serverdb"
	"github.com/trukeio/f1/internal/store"
)

const repoUsage = `Usage: f1d repo <command> [--config f1d.toml] [flags] [args]

Commands:
  list                             list repositories
  freeze <ns>                      make a repository read-only for copying (running server)
  unfreeze <ns>                    make a frozen repository writable again (running server)
  archive <ns> <file.zip>          write a repository archive; writes may continue
  import <file.zip|dir>            import a repository archive or directory
         [--name <ns>] [--new-uuid]
  attach <ns>                      register the directory repos/<ns>
  detach <ns>                      unregister a repository and leave its directory ready to move
  rebuild-cache <ns> | --dir <dir> rebuild cache.db (offline)
  export-acl <ns> [<file.toml>]    write the namespace's ACL as TOML (default: stdout)
  import-acl <ns> <file.toml>      apply an exported ACL; unknown users are skipped

With a running server, commands go through its admin socket; otherwise
they work on the data directory directly.
`

func repoCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, repoUsage)
		return 2
	}
	cmd := args[0]
	fs := flag.NewFlagSet("repo "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, repoUsage) }
	configPath := fs.String("config", "f1d.toml", "path to the configuration file")
	name := fs.String("name", "", "import: namespace name (default: archive root or directory name)")
	newUUID := fs.Bool("new-uuid", false, "import: give the repository a new UUID")
	dir := fs.String("dir", "", "rebuild-cache: repository directory")
	if err := parseFlags(fs, args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	nargs := map[string]int{"list": 0, "freeze": 1, "unfreeze": 1, "archive": 2, "import": 1, "attach": 1, "detach": 1, "rebuild-cache": -1,
		"export-acl": -1, "import-acl": 2}
	want, ok := nargs[cmd]
	if !ok {
		fmt.Fprintf(stderr, "f1d repo: unknown command %q\n\n%s", cmd, repoUsage)
		return 2
	}
	if want >= 0 && fs.NArg() != want {
		fs.Usage()
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "f1d repo %s: %v\n", cmd, err)
		return 1
	}

	if cmd == "rebuild-cache" {
		path := *dir
		switch {
		case path != "" && fs.NArg() == 0:
		case path == "" && fs.NArg() == 1:
			cfg, err := config.Load(*configPath)
			if err != nil {
				return fail(err)
			}
			path = filepath.Join(cfg.Server.DataDir, server.ReposDir, fs.Arg(0))
		default:
			fs.Usage()
			return 2
		}
		if err := rebuildCache(ctx, path, stdout); err != nil {
			return fail(err)
		}
		return 0
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fail(err)
	}
	dataDir := cfg.Server.DataDir
	reposDir := filepath.Join(dataDir, server.ReposDir)
	admin, running := server.DialAdmin(dataDir)
	offlineDB := func() (*serverdb.DB, error) {
		return serverdb.Open(ctx, filepath.Join(dataDir, server.ServerDB))
	}

	switch cmd {
	case "export-acl", "import-acl":
		if cmd == "export-acl" && (fs.NArg() < 1 || fs.NArg() > 2) {
			fs.Usage()
			return 2
		}
		db, err := offlineDB()
		if err != nil {
			return fail(err)
		}
		defer db.Close()
		if cmd == "import-acl" {
			data, err := os.ReadFile(fs.Arg(1))
			if err != nil {
				return fail(err)
			}
			n, err := importACL(ctx, db, fs.Arg(0), data, stdout)
			if err != nil {
				return fail(err)
			}
			fmt.Fprintf(stdout, "%s: imported %d ACL entries\n", fs.Arg(0), n)
			return 0
		}
		out := stdout
		if fs.NArg() == 2 {
			f, err := os.Create(fs.Arg(1))
			if err != nil {
				return fail(err)
			}
			defer f.Close()
			out = f
		}
		if _, err := exportACL(ctx, db, fs.Arg(0), out); err != nil {
			return fail(err)
		}
		return 0
	case "list":
		if running {
			repos, err := admin.List(ctx)
			if err != nil {
				return fail(err)
			}
			for _, r := range repos {
				state := ""
				if r.Frozen {
					state = " (frozen)"
				}
				fmt.Fprintf(stdout, "%s\t%s%s\n", r.Name, r.UUID, state)
			}
			return 0
		}
		db, err := offlineDB()
		if err != nil {
			return fail(err)
		}
		defer db.Close()
		repos, err := db.Repositories(ctx)
		if err != nil {
			return fail(err)
		}
		for _, r := range repos {
			fmt.Fprintf(stdout, "%s\t%s\n", r.Name, r.UUID)
		}
		return 0

	case "freeze", "unfreeze":
		if !running {
			return fail(errors.New("no server is running; the repositories of a stopped server can be copied directly"))
		}
		if err := admin.Do(ctx, cmd, fs.Arg(0)); err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "%s: %s\n", fs.Arg(0), map[string]string{"freeze": "frozen", "unfreeze": "unfrozen"}[cmd])
		return 0

	case "archive":
		ns := fs.Arg(0)
		out, err := filepath.Abs(fs.Arg(1))
		if err != nil {
			return fail(err)
		}
		var info repo.ArchiveInfo
		if running {
			info, err = admin.Archive(ctx, ns, out)
		} else {
			info, err = archiveOffline(ctx, filepath.Join(reposDir, ns), ns, out)
		}
		if err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "%s: archived %d refs, %d packs (%d bytes) to %s\n", ns, len(info.Refs), info.Packs, info.Bytes, out)
		return 0

	case "import":
		res, err := server.ImportRepo(ctx, reposDir, fs.Arg(0), server.ImportOptions{Name: *name, NewUUID: *newUUID})
		if err != nil {
			return fail(err)
		}
		for _, s := range res.Skipped {
			fmt.Fprintf(stdout, "skipped %s (not repository state)\n", s)
		}
		if running {
			err = admin.Do(ctx, "attach", res.Name)
		} else {
			err = registerOffline(ctx, offlineDB, res.Name, res.UUID)
		}
		if err != nil {
			return fail(fmt.Errorf("imported into %s but could not register it: %w", filepath.Join(reposDir, res.Name), err))
		}
		fmt.Fprintf(stdout, "%s: imported %d files, uuid %s, fsck ok\n", res.Name, res.Files, res.UUID)
		return 0

	case "attach":
		ns := fs.Arg(0)
		if running {
			err = admin.Do(ctx, "attach", ns)
		} else {
			var meta repodir.Meta
			if meta, err = repodir.ReadMeta(filepath.Join(reposDir, ns)); err == nil {
				err = registerOffline(ctx, offlineDB, ns, meta.UUID)
			}
		}
		if err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "%s: attached\n", ns)
		return 0

	case "detach":
		ns := fs.Arg(0)
		if running {
			err = admin.Do(ctx, "detach", ns)
		} else {
			var db *serverdb.DB
			if db, err = offlineDB(); err == nil {
				err = server.DetachOffline(ctx, db, reposDir, ns)
				db.Close()
			}
		}
		if err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "%s: detached; %s can be moved\n", ns, filepath.Join(reposDir, ns))
		return 0
	}
	return 2
}

func registerOffline(ctx context.Context, open func() (*serverdb.DB, error), ns, uuid string) error {
	db, err := open()
	if err != nil {
		return err
	}
	defer db.Close()
	return db.Register(ctx, ns, uuid)
}

// archiveOffline archives a repository that no server has open.
func archiveOffline(ctx context.Context, path, root, out string) (repo.ArchiveInfo, error) {
	r, _, err := repo.Open(ctx, path, repo.Options{})
	if err != nil {
		return repo.ArchiveInfo{}, err
	}
	defer r.Close(ctx)
	return server.ArchiveToFile(ctx, out, func(w io.Writer) (repo.ArchiveInfo, error) {
		return r.Archive(ctx, w, root)
	})
}

// rebuildCache deletes and rebuilds cache.db of a repository that no
// server has open.
func rebuildCache(ctx context.Context, path string, stdout io.Writer) error {
	d, err := repodir.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := removeCache(d); err != nil {
		return err
	}
	s, rep, err := store.Open(ctx, d, store.Options{})
	if err != nil {
		return err
	}
	if err := s.Close(ctx); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "cache.db rebuilt from %d packs\n", len(rep.Indexed))
	return nil
}

func removeCache(d *repodir.Dir) error { return cache.Remove(d.Join(repodir.CacheFile)) }
