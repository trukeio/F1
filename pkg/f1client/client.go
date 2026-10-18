package f1client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client talks to an F1 server.
type Client struct {
	// BaseURL is the server URL, e.g. "https://f1.example.com".
	BaseURL string
	// Token is the bearer token: a local token ("f1_...") or an OIDC
	// access token.
	Token string
	// HTTP is the HTTP client; nil means http.DefaultClient.
	HTTP *http.Client
}

// New returns a client.
func New(baseURL, token string) *Client {
	return &Client{BaseURL: strings.TrimSuffix(baseURL, "/"), Token: token}
}

// APIError is an error response from the server.
type APIError struct {
	Status int
	Error_ Error
}

func (e *APIError) Error() string {
	return fmt.Sprintf("f1: %d %s: %s", e.Status, e.Error_.Code, e.Error_.Message)
}

// Code returns the error code of err if it is an *APIError, or "".
func Code(err error) string {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Error_.Code
	}
	return ""
}

// seg escapes one path segment (ref names may contain "/").
func seg(s string) string { return url.PathEscape(s) }

// filePath escapes a file path, keeping "/" separators.
func filePath(p string) string {
	parts := strings.Split(p, "/")
	for i, x := range parts {
		parts[i] = url.PathEscape(x)
	}
	return strings.Join(parts, "/")
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) request(ctx context.Context, method, path string, body io.Reader, hdr http.Header) (*http.Response, error) {
	return c.requestWithLength(ctx, method, path, body, -1, hdr)
}

// requestWithLength sends a request whose body has a known length (size >=
// 0), so that the server can compute every digest in one pass.
func (c *Client) requestWithLength(ctx context.Context, method, path string, body io.Reader, size int64, hdr http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+"/api/v1"+path, body)
	if err != nil {
		return nil, err
	}
	if size >= 0 {
		req.ContentLength = size
		if size == 0 {
			req.Body = http.NoBody
		}
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotModified {
		defer resp.Body.Close()
		ae := &APIError{Status: resp.StatusCode}
		if method == http.MethodHead {
			ae.Error_ = Error{Code: codeForStatus(resp.StatusCode), Message: resp.Status}
		} else if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&ae.Error_); err != nil {
			ae.Error_ = Error{Code: codeForStatus(resp.StatusCode), Message: resp.Status}
		}
		return nil, ae
	}
	return resp, nil
}

func codeForStatus(s int) string {
	switch s {
	case http.StatusNotFound:
		return CodeNotFound
	case http.StatusUnauthorized:
		return CodeUnauthenticated
	case http.StatusForbidden:
		return CodeForbidden
	}
	return CodeInternal
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	hdr := http.Header{}
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
		hdr.Set("Content-Type", "application/json")
	}
	resp, err := c.request(ctx, method, path, body, hdr)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, err := io.Copy(io.Discard, resp.Body)
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Namespaces lists the namespaces the caller can access.
func (c *Client) Namespaces(ctx context.Context) ([]Namespace, error) {
	var out []Namespace
	return out, c.do(ctx, http.MethodGet, "/ns", nil, &out)
}

// CreateNamespace creates a namespace (server administrators).
func (c *Client) CreateNamespace(ctx context.Context, req CreateNamespaceRequest) (Namespace, error) {
	var out Namespace
	return out, c.do(ctx, http.MethodPost, "/ns", req, &out)
}

// Refs lists the refs of a namespace.
func (c *Client) Refs(ctx context.Context, ns string) ([]Ref, error) {
	var out []Ref
	return out, c.do(ctx, http.MethodGet, "/ns/"+seg(ns)+"/refs", nil, &out)
}

// UpdateRef updates a ref with compare-and-swap.
func (c *Client) UpdateRef(ctx context.Context, ns, ref, old, new string) error {
	return c.do(ctx, http.MethodPut, "/ns/"+seg(ns)+"/refs/"+filePath(ref), UpdateRefRequest{Old: old, New: new}, nil)
}

