package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/trukeio/f1/internal/gc"
	"github.com/trukeio/f1/internal/policy"
	"github.com/trukeio/f1/internal/repo"
)

// AdminSocket is the name of the admin socket in the data directory.
const AdminSocket = "admin.sock"

// ErrServerRunning is returned when another server owns the data directory.
var ErrServerRunning = errors.New("server: another server is running on this data directory")

// listenAdmin listens on the admin socket with mode 0600. A stale socket
// from a crashed server is removed; a live one means another server owns
// the data directory.
func listenAdmin(path string) (net.Listener, error) {
	if _, err := os.Stat(path); err == nil {
		if c, err := dialUnix(context.Background(), path, time.Second); err == nil {
			c.Close()
			return nil, fmt.Errorf("%w (%s)", ErrServerRunning, path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("server: remove stale admin socket: %w", err)
		}
	}
	var ln net.Listener
	err := shortSocketPath(path, func(p string) error {
		old := syscall.Umask(0o077)
		defer syscall.Umask(old)
		var err error
		ln, err = net.Listen("unix", p)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("server: admin socket: %w", err)
	}
	// The name it was bound with may be gone (a temporary symlink); the
	// server removes the socket by its real path.
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("server: admin socket: %w", err)
	}
	return ln, nil
}

// maxSocketPath is the longest socket path used as is: a socket address
// holds 104 bytes on macOS and the BSDs and 108 on Linux, including the
// terminating NUL.
const maxSocketPath = 103

// shortSocketPath calls fn with a name for the socket at path that fits in
// a socket address. A longer path is reached through a symlink to its
// directory, in a new private temporary directory that is removed when fn
// returns. The socket itself stays at path.
func shortSocketPath(path string, fn func(string) error) error {
	if len(path) <= maxSocketPath {
		return fn(path)
	}
	dir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "f1d-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	link := filepath.Join(tmp, "d")
	if err := os.Symlink(dir, link); err != nil {
		return err
	}
	short := filepath.Join(link, filepath.Base(path))
	if len(short) > maxSocketPath {
		return fmt.Errorf("socket path %s is too long, and so is the temporary directory %s (set TMPDIR)", path, tmp)
	}
	return fn(short)
}

// dialUnix connects to the socket at path, however long the path.
func dialUnix(ctx context.Context, path string, timeout time.Duration) (net.Conn, error) {
	var c net.Conn
	err := shortSocketPath(path, func(p string) error {
		d := net.Dialer{Timeout: timeout}
		var err error
		c, err = d.DialContext(ctx, "unix", p)
		return err
	})
	return c, err
}

// adminHandler serves the admin API used by "f1d repo ..." on a running
// server. Every request is local and authenticated by the socket's file
// permissions.
func adminHandler(m *Manager, gcGrace time.Duration, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, v any, err error) {
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			status := http.StatusInternalServerError
			switch {
			case errors.Is(err, ErrNoRepo):
				status = http.StatusNotFound
			case errors.Is(err, ErrRepoExists):
				status = http.StatusConflict
			}
			w.WriteHeader(status)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		if v == nil {
			v = map[string]string{"status": "ok"}
		}
		json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("GET /repos", func(w http.ResponseWriter, r *http.Request) {
		reply(w, m.List(), nil)
	})
	mux.HandleFunc("POST /repos/{ns}/freeze", func(w http.ResponseWriter, r *http.Request) {
		reply(w, nil, m.Freeze(r.Context(), r.PathValue("ns")))
	})
	mux.HandleFunc("POST /repos/{ns}/unfreeze", func(w http.ResponseWriter, r *http.Request) {
		reply(w, nil, m.Unfreeze(r.PathValue("ns")))
	})
	mux.HandleFunc("POST /repos/{ns}/attach", func(w http.ResponseWriter, r *http.Request) {
		reply(w, nil, m.Attach(r.Context(), r.PathValue("ns")))
	})
	mux.HandleFunc("POST /repos/{ns}/create", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Preset string `json:"preset"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		cfg := policy.Default()
		var err error
		if req.Preset != "" {
			if cfg, err = policy.Preset(req.Preset); err != nil {
				reply(w, nil, err)
				return
			}
		}
		_, err = m.Create(r.Context(), r.PathValue("ns"), cfg)
		reply(w, nil, err)
	})
	mux.HandleFunc("POST /repos/{ns}/detach", func(w http.ResponseWriter, r *http.Request) {
		reply(w, nil, m.Detach(r.Context(), r.PathValue("ns")))
	})
	mux.HandleFunc("POST /repos/{ns}/gc", func(w http.ResponseWriter, r *http.Request) {
		var req GCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			reply(w, nil, fmt.Errorf("server: gc: %w", err))
			return
		}
		grace, err := time.ParseDuration(req.Grace)
		if req.Grace == "" {
			grace, err = gcGrace, nil
		}
		if err != nil {
			reply(w, nil, fmt.Errorf("server: gc: grace: %w", err))
			return
		}
		if grace == 0 {
			grace = -1
		}
		ns := r.PathValue("ns")
		res, err := m.GC(r.Context(), ns, gc.Options{Grace: grace, DryRun: req.DryRun})
		if err == nil && !req.DryRun && res.Skipped == "" {
			log.Info("garbage collection", "namespace", ns, "deleted_objects", res.DeletedObjects, "deleted_bytes", res.DeletedBytes)
		}
		if err == nil && req.List {
			res.List = res.Deleted
		}
		reply(w, res, err)
	})
	mux.HandleFunc("POST /backup", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Dir string `json:"dir"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !filepath.IsAbs(req.Dir) {
			reply(w, nil, fmt.Errorf("server: backup needs an absolute directory"))
			return
		}
		rep, err := m.Backup(r.Context(), req.Dir)
		if err == nil {
			log.Info("backup written", "dir", req.Dir, "repositories", len(rep.Repos))
		}
		reply(w, rep, err)
	})
	mux.HandleFunc("POST /repos/{ns}/archive", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Path string `json:"path"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !filepath.IsAbs(req.Path) {
			reply(w, nil, fmt.Errorf("server: archive needs an absolute output path"))
			return
		}
		ns := r.PathValue("ns")
		info, err := ArchiveToFile(r.Context(), req.Path, func(out io.Writer) (repo.ArchiveInfo, error) {
			return m.Archive(r.Context(), ns, out)
		})
		if err == nil {
			log.Info("repository archived", "namespace", ns, "path", req.Path, "packs", info.Packs, "bytes", info.Bytes)
		}
		reply(w, info, err)
	})
	return mux
}

// GCRequest asks for a garbage collection over the admin socket.
type GCRequest struct {
	DryRun bool   `json:"dry_run"`
	Grace  string `json:"grace,omitempty"` // default: the server's
	List   bool   `json:"list,omitempty"`  // include the deleted object hashes
}

// AdminClient talks to a running server over its admin socket.
type AdminClient struct {
	http *http.Client
}

// DialAdmin connects to the admin socket of the server running on dataDir.
// It returns ok == false if no server is running there.
func DialAdmin(dataDir string) (*AdminClient, bool) {
	path := filepath.Join(dataDir, AdminSocket)
	c, err := dialUnix(context.Background(), path, time.Second)
	if err != nil {
		return nil, false
	}
	c.Close()
	return &AdminClient{http: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialUnix(ctx, path, 0)
		},
	}}}, true
}

func (c *AdminClient) call(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://admin"+path, rd)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("server: admin socket: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("server: %s", e.Error)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// List lists the server's open repositories.
func (c *AdminClient) List(ctx context.Context) ([]RepoStatus, error) {
	var out []RepoStatus
	return out, c.call(ctx, http.MethodGet, "/repos", nil, &out)
}

// Do runs a repository action: freeze, unfreeze, attach or detach.
func (c *AdminClient) Do(ctx context.Context, action, ns string) error {
	return c.call(ctx, http.MethodPost, "/repos/"+ns+"/"+action, nil, nil)
}

// Create asks the server to create a namespace from a preset ("" for the
// defaults).
func (c *AdminClient) Create(ctx context.Context, ns, preset string) error {
	return c.call(ctx, http.MethodPost, "/repos/"+ns+"/create", map[string]string{"preset": preset}, nil)
}

// Backup asks the server to write a backup to dir (absolute, writable by
// the server).
func (c *AdminClient) Backup(ctx context.Context, dir string) (BackupReport, error) {
	var rep BackupReport
	err := c.call(ctx, http.MethodPost, "/backup", map[string]string{"dir": dir}, &rep)
	return rep, err
}

// GC asks the server to collect garbage in ns.
func (c *AdminClient) GC(ctx context.Context, ns string, req GCRequest) (*gc.Result, error) {
	var res gc.Result
	return &res, c.call(ctx, http.MethodPost, "/repos/"+ns+"/gc", req, &res)
}

// Archive asks the server to write an archive of ns to path (absolute,
// writable by the server).
func (c *AdminClient) Archive(ctx context.Context, ns, path string) (repo.ArchiveInfo, error) {
	var info repo.ArchiveInfo
	err := c.call(ctx, http.MethodPost, "/repos/"+ns+"/archive", map[string]string{"path": path}, &info)
	return info, err
}
