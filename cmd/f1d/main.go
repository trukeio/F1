// Command f1d is the F1 server and admin CLI.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/trukeio/f1/internal/config"
	"github.com/trukeio/f1/internal/gc"
	"github.com/trukeio/f1/internal/logging"
	"github.com/trukeio/f1/internal/server"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `Usage: f1d <command> [flags]

Commands:
  serve     run the server
  fsck      check a repository (offline; refuses while a server has it open)
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