// GetOptions select a byte range and conditional reads.
type GetOptions struct {
	Range       string // HTTP Range header value, e.g. "bytes=0-99"
	IfNoneMatch string
}

// Get opens a file at a ref (or commit hash). The caller closes the body.
func (c *Client) Get(ctx context.Context, ns, ref, path string, opts *GetOptions) (io.ReadCloser, FileInfo, error) {
	hdr := http.Header{}
	if opts != nil {
		if opts.Range != "" {
			hdr.Set("Range", opts.Range)
		}
		if opts.IfNoneMatch != "" {
			hdr.Set("If-None-Match", opts.IfNoneMatch)
		}
	}
	resp, err := c.request(ctx, http.MethodGet, "/ns/"+seg(ns)+"/files/"+seg(ref)+"/"+filePath(path), nil, hdr)
	if err != nil {
		return nil, FileInfo{}, err
	}
	return resp.Body, fileInfo(resp, path), nil
}

// Open sends a file GET with the given request headers (Range, If-Range,
// If-None-Match) and returns the response as is, for proxies that pass
// on its status (200, 206 or 304) and headers. The caller closes the body.
func (c *Client) Open(ctx context.Context, ns, ref, path string, hdr http.Header) (*http.Response, error) {
	return c.request(ctx, http.MethodGet, "/ns/"+seg(ns)+"/files/"+seg(ref)+"/"+filePath(path), nil, hdr)
}

// Stat returns information about a file.
func (c *Client) Stat(ctx context.Context, ns, ref, path string) (FileInfo, error) {
	resp, err := c.request(ctx, http.MethodHead, "/ns/"+seg(ns)+"/files/"+seg(ref)+"/"+filePath(path), nil, nil)
	if err != nil {
		return FileInfo{}, err
	}
	resp.Body.Close()
	return fileInfo(resp, path), nil
}

func fileInfo(resp *http.Response, path string) FileInfo {
	fi := FileInfo{
		Path:        path,
		Commit:      resp.Header.Get("F1-Commit"),
		Kind:        resp.Header.Get("F1-Kind"),
		File:        resp.Header.Get("F1-File"),
		ContentType: resp.Header.Get("Content-Type"),
	}
	fi.Size, _ = strconv.ParseInt(resp.Header.Get("F1-Size"), 10, 64)
	return fi
}

// PutOptions control Put and Delete.
type PutOptions struct {
	// Parent is the expected current commit of the ref (required unless the
	// namespace has autocommit). "new" means the ref must not exist.
	Parent      string
	ContentType string
	Message     string
	// Rebase applies the write on the current head if the ref has moved
	// but the path has not changed since Parent (ADR-0014).
	Rebase bool
}

// Put writes a file and commits it. size is the content length, or -1 if
// unknown.
func (c *Client) Put(ctx context.Context, ns, ref, path string, r io.Reader, size int64, opts *PutOptions) (PutResult, error) {
	hdr := http.Header{}
	if opts != nil {
		setPutHeaders(hdr, opts)
		if opts.ContentType != "" {
			hdr.Set("Content-Type", opts.ContentType)
		}
	}
	var out PutResult
	resp, err := c.requestWithLength(ctx, http.MethodPut, "/ns/"+seg(ns)+"/files/"+seg(ref)+"/"+filePath(path), r, size, hdr)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	return out, json.NewDecoder(resp.Body).Decode(&out)
}

func setPutHeaders(hdr http.Header, opts *PutOptions) {
	if opts.Parent != "" {
		hdr.Set("F1-Parent", opts.Parent)
	}
	if opts.Message != "" {
		hdr.Set("F1-Message", opts.Message)
	}
	if opts.Rebase {
		hdr.Set("F1-Rebase", "true")
	}
}

