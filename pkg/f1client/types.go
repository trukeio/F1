// Package f1client is the Go client for the F1 native API (/api/v1).
//
// Hashes are 64-digit lowercase hex strings. Ref names may be short branch
// names ("main") or full names ("heads/main", "tags/v1"); the client
// escapes them in URLs.
package f1client

import "time"

// NamespaceConfig is a namespace configuration.
type NamespaceConfig struct {
	Layout     string `json:"layout"`
	Retention  string `json:"retention"`
	Autocommit bool   `json:"autocommit"`
	Branches   bool   `json:"branches"`
	Locks      string `json:"locks"`
	Git        bool   `json:"git"`
	DefaultRef string `json:"default_ref"`
	// Merge maps globs to "text" or "binary", overriding the native
	// client's text detection.
	Merge map[string]string `json:"merge,omitempty"`
}

// Namespace describes a namespace (repository).
type Namespace struct {
	Name   string          `json:"name"`
	UUID   string          `json:"uuid"`
	Frozen bool            `json:"frozen"`
	Config NamespaceConfig `json:"config"`
}

// CreateNamespaceRequest creates a namespace from a preset ("s3", "svn",
// "git") or an explicit configuration (or the defaults if neither is set).
type CreateNamespaceRequest struct {
	Name   string           `json:"name"`
	Preset string           `json:"preset,omitempty"`
	Config *NamespaceConfig `json:"config,omitempty"`
}

// Ref is a ref and the commit it points to.
type Ref struct {
	Name   string `json:"name"`
	Commit string `json:"commit"`
}

// UpdateRefRequest is a compare-and-swap ref update. An empty Old means the
// ref must not exist; an empty New deletes it.
type UpdateRefRequest struct {
	Old string `json:"old"`
	New string `json:"new"`
}

// Entry is a tree entry.
type Entry struct {
	Kind        string `json:"kind"`             // file, exec, symlink, dir, gitlink, lfs
	Target      string `json:"target,omitempty"` // File hash (dir: tree hash)
	ContentType string `json:"content_type,omitempty"`
}

// FileInfo describes a file in a commit.
type FileInfo struct {
	Path        string `json:"path"`
	Commit      string `json:"commit"`
	Kind        string `json:"kind"`
	File        string `json:"file"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type,omitempty"`
}

