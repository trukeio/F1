// Command f1 is the F1 native client: working copies with sparse
// checkouts, local commits, pull with merge, push, and locks (doc/plan.md,
// "Native client"; ADR-0010).
//
// The bearer token is read from the F1_TOKEN environment variable; it is
// never stored in the working copy.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/trukeio/f1/internal/client"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `Usage: f1 <command> [flags] [args]

Commands:
  clone <server-url> <namespace> [dir]   create a working copy
        --ref <branch>     branch to follow (default: the namespace's default)
        --sparse <dir>     check out only this directory (repeatable)
  sparse list|add <dir>|remove <dir>     change the sparse checkout
  status                                 show changed files
  add <path>...                          add files; mark conflicts resolved
  rm <path>...                           delete files; resolve as deleted
  commit -m <message>                    record a local commit
  push                                   send local commits to the server
  pull                                   merge the server's new commits
  log [--limit n] [path]                 show history
  diff [path]...                         show uncommitted changes
  lock <path>...                         lock files
  unlock [--force] <path>...             unlock files
  locks                                  list locks
  checkout <branch|commit>               switch the working copy
  branch [name]                          list branches, or create one at the base commit
  version                                print the version

The token is read from F1_TOKEN.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// run executes the command in args and returns the process exit code.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd, args := args[0], args[1:]
	if cmd == "version" {
		fmt.Fprintln(stdout, "f1", version)
		return 0
	}
	opts := client.Options{Token: os.Getenv("F1_TOKEN"), Out: stdout}
	err := dispatch(ctx, cmd, args, opts, stdout)
	var ue usageError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ue):
		fmt.Fprintf(stderr, "f1 %s: %v\n", cmd, err)
		return 2
	case errors.Is(err, errConflicts):
		return 1
	default:
		fmt.Fprintf(stderr, "f1 %s: %v\n", cmd, err)
		return 1
	}
}

type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

var errConflicts = errors.New("conflicts")