// Delete deletes a file and commits.
func (c *Client) Delete(ctx context.Context, ns, ref, path string, opts *PutOptions) (CommitResult, error) {
	hdr := http.Header{}
	if opts != nil {
		setPutHeaders(hdr, opts)
	}
	var out CommitResult
	resp, err := c.request(ctx, http.MethodDelete, "/ns/"+seg(ns)+"/files/"+seg(ref)+"/"+filePath(path), nil, hdr)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	return out, json.NewDecoder(resp.Body).Decode(&out)
}

// ListOptions select a listing page.
type ListOptions struct {
	Delimiter string // "" or "/"
	After     string
	Limit     int
}

// List lists files below prefix at a ref.
func (c *Client) List(ctx context.Context, ns, ref, prefix string, opts *ListOptions) (ListResponse, error) {
	q := url.Values{}
	if opts != nil {
		if opts.Delimiter != "" {
			q.Set("delim", opts.Delimiter)
		}
		if opts.After != "" {
			q.Set("after", opts.After)
		}
		if opts.Limit > 0 {
			q.Set("limit", strconv.Itoa(opts.Limit))
		}
	}
	var out ListResponse
	return out, c.do(ctx, http.MethodGet, "/ns/"+seg(ns)+"/tree/"+seg(ref)+"/"+filePath(prefix)+query(q), nil, &out)
}

func query(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	return "?" + q.Encode()
}

// Log returns the history of a ref, optionally of one path.
func (c *Client) Log(ctx context.Context, ns, ref, path string, limit int) ([]Commit, error) {
	return c.LogPage(ctx, ns, ref, path, 0, limit)
}

