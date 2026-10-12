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
	"syscall"

	"github.com/trukeio/f1/internal/config"
	"github.com/trukeio/f1/internal/logging"
	"github.com/trukeio/f1/internal/server"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `Usage: f1d <command> [flags]

Commands:
  serve     run the server
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
