package web

import (
	"archive/zip"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/trukeio/f1/pkg/f1client"
)

// codeMenu is the Code button of a namespace's root directory: the ways
// to clone it, and a ZIP of its files at the ref shown.
type codeMenu struct {
	Clones []cloneWay
	ZIP    string
}

// cloneWay is one tab of the Code menu.
type cloneWay struct {
	ID, Name string
	Command  string
	Note     string
}

// codeMenu returns the Code menu of a namespace at v.Ref.
func (s *Server) codeMenu(v *repoView) *codeMenu {
	ns := v.NS.Name
	m := &codeMenu{ZIP: "/" + ns + "/archive/" + url.PathEscape(v.Ref) + ".zip"}
	if v.NS.Config.Git {
		m.Clones = append(m.Clones, cloneWay{ID: "git", Name: "Git",
			Command: "git clone " + s.cfg.CloneURL + "/git/" + ns + ".git",
			Note:    "Use an F1 token as the password; the user name is ignored."})
	}
	if v.NS.Config.Layout == "tree" {
		cmd := "f1 clone "
		if branch := v.Ref; !isHash(branch) && !strings.HasPrefix(branch, "tags/") && branch != shortRef(v.NS.Config.DefaultRef) {
			cmd += "--ref " + shellQuote(branch) + " "
		}
		m.Clones = append(m.Clones, cloneWay{ID: "f1", Name: "F1",
			Command: cmd + s.cfg.CloneURL + " " + ns,
			Note:    "The native client, for large files, locks and sparse checkouts. Set F1_TOKEN to an F1 token first."})
	}
	if s.cfg.S3URL != "" {
		m.Clones = append(m.Clones, cloneWay{ID: "s3", Name: "S3",
			Command: "aws --endpoint-url " + s.cfg.S3URL + " s3 sync s3://" + ns + " " + ns,
			Note:    "Copies the default branch. Needs an S3 access key."})
	}
	return m
}

// shellQuote quotes s for a POSIX shell if it needs it.
func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./+@%:,=", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// archivePage is the number of paths listed per call while writing a ZIP.
const archivePage = 1000

// archive streams the files of a namespace at a ref as a ZIP, below a
// directory named after the namespace and the ref, as GitHub's "Download
// ZIP" does. It lists the ref's commit once and reads every file at that
// commit, so the ZIP is consistent while writes continue. Only the files
// the user can read are listed (D13). Git submodules are left out.
func (s *Server) archive(w http.ResponseWriter, r *http.Request, rq *request) error {
	ctx := r.Context()
	ref, ok := strings.CutSuffix(r.PathValue("file"), ".zip")
	if !ok || ref == "" {
		return errNotFound
	}
	v, err := s.loadRepo(ctx, rq.api, r.PathValue("ns"), ref, "", "code")
	if err != nil {
		return err
	}
	ns := v.NS.Name
	list, err := rq.api.List(ctx, ns, ref, "", &f1client.ListOptions{Limit: archivePage})
	if err != nil {
		return err
	}
	commit := list.Commit
	modified := time.Now()
	if v.History() {
		// Without whole-namespace read, the root's history is refused
		// (D13); the files then get the current time.
		if log, err := rq.api.Log(ctx, ns, commit, "", 1); err == nil && len(log) == 1 {
			modified = time.Unix(log[0].Committer.Time, 0)
		}
	}
	root := ns + "-" + strings.ReplaceAll(ref, "/", "-")
	h := w.Header()
	h.Set("Content-Type", "application/zip")
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": root + ".zip"}))
	h.Set("Cache-Control", "no-store")
	zw := zip.NewWriter(w)
	for {
		for _, it := range list.Items {
			if it.Prefix || it.Kind == "gitlink" {
				continue
			}
			if err := s.archiveFile(r, rq, zw, ns, commit, root, modified, it); err != nil {
				// Headers are sent; the browser gets a truncated ZIP.
				s.log.Info("ZIP download interrupted", "path", r.URL.Path, "file", it.Path, "err", err)
				return nil
			}
		}
		if list.Next == "" {
			break
		}
		if list, err = rq.api.List(ctx, ns, commit, "", &f1client.ListOptions{After: list.Next, Limit: archivePage}); err != nil {
			s.log.Info("ZIP download interrupted", "path", r.URL.Path, "err", err)
			return nil
		}
	}
	if err := zw.Close(); err != nil {
		s.log.Info("ZIP download interrupted", "path", r.URL.Path, "err", err)
	}
	return nil
}

// archiveFile adds one file to a ZIP.
func (s *Server) archiveFile(r *http.Request, rq *request, zw *zip.Writer, ns, commit, root string, modified time.Time, it f1client.ListItem) error {
	fh := &zip.FileHeader{Name: path.Join(root, it.Path), Method: zip.Deflate, Modified: modified}
	switch it.Kind {
	case "exec":
		fh.SetMode(0o755)
	case "symlink":
		fh.SetMode(fs.ModeSymlink | 0o777)
	default:
		fh.SetMode(0o644)
	}
	resp, err := rq.api.Open(r.Context(), ns, commit, it.Path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	fw, err := zw.CreateHeader(fh)
	if err != nil {
		return err
	}
	_, err = io.Copy(fw, resp.Body)
	return err
}