// PutResult describes a written file and the commit that contains it.
type PutResult struct {
	Commit  string `json:"commit"`
	File    string `json:"file"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
	SHA1Git string `json:"sha1git"`
	MD5     string `json:"md5"`
	// Parent is the commit the new commit was built on: the requested
	// parent, or with Rebase the branch head it was rebased on.
	Parent string `json:"parent,omitempty"`
}

// ListItem is a file, or a collapsed prefix when Prefix is set.
type ListItem struct {
	Path   string `json:"path"`
	Prefix bool   `json:"prefix,omitempty"`
	Kind   string `json:"kind,omitempty"`
	File   string `json:"file,omitempty"`
	Size   int64  `json:"size,omitempty"`
}

// ListResponse is one page of a listing. Next, if not empty, is the After
// value for the next page.
type ListResponse struct {
	Commit string     `json:"commit"`
	Items  []ListItem `json:"items"`
	Next   string     `json:"next,omitempty"`
}

// Person is a commit author or committer.
type Person struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Time  int64  `json:"time"`
	TZ    string `json:"tz"`
}

// Commit is a commit.
type Commit struct {
	Hash      string   `json:"hash"`
	Tree      string   `json:"tree"`
	Parents   []string `json:"parents"`
	Author    Person   `json:"author"`
	Committer Person   `json:"committer"`
	Message   string   `json:"message"`
}

// Change is a changed file between two commits.
type Change struct {
	Path string `json:"path"`
	Old  *Entry `json:"old,omitempty"`
	New  *Entry `json:"new,omitempty"`
}

// CommitChange is one change in a CommitRequest.
type CommitChange struct {
	Op    string `json:"op"` // put, delete, rename
	Path  string `json:"path"`
	From  string `json:"from,omitempty"`  // rename
	Entry *Entry `json:"entry,omitempty"` // put
}

// CommitRequest creates a commit from already uploaded objects. Parent is
// the expected current value of Ref; empty means Ref must not exist.
type CommitRequest struct {
	Ref     string         `json:"ref"`
	Parent  string         `json:"parent"`
	Changes []CommitChange `json:"changes"`
	Message string         `json:"message"`
	// Rebase applies the changes on the current head if the ref has moved
	// but no path they name has changed since Parent (ADR-0014).
	Rebase bool `json:"rebase,omitempty"`
}

// CommitResult is the hash of a new commit.
type CommitResult struct {
	Commit string `json:"commit"`
	// Parent is the commit the new commit was built on (see PutResult).
	Parent string `json:"parent,omitempty"`
}

// HashList is a list of object hashes.
type HashList struct {
	Hashes []string `json:"hashes"`
}

// MissingResponse lists the hashes that are not stored.
type MissingResponse struct {
	Missing []string `json:"missing"`
}

// UploadResult reports an object upload.
type UploadResult struct {
	Stored int `json:"stored"`
}

// Lock is a file lock.
type Lock struct {
	ID      string     `json:"id"`
	Ref     string     `json:"ref"`
	Path    string     `json:"path"`
	Owner   string     `json:"owner"`
	Created time.Time  `json:"created"`
	Expires *time.Time `json:"expires,omitempty"`
}

// LockRequest locks a path; TTLSeconds 0 means no expiry.
type LockRequest struct {
	Ref        string `json:"ref"`
	Path       string `json:"path"`
	TTLSeconds int64  `json:"ttl_seconds,omitempty"`
}

// Me is the authenticated caller.
type Me struct {
	Principal string   `json:"principal"`
	Groups    []string `json:"groups"`
}

// S3Key is an S3 access key. Secret is returned only when the key is
// created.
type S3Key struct {
	AccessKey string    `json:"access_key"`
	Principal string    `json:"principal,omitempty"` // in administration listings
	Secret    string    `json:"secret,omitempty"`
	Created   time.Time `json:"created"`
}

// Token is a local API token ("f1_..."). Secret is returned only when the
// token is created.
type Token struct {
	ID          string     `json:"id"`
	Principal   string     `json:"principal"`
	Description string     `json:"description,omitempty"`
	Created     time.Time  `json:"created"`
	Expires     *time.Time `json:"expires,omitempty"` // nil: never
	Secret      string     `json:"secret,omitempty"`
}

// CreateTokenRequest creates a token for the caller. Expires is required,
// and at most the server's auth.token_max_ttl from now.
type CreateTokenRequest struct {
	Description string    `json:"description,omitempty"`
	Expires     time.Time `json:"expires"`
}

// Principal is a user known to F1: a local user, or an OIDC user that has
// used F1 (ADR-0020).
type Principal struct {
	Name   string `json:"name"`   // "user:<name>"
	Issuer string `json:"issuer"` // "local", or the OIDC provider's name
	// Groups are a local user's groups, or an OIDC user's groups at their
	// last token use, recorded at GroupsSeen.
	Groups     []string   `json:"groups"`
	GroupsSeen *time.Time `json:"groups_seen,omitempty"`
	Disabled   bool       `json:"disabled"`
	Created    time.Time  `json:"created"`
}

// CreateUserRequest adds a local user. Names may omit "user:" and
// "group:".
type CreateUserRequest struct {
	Name   string   `json:"name"`
	Groups []string `json:"groups,omitempty"`
}

// AdminTokenRequest creates a token for any principal. A nil Expires
// never expires.
type AdminTokenRequest struct {
	Principal   string     `json:"principal"`
	Description string     `json:"description,omitempty"`
	Expires     *time.Time `json:"expires,omitempty"`
}

// ACLEntry grants Perms (read, write, lock, admin) on the paths of a
// namespace that start with Prefix to Principal ("user:<name>" or
// "group:<name>"). Empty Perms deny, overriding shorter prefixes.
type ACLEntry struct {
	Principal string   `json:"principal"`
	Prefix    string   `json:"prefix"`
	Perms     []string `json:"perms"`
	// Known is set in listings: the principal is a user F1 knows, or a
	// group with a member F1 knows. OIDC users become known when they
	// first use F1.
	Known bool `json:"known,omitempty"`
}

// Error is the body of every error response.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Current string `json:"current,omitempty"` // conflict: the ref's current value
	Owner   string `json:"owner,omitempty"`   // locked: the lock holder
	// Paths lists, for a refused rebase, the paths changed since the
	// parent.
	Paths []string `json:"paths,omitempty"`
}

// Error codes.
const (
	CodeBadRequest      = "bad_request"
	CodeUnauthenticated = "unauthenticated"
	CodeForbidden       = "forbidden"
	CodeNotFound        = "not_found"
	CodeConflict        = "conflict"        // CAS failure; Current is set
	CodePathConflict    = "path_conflict"   // file/directory hierarchy conflict
	CodeExists          = "exists"          // name already taken
	CodeMissingObjects  = "missing_objects" // upload the objects and retry
	CodeLocked          = "locked"          // path locked; Owner is set (empty: lock required)
	CodeFrozen          = "frozen"          // repository frozen for copying
	CodeParentRequired  = "parent_required" // F1-Parent header needed
	CodeLastAdmin       = "last_admin"      // the server would have no enabled administrator
	CodeInternal        = "internal"
)