// parse parses flags that may appear before, between or after the
// positional arguments, and returns the positional arguments.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, usageError{err.Error()}
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		if args[0] == "--" {
			return append(pos, args[1:]...), nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func dispatch(ctx context.Context, cmd string, args []string, opts client.Options, out io.Writer) error {
	if cmd == "clone" {
		return clone(ctx, args, opts, out)
	}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	message := fs.String("m", "", "commit message")
	limit := fs.Int("limit", 20, "number of commits")
	force := fs.Bool("force", false, "remove other principals' locks")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	wc, err := client.Open(".", opts)
	if err != nil {
		return err
	}
	defer wc.Close()
	paths := func(min int) ([]string, error) {
		if len(pos) < min {
			return nil, usageError{"missing path"}
		}
		out := make([]string, len(pos))
		for i, p := range pos {
			if out[i], err = wc.Rel(p); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	switch cmd {
	case "status":
		list, err := wc.Status(ctx)
		if err != nil {
			return err
		}
		for _, s := range list {
			fmt.Fprintf(out, "%c %s\n", s.Code, s.Path)
		}
		if n := len(wc.State.Commits); n > 0 {
			fmt.Fprintf(out, "%d local commit(s) not pushed\n", n)
		}
		return nil
	case "add":
		ps, err := paths(1)
		if err != nil {
			return err
		}
		return wc.Add(ctx, ps)
	case "rm":
		ps, err := paths(1)
		if err != nil {
			return err
		}
		return wc.Remove(ctx, ps)
	case "commit":
		if *message == "" {
			if wc.State.Message == "" {
				return usageError{"-m <message> is required"}
			}
			*message = wc.State.Message
		}
		c, err := wc.Commit(ctx, *message)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "committed %d change(s) locally; f1 push sends them\n", len(c.Changes))
		return nil
	case "push":
		n, err := wc.Push(ctx)
		if err == nil && n == 0 {
			fmt.Fprintln(out, "nothing to push")
		}
		return err
	case "pull":
		res, err := wc.Pull(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%d file(s) updated", len(res.Updated))
		if res.Rebased+res.Dropped > 0 {
			fmt.Fprintf(out, ", %d local commit(s) rebased", res.Rebased)
		}
		fmt.Fprintln(out)
		if res.Folded > 0 {
			fmt.Fprintf(out, "%d local commit(s) conflicted and are working changes again; their messages are kept for f1 commit\n", res.Folded)
		}
		if len(res.Conflicts) > 0 {
			for _, p := range res.Conflicts {
				fmt.Fprintf(out, "C %s\n", p)
			}
			fmt.Fprintln(out, "resolve the conflicts, then f1 add (or f1 rm) each path")
			return errConflicts
		}
		return nil
	case "log":
		var path string
		if len(pos) > 0 {
			ps, err := paths(1)
			if err != nil {
				return err
			}
			path = ps[0]
		}
		list, err := wc.Log(ctx, path, *limit)
		if err != nil {
			return err
		}
		for _, e := range list {
			id := "local " + e.LocalID
			if e.Hash != "" {
				id = e.Hash[:12] + " " + e.Author
			}
			fmt.Fprintf(out, "%s %s %s\n", id, e.Time.Format("2006-01-02 15:04"), firstLine(e.Message))
		}
		return nil
	case "diff":
		ps, err := paths(0)
		if err != nil {
			return err
		}
		return wc.Diff(ctx, ps, out)
	case "lock":
		ps, err := paths(1)
		if err != nil {
			return err
		}
		return wc.Lock(ctx, ps)
	case "unlock":
		ps, err := paths(1)
		if err != nil {
			return err
		}
		return wc.Unlock(ctx, ps, *force)
	case "locks":
		list, err := wc.Locks(ctx)
		if err != nil {
			return err
		}
		for _, l := range list {
			fmt.Fprintf(out, "%s %s %s\n", l.Path, l.Owner, l.Created.Format("2006-01-02 15:04"))
		}
		return nil
	case "checkout":
		if len(pos) != 1 {
			return usageError{"usage: f1 checkout <branch|commit>"}
		}
		return wc.Checkout(ctx, pos[0])
	case "branch":
		if len(pos) == 1 {
			return wc.CreateBranch(ctx, pos[0])
		}
		list, err := wc.Branches(ctx)
		if err != nil {
			return err
		}
		for _, b := range list {
			mark := " "
			if b == wc.Config.Ref {
				mark = "*"
			}
			fmt.Fprintf(out, "%s %s\n", mark, b)
		}
		return nil
	case "sparse":
		if len(pos) == 0 || pos[0] == "list" {
			for _, s := range wc.Config.Sparse {
				fmt.Fprintln(out, s)
			}
			return nil
		}
		if len(pos) != 2 {
			return usageError{"usage: f1 sparse list|add <dir>|remove <dir>"}
		}
		dir, err := wc.Rel(pos[1])
		if err != nil {
			return err
		}
		switch pos[0] {
		case "add":
			return wc.SparseAdd(ctx, dir)
		case "remove":
			return wc.SparseRemove(ctx, dir)
		}
		return usageError{"usage: f1 sparse list|add <dir>|remove <dir>"}
	}
	return usageError{fmt.Sprintf("unknown command %q", cmd)}
}

func clone(ctx context.Context, args []string, opts client.Options, out io.Writer) error {
	fs := flag.NewFlagSet("clone", flag.ContinueOnError)
	ref := fs.String("ref", "", "branch to follow")
	var sparse multi
	fs.Var(&sparse, "sparse", "directory to check out (repeatable)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 2 || len(pos) > 3 {
		return usageError{"usage: f1 clone [--ref branch] [--sparse dir]... <server-url> <namespace> [dir]"}
	}
	dir := pos[1]
	if len(pos) == 3 {
		dir = pos[2]
	}
	wc, err := client.Clone(ctx, pos[0], pos[1], dir, *ref, sparse, opts)
	if err != nil {
		return err
	}
	defer wc.Close()
	fmt.Fprintf(out, "cloned %s (%s) into %s: %d file(s)\n", pos[1], wc.Config.Ref, dir, len(wc.Index))
	return nil
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
