// Command f1d is the F1 server and admin CLI.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/trukeio/f1/internal/config"
	"github.com/trukeio/f1/internal/gc"
	"github.com/trukeio/f1/internal/logging"
	"github.com/trukeio/f1/internal/repo"
	"github.com/trukeio/f1/internal/server"
	"github.com/trukeio/f1/internal/serverdb"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `Usage: f1d <command> [flags]

Commands:
  serve     run the server
  fsck      check a repository (offline; refuses while a server has it open)
  gc        collect garbage in a repository (through a running server, or offline)
  backup    back up server.db and every repository to a directory (incremental)
  repo      manage repositories: list, freeze, archive, import, attach, ...
  ns        create namespaces
  user      manage local users
  token     manage local API tokens
  s3key     create S3 access keys
  acl       manage ACLs
  version   print the version

Run "f1d <command> -h" for command flags.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// run executes the command in args and returns the process exit code.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "serve":
		return serve(ctx, args[1:], stderr)
	case "fsck":
		return fsck(ctx, args[1:], stdout, stderr)
	case "gc":
		return gcCmd(ctx, args[1:], stdout, stderr)
	case "backup":
		return backupCmd(ctx, args[1:], stdout, stderr)
	case "repo":
		return repoCmd(ctx, args[1:], stdout, stderr)
	case "ns", "user", "token", "s3key", "acl":
		return identityCmd(ctx, args[0], args[1:], stdout, stderr)
	case "version":
		fmt.Fprintf(stdout, "f1d %s\n", version)
		return 0
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "f1d: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

func serve(ctx context.Context, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "f1d.toml", "path to the configuration file")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "f1d serve: unexpected arguments: %v\n", fs.Args())
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "f1d serve: %v\n", err)
		return 1
	}
	log, err := logging.New(stderr, cfg.Log)
	if err != nil {
		fmt.Fprintf(stderr, "f1d serve: %v\n", err)
		return 1
	}
	log.Info("starting f1d", "version", version, "config", *configPath)
	if err := server.Run(ctx, cfg, log, nil); err != nil {
		log.Error("server failed", "err", err)
		return 1
	}
	return 0
}

func fsck(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fsck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, "Usage: f1d fsck [--config f1d.toml] <namespace>\n       f1d fsck --dir <repository directory>\n")
		fs.PrintDefaults()
	}
	configPath := fs.String("config", "f1d.toml", "path to the configuration file")
	dir := fs.String("dir", "", "check this repository directory instead of a namespace")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	path := *dir
	switch {
	case path != "" && fs.NArg() == 0:
	case path == "" && fs.NArg() == 1:
		cfg, err := config.Load(*configPath)
		if err != nil {
			fmt.Fprintf(stderr, "f1d fsck: %v\n", err)
			return 1
		}
		path = filepath.Join(cfg.Server.DataDir, server.ReposDir, fs.Arg(0))
	default:
		fs.Usage()
		return 2
	}
	rep, err := gc.CheckDir(ctx, path)
	if err != nil {
		fmt.Fprintf(stderr, "f1d fsck: %v\n", err)
		return 1
	}
	for _, f := range rep.Findings {
		fmt.Fprintln(stdout, f)
	}
	fmt.Fprintf(stdout, "%d packs, %d objects, %d refs, %d reachable objects\n", rep.Packs, rep.Objects, rep.Refs, rep.Reachable)
	if !rep.OK() {
		fmt.Fprintln(stdout, "fsck: errors found")
		return 1
	}
	if rep.Clean() {
		fmt.Fprintln(stdout, "fsck: ok")
	} else {
		fmt.Fprintln(stdout, "fsck: ok (warnings are repaired when the repository is opened)")
	}
	return 0
}

func gcCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, "Usage: f1d gc [--config f1d.toml] [--dry-run] [--grace 24h] [--list] <namespace>\n"+
			"       f1d gc [--dry-run] [--grace 24h] [--list] --dir <repository directory>\n")
		fs.PrintDefaults()
	}
	configPath := fs.String("config", "f1d.toml", "path to the configuration file")
	dir := fs.String("dir", "", "collect in this repository directory (offline) instead of a namespace")
	dryRun := fs.Bool("dry-run", false, "report what would be deleted without deleting it")
	grace := fs.String("grace", "", "keep unreachable objects written this recently (default: server.gc.grace)")
	list := fs.Bool("list", false, "list the hashes of the deleted objects")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if (*dir == "") == (fs.NArg() != 1) || fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "f1d gc: %v\n", err)
		return 1
	}
	var res *gc.Result
	if *dir == "" {
		cfg, err := config.Load(*configPath)
		if err != nil {
			return fail(err)
		}
		if *grace == "" {
			*grace = cfg.Server.GC.Grace
		}
		if c, ok := server.DialAdmin(cfg.Server.DataDir); ok {
			if res, err = c.GC(ctx, fs.Arg(0), server.GCRequest{DryRun: *dryRun, Grace: *grace, List: *list}); err != nil {
				return fail(err)
			}
		} else {
			*dir = filepath.Join(cfg.Server.DataDir, server.ReposDir, fs.Arg(0))
		}
	}
	if res == nil {
		// Offline: open the repository directly (refused while a server
		// has it open).
		if *grace == "" {
			*grace = gc.DefaultGrace.String()
		}
		d, err := time.ParseDuration(*grace)
		if err != nil {
			return fail(fmt.Errorf("--grace: %w", err))
		}
		if d == 0 {
			d = -1
		}
		r, _, err := repo.Open(ctx, *dir, repo.Options{})
		if err != nil {
			return fail(err)
		}
		res, err = gc.Run(ctx, r, gc.Options{Grace: d, DryRun: *dryRun})
		if cerr := r.Close(ctx); err == nil {
			err = cerr
		}
		if err != nil {
			return fail(err)
		}
		if *list {
			res.List = res.Deleted
		}
	}
	if res.Skipped != "" {
		fmt.Fprintf(stdout, "skipped: %s\n", res.Skipped)
		return 0
	}
	for _, h := range res.List {
		fmt.Fprintln(stdout, h)
	}
	verb := "deleted"
	if res.DryRun {
		verb = "would delete"
	}
	fmt.Fprintf(stdout, "%s %d objects (%d bytes): %d packs deleted, %d rewritten; %d unreachable objects kept in mostly live packs; %d retained commits, %d live objects (%s)\n",
		verb, res.DeletedObjects, res.DeletedBytes, len(res.PacksDeleted), len(res.PacksRewritten), res.Kept, res.Retained, res.Marked, res.Duration)
	return 0
}

func backupCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, "Usage: f1d backup [--config f1d.toml] <directory>\n"+
			"Writes <directory>/server.db and <directory>/repos/<ns>/; run it again to update the backup.\n"+
			"Restore: f1d repo import <directory>/repos/<ns> for each repository, with the backed up server.db.\n")
		fs.PrintDefaults()
	}
	configPath := fs.String("config", "f1d.toml", "path to the configuration file")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "f1d backup: %v\n", err)
		return 1
	}
	dst, err := filepath.Abs(fs.Arg(0))
	if err != nil {
		return fail(err)
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return fail(err)
	}
	var rep server.BackupReport
	if c, ok := server.DialAdmin(cfg.Server.DataDir); ok {
		if rep, err = c.Backup(ctx, dst); err != nil {
			return fail(err)
		}
	} else {
		// Offline: open server.db and the repositories directly.
		db, err := serverdb.Open(ctx, filepath.Join(cfg.Server.DataDir, server.ServerDB))
		if err != nil {
			return fail(err)
		}
		defer db.Close()
		mgr, err := server.OpenManager(ctx, cfg.Server.DataDir, db, slog.New(slog.DiscardHandler), repo.Options{})
		if err != nil {
			return fail(err)
		}
		rep, err = mgr.Backup(ctx, dst)
		if cerr := mgr.Close(ctx); err == nil {
			err = cerr
		}
		if err != nil {
			return fail(err)
		}
	}
	names := make([]string, 0, len(rep.Repos))
	for ns := range rep.Repos {
		names = append(names, ns)
	}
	sort.Strings(names)
	for _, ns := range names {
		i := rep.Repos[ns]
		fmt.Fprintf(stdout, "%s: %d packs, %d copied (%d bytes), %d removed\n", ns, i.Packs, i.Copied, i.Bytes, i.Removed)
	}
	fmt.Fprintf(stdout, "backup of %d repositories in %s\n", len(rep.Repos), dst)
	return 0
}