// LogPage returns the history of a ref, optionally of one path, leaving
// out the first skip commits.
func (c *Client) LogPage(ctx context.Context, ns, ref, path string, skip, limit int) ([]Commit, error) {
	q := url.Values{}
	if path != "" {
		q.Set("path", path)
	}
	if skip > 0 {
		q.Set("skip", strconv.Itoa(skip))
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var out []Commit
	return out, c.do(ctx, http.MethodGet, "/ns/"+seg(ns)+"/log/"+seg(ref)+query(q), nil, &out)
}

// Diff returns the files that differ between two refs or commits.
func (c *Client) Diff(ctx context.Context, ns, a, b, prefix string) ([]Change, error) {
	q := url.Values{}
	if prefix != "" {
		q.Set("prefix", prefix)
	}
	var out []Change
	return out, c.do(ctx, http.MethodGet, "/ns/"+seg(ns)+"/diff/"+seg(a)+".."+seg(b)+query(q), nil, &out)
}

// MissingObjects returns the hashes that are not stored.
func (c *Client) MissingObjects(ctx context.Context, ns string, hashes []string) ([]string, error) {
	var out MissingResponse
	return out.Missing, c.do(ctx, http.MethodPost, "/ns/"+seg(ns)+"/objects/missing", HashList{Hashes: hashes}, &out)
}

// UploadObjects uploads a pack stream (the pack file format).
func (c *Client) UploadObjects(ctx context.Context, ns string, pack io.Reader) (UploadResult, error) {
	var out UploadResult
	resp, err := c.request(ctx, http.MethodPost, "/ns/"+seg(ns)+"/objects", pack, http.Header{"Content-Type": {"application/x-f1-pack"}})
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	return out, json.NewDecoder(resp.Body).Decode(&out)
}

// GetObject returns one object's type and payload (chunk bytes or
// canonical CBOR).
func (c *Client) GetObject(ctx context.Context, ns, hash string) (string, []byte, error) {
	resp, err := c.request(ctx, http.MethodGet, "/ns/"+seg(ns)+"/objects/"+seg(hash), nil, nil)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.Header.Get("F1-Type"), b, err
}

// FetchObjects returns a pack stream with the given objects. The caller
// closes it.
func (c *Client) FetchObjects(ctx context.Context, ns string, hashes []string) (io.ReadCloser, error) {
	b, err := json.Marshal(HashList{Hashes: hashes})
	if err != nil {
		return nil, err
	}
	resp, err := c.request(ctx, http.MethodPost, "/ns/"+seg(ns)+"/objects/fetch", bytes.NewReader(b), http.Header{"Content-Type": {"application/json"}})
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// Commit creates a commit from uploaded objects.
func (c *Client) Commit(ctx context.Context, ns string, req CommitRequest) (CommitResult, error) {
	var out CommitResult
	return out, c.do(ctx, http.MethodPost, "/ns/"+seg(ns)+"/commits", req, &out)
}

// Lock locks a path.
func (c *Client) Lock(ctx context.Context, ns, ref, path string, ttl time.Duration) (Lock, error) {
	var out Lock
	return out, c.do(ctx, http.MethodPost, "/ns/"+seg(ns)+"/locks", LockRequest{Ref: ref, Path: path, TTLSeconds: int64(ttl / time.Second)}, &out)
}

// Unlock removes a lock; force removes another principal's lock (admin).
func (c *Client) Unlock(ctx context.Context, ns, id string, force bool) error {
	q := ""
	if force {
		q = "?force=true"
	}
	return c.do(ctx, http.MethodDelete, "/ns/"+seg(ns)+"/locks/"+seg(id)+q, nil, nil)
}

// Locks lists locks of a ref ("" for all) below prefix.
func (c *Client) Locks(ctx context.Context, ns, ref, prefix string) ([]Lock, error) {
	q := url.Values{}
	if ref != "" {
		q.Set("ref", ref)
	}
	if prefix != "" {
		q.Set("prefix", prefix)
	}
	var out []Lock
	return out, c.do(ctx, http.MethodGet, "/ns/"+seg(ns)+"/locks"+query(q), nil, &out)
}

// Archive streams a repository archive (ZIP) to w (namespace
// administrators).
func (c *Client) Archive(ctx context.Context, ns string, w io.Writer) error {
	resp, err := c.request(ctx, http.MethodGet, "/ns/"+seg(ns)+"/archive", nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = io.Copy(w, resp.Body)
	return err
}

// Me returns the authenticated caller.
func (c *Client) Me(ctx context.Context) (Me, error) {
	var out Me
	return out, c.do(ctx, http.MethodGet, "/me", nil, &out)
}

// CreateS3Key creates an S3 access key for the caller. The secret is
// returned only here.
func (c *Client) CreateS3Key(ctx context.Context) (S3Key, error) {
	var out S3Key
	return out, c.do(ctx, http.MethodPost, "/me/s3keys", nil, &out)
}

// S3Keys lists the caller's S3 access keys (without secrets).
func (c *Client) S3Keys(ctx context.Context) ([]S3Key, error) {
	var out []S3Key
	return out, c.do(ctx, http.MethodGet, "/me/s3keys", nil, &out)
}

// DeleteS3Key revokes one of the caller's S3 access keys.
func (c *Client) DeleteS3Key(ctx context.Context, accessKey string) error {
	return c.do(ctx, http.MethodDelete, "/me/s3keys/"+seg(accessKey), nil, nil)
}

// CreateToken creates a token for the caller. The secret is returned only
// here.
func (c *Client) CreateToken(ctx context.Context, req CreateTokenRequest) (Token, error) {
	var out Token
	return out, c.do(ctx, http.MethodPost, "/me/tokens", req, &out)
}

// Tokens lists the caller's tokens that are not revoked (without secrets).
// Expired tokens are listed until revoked.
func (c *Client) Tokens(ctx context.Context) ([]Token, error) {
	var out []Token
	return out, c.do(ctx, http.MethodGet, "/me/tokens", nil, &out)
}

// DeleteToken revokes one of the caller's tokens.
func (c *Client) DeleteToken(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/me/tokens/"+seg(id), nil, nil)
}
