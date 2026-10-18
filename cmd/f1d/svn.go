package main

import (
	"context"
	"fmt"
	"io"
	"net/mail"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/trukeio/f1/internal/config"
	"github.com/trukeio/f1/internal/repo"
	"github.com/trukeio/f1/internal/server"
	"github.com/trukeio/f1/internal/svnimport"
)

// svnFlags are the flags of "f1d repo import-svn".
type svnFlags struct {
	authors string
	layout  svnimport.Layout
	create  string
}

// importSVN imports a Subversion dump into ns (ADR-0019), through the
// running server's admin socket or, with no server, directly.
func importSVN(ctx context.Context, cfg config.Config, admin *server.AdminClient, running bool, ns, src string, f svnFlags, stdout, stderr io.Writer) error {
	authors, err := readAuthors(f.authors)
	if err != nil {
		return err
	}
	var dump io.Reader = os.Stdin
	if src != "-" {
		file, err := os.Open(src)
		if err != nil {
			return err
		}
		defer file.Close()
		dump = file
	}
	if f.create != "" {
		if running {
			err = admin.Create(ctx, ns, f.create)
		} else {
			err = createOffline(ctx, cfg, ns, f.create)
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s created\n", ns)
	}
	last := time.Now()
	progress := func(rev int) {
		if time.Since(last) >= time.Second {
			last = time.Now()
			fmt.Fprintf(stderr, "r%d\n", rev)
		}
	}
	var rep *svnimport.Report
	if running {
		rep, err = admin.ImportSVN(ctx, ns, server.SVNImportRequest{Layout: f.layout, Authors: authors}, dump, func(rev int) {
			fmt.Fprintf(stderr, "r%d\n", rev)
		})
	} else {
		rep, err = importSVNOffline(ctx, filepath.Join(cfg.Server.DataDir, server.ReposDir, ns), dump,
			svnimport.Options{Layout: f.layout, Authors: authors, Progress: progress})
	}
	if rep != nil && rep.Revisions > 0 {
		printSVNReport(stdout, ns, rep)
	}
	return err
}

func importSVNOffline(ctx context.Context, path string, dump io.Reader, opts svnimport.Options) (*svnimport.Report, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("no namespace at %s (create it, or pass --create)", path)
	}
	rp, _, err := repo.Open(ctx, path, repo.Options{})
	if err != nil {
		return nil, err
	}
	rep, err := svnimport.Import(ctx, rp, dump, opts)
	if cerr := rp.Close(ctx); err == nil {
		err = cerr
	}
	return rep, err
}

// readAuthors reads an authors file: SVN user names mapped to
// "Name <email>".
func readAuthors(file string) (map[string]repo.Person, error) {
	if file == "" {
		return nil, nil
	}
	var raw map[string]string
	if _, err := toml.DecodeFile(file, &raw); err != nil {
		return nil, fmt.Errorf("authors file: %w", err)
	}
	out := make(map[string]repo.Person, len(raw))
	for user, v := range raw {
		a, err := mail.ParseAddress(v)
		if err != nil || a.Name == "" {
			return nil, fmt.Errorf("authors file: %s = %q is not \"Name <email>\"", user, v)
		}
		out[user] = repo.Person{Name: a.Name, Email: a.Address}
	}
	return out, nil
}

func printSVNReport(w io.Writer, ns string, rep *svnimport.Report) {
	fmt.Fprintf(w, "%s: imported to r%d: %d revisions, %d commits, %d bytes of content\n",
		ns, rep.LastRevision, rep.Revisions, rep.Commits, rep.Bytes)
	paths := make([]string, 0, len(rep.Refs))
	for p := range rep.Refs {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		name := p
		if name == "" {
			name = "/"
		}
		fmt.Fprintf(w, "  %s → %s\n", name, rep.Refs[p])
	}
	list := func(title string, items []string) {
		if len(items) > 0 {
			fmt.Fprintf(w, "%s: %s\n", title, strings.Join(items, ", "))
		}
	}
	list("deleted branches and tags, kept as", rep.Deleted)
	list("tags changed after their creation", rep.TagChanges)
	list("authors not in the authors file", rep.UnknownAuthors)
	if len(rep.NeedsLock) > 0 {
		fmt.Fprintf(w, "files with svn:needs-lock; suggested f1repo.toml setting: locks = \"required:%s\"\n", strings.Join(rep.NeedsLock, ","))
	}
	list("svn:externals (not imported) on", rep.Externals)
	list("svn:ignore (not imported) on", rep.Ignores)
	if len(rep.DroppedProps) > 0 {
		var ds []string
		for k, n := range rep.DroppedProps {
			ds = append(ds, fmt.Sprintf("%s (%d)", k, n))
		}
		sort.Strings(ds)
		list("properties not kept", ds)
	}
}
