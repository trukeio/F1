// Command f1web is the web viewer of an F1 installation (ADR-0018).
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

	"github.com/trukeio/f1/internal/web"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// run executes f1web with args and returns the process exit code.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("f1web", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "f1web.toml", "path to the configuration file")
	showVersion := fs.Bool("version", false, "print the version")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *showVersion {
		fmt.Fprintf(stdout, "f1web %s\n", version)
		return 0
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "f1web: unexpected arguments: %v\n", fs.Args())
		return 2
	}
	cfg, err := web.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "f1web: %v\n", err)
		return 1
	}
	log := web.NewLogger(stderr, cfg.Log)
	log.Info("starting f1web", "version", version, "config", *configPath)
	if err := web.Run(ctx, cfg, log, nil); err != nil {
		log.Error("f1web failed", "err", err)
		return 1
	}
	return 0
}
