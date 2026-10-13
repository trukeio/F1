package s3

import (
	"context"
	"encoding/base32"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/trukeio/f1/internal/auth"
	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
	"github.com/trukeio/f1/internal/repo"
	"github.com/trukeio/f1/internal/serverdb"
)

// nullVersion is the version id of objects in buckets without versioning.
const nullVersion = "null"

var versionEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// versionID is the S3 version id of the object version written in commit:
// the base32 of the commit hash (doc/plan.md, "Operations").
func versionID(commit hash.Hash) string { return versionEncoding.EncodeToString(commit[:]) }

func parseVersionID(v string) (hash.Hash, bool) {
	var h hash.Hash
	b, err := versionEncoding.DecodeString(v)
	if err != nil || len(b) != hash.Size {
		return h, false
	}
	copy(h[:], b)
	return h, true
}

func errNoSuchVersion(key string) *Error {
	e := errCode("NoSuchVersion", "The specified version does not exist.")
	e.Key = key
	return e
}

// resolveVersion returns the commit to read a key's version from.
func (s *Server) resolveVersion(ctx context.Context, b *bucket, key, v string) (hash.Hash, error) {
	if v == nullVersion {
		if b.versioned() {
			return hash.Zero, errNoSuchVersion(key)
		}
		return b.head(), nil
	}
	h, ok := parseVersionID(v)
	if !ok {
		return hash.Zero, errCode("InvalidArgument", "Invalid version id specified")
	}
	if _, err := b.rp.ReadCommit(ctx, h); err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return hash.Zero, errNoSuchVersion(key)
		}
		return hash.Zero, err
	}
	return h, nil
}

// missingVersion explains a key missing at a version's commit: a delete
// marker if that commit deleted it (405 with x-amz-delete-marker, as S3
// answers GET on a delete marker), otherwise NoSuchVersion.
func (s *Server) missingVersion(ctx context.Context, b *bucket, commit hash.Hash, key, v string) error {
	c, err := b.rp.ReadCommit(ctx, commit)
	if err != nil {
		return err
	}
	if len(c.Parents) > 0 {
		if _, err := b.rp.Stat(ctx, c.Parents[0], key); err == nil {
			e := errCode("MethodNotAllowed", "The specified method is not allowed against this resource.")
			e.Header = map[string]string{"x-amz-delete-marker": "true", "x-amz-version-id": v, "Last-Modified": time.Unix(c.Committer.Time, 0).UTC().Format(http.TimeFormat)}
			return e
		}
	}
	return errNoSuchVersion(key)
}

// latestVersion finds the newest change of key in the bucket's history.
func (s *Server) latestVersion(ctx context.Context, b *bucket, key string) (repo.Version, bool, error) {
	var found repo.Version
	ok := false
	errFound := errors.New("found")
	err := b.rp.History(ctx, b.head(), key, func(v repo.Version) error {
		if v.Path == key {
			found, ok = v, true
			return errFound
		}
		return nil
	})
	if err != nil && !errors.Is(err, errFound) {
		return found, false, err
	}
	return found, ok, nil
}

type versionDelete struct {
	versionID    string
	deleteMarker bool
}

func (v versionDelete) setHeaders(h http.Header) {
	h.Set("x-amz-version-id", v.versionID)
	if v.deleteMarker {
		h.Set("x-amz-delete-marker", "true")
	}
}

// deleteVersion deletes a specific version. History is immutable, so only
// the newest version of a key can be deleted, which makes the version before
// it current, as in S3: deleting a delete marker restores the object, and
// deleting the current version restores the previous one (or removes the
// key). Older versions remain until the retention policy drops them
// (ADR-0007).
func (s *Server) deleteVersion(ctx context.Context, b *bucket, c *auth.Caller, key, v string) (versionDelete, error) {
	h, ok := parseVersionID(v)
	if v == nullVersion || !ok {
		return versionDelete{}, errNoSuchVersion(key)
	}
	latest, found, err := s.latestVersion(ctx, b, key)
	if err != nil {
		return versionDelete{}, err
	}
	if !found || latest.Commit != h {
		if _, err := b.rp.ReadCommit(ctx, h); err != nil {
			return versionDelete{}, errNoSuchVersion(key)
		}
		return versionDelete{}, notImplemented("deleting a non-current version (F1 keeps history; old versions expire with the namespace retention policy)")
	}
	// Restore the entry before the latest change, if the key is still at
	// the latest version when the commit is made.
	cur := latest.New
	pre := func(e *object.Entry) error {
		if !sameTarget(e, cur) {
			return s3err(http.StatusConflict, "OperationAborted", "the object changed concurrently; retry")
		}
		return nil
	}
	var ch repo.Change
	if latest.Old != nil {
		ch = repo.Change{Op: repo.OpPut, Path: key, Entry: *latest.Old}
	} else {
		ch = repo.Change{Op: repo.OpDelete, Path: key}
	}
	if _, err := s.commit(ctx, b, c, []repo.Change{ch}, pre, "s3: DeleteObject "+key+" version "+v); err != nil {
		return versionDelete{}, err
	}
	return versionDelete{versionID: v, deleteMarker: cur == nil}, nil
}

func sameTarget(a, b *object.Entry) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Kind == b.Kind && a.Target == b.Target
}

type versionXML struct {
	Key          string
	VersionId    string
	IsLatest     bool
	LastModified string
	ETag         string
	Size         int64
	StorageClass string
	Owner        ownerXML
}

type deleteMarkerXML struct {
	Key          string
	VersionId    string
	IsLatest     bool
	LastModified string
	Owner        ownerXML
}

type listVersionsResult struct {
	XMLName             struct{} `xml:"ListVersionsResult"`
	NS                  string   `xml:"xmlns,attr"`
	Name                string
	Prefix              string
	KeyMarker           string
	VersionIdMarker     string
	NextKeyMarker       string `xml:",omitempty"`
	NextVersionIdMarker string `xml:",omitempty"`
	Delimiter           string `xml:",omitempty"`
	MaxKeys             int
	EncodingType        string `xml:",omitempty"`
	IsTruncated         bool
	// Versions and delete markers are interleaved in S3 responses; clients
	// read them as two lists, so they are encoded as two element sequences.
	Versions       []versionXML      `xml:"Version"`
	DeleteMarkers  []deleteMarkerXML `xml:"DeleteMarker"`
	CommonPrefixes []prefixXML       `xml:"CommonPrefixes"`
}

// objVersion is a version found in the history.
type objVersion struct {
	key    string
	commit hash.Hash
	time   time.Time
	entry  *object.Entry // nil: delete marker
	latest bool
}

// listVersions lists object versions by walking the bucket's first-parent
// history (repo.History): each change of a key is a version, each deletion
// a delete marker. Its cost grows with the length of the history
// (ADR-0007). Buckets without versioning list their objects with version
// id "null".
func (s *Server) listVersions(q *request) error {
	b, err := s.bucket(q)
	if err != nil {
		return err
	}
	ctx := q.r.Context()
	if err := need(b.g.Any(serverdb.PermRead)); err != nil {
		return err
	}
	query := q.r.URL.Query()
	prefix, delim := query.Get("prefix"), query.Get("delimiter")
	keyMarker, idMarker := query.Get("key-marker"), query.Get("version-id-marker")
	max, err := maxKeys(query, "max-keys")
	if err != nil {
		return err
	}
	encoding := query.Get("encoding-type")
	if encoding != "" && encoding != "url" {
		return errCode("InvalidArgument", "Invalid Encoding Method specified in Request")
	}
	enc := keyEncoder(encoding)
	var all []objVersion
	if b.versioned() {
		seen := map[string]bool{}
		err = b.rp.History(ctx, b.head(), prefix, func(v repo.Version) error {
			if v.New != nil && !readable(v.New) {
				return nil
			}
			all = append(all, objVersion{key: v.Path, commit: v.Commit, time: v.Time, entry: v.New, latest: !seen[v.Path]})
			seen[v.Path] = true
			return nil
		})
	} else {
		err = b.rp.List(ctx, b.head(), repo.ListOpts{Prefix: prefix}, func(it repo.ListItem) error {
			if readable(&it.Entry) {
				e := it.Entry
				all = append(all, objVersion{key: it.Path, entry: &e, latest: true})
			}
			return nil
		})
	}
	if err != nil {
		return err
	}
	// Keys in order; versions of a key newest first (the walk order).
	sort.SliceStable(all, func(i, j int) bool { return all[i].key < all[j].key })

	res := listVersionsResult{NS: xmlNS, Name: b.name, Prefix: enc(prefix), KeyMarker: enc(keyMarker), VersionIdMarker: idMarker,
		Delimiter: enc(delim), MaxKeys: max, EncodingType: encoding, Versions: []versionXML{}}
	vid := func(v objVersion) string {
		if v.commit.IsZero() {
			return nullVersion
		}
		return versionID(v.commit)
	}
	skipping := keyMarker != ""
	markerSeen := false // the version-id-marker of keyMarker was passed
	n := 0
	lastPrefix := ""
	var pageKeys []string
	var page []objVersion
	for _, v := range all {
		if skipping {
			if v.key < keyMarker {
				continue
			}
			if v.key == keyMarker && !markerSeen {
				// Without a version id marker, every version of keyMarker
				// was listed; otherwise the page starts after the marker.
				markerSeen = idMarker != "" && vid(v) == idMarker
				continue
			}
			skipping = false
		}
		if delim != "" {
			if i := strings.Index(v.key[len(prefix):], delim); i >= 0 {
				cp := v.key[:len(prefix)+i+len(delim)]
				if cp == lastPrefix || cp <= keyMarker {
					continue
				}
				if !b.g.Under(cp, serverdb.PermRead) {
					continue
				}
				if n == max {
					res.IsTruncated = true
					break
				}
				lastPrefix = cp
				res.CommonPrefixes = append(res.CommonPrefixes, prefixXML{Prefix: enc(cp)})
				res.NextKeyMarker, res.NextVersionIdMarker = cp, ""
				n++
				continue
			}
		}
		if !b.g.Allow(v.key, serverdb.PermRead) {
			continue
		}
		if n == max {
			res.IsTruncated = true
			break
		}
		page = append(page, v)
		if v.entry != nil && (v.entry.Attrs == nil || v.entry.Attrs.MTime == 0) && v.commit.IsZero() {
			pageKeys = append(pageKeys, v.key)
		}
		res.NextKeyMarker, res.NextVersionIdMarker = v.key, vid(v)
		n++
	}
	if !res.IsTruncated {
		res.NextKeyMarker, res.NextVersionIdMarker = "", ""
	}
	res.NextKeyMarker = enc(res.NextKeyMarker)
	// Unversioned listings have no commit per version: use the last
	// modification time from the history, as listings do.
	lm, err := s.lastModified(ctx, b.rp, b.head(), pageKeys)
	if err != nil {
		return err
	}
	for _, v := range page {
		ow := owner(q.caller.Principal)
		modified := v.time
		if v.entry == nil {
			res.DeleteMarkers = append(res.DeleteMarkers, deleteMarkerXML{Key: enc(v.key), VersionId: vid(v), IsLatest: v.latest,
				LastModified: s3Time(modified), Owner: ow})
			continue
		}
		if a := v.entry.Attrs; a != nil && a.MTime != 0 {
			modified = time.Unix(0, a.MTime)
		} else if t, ok := lm[v.key]; ok && v.commit.IsZero() {
			modified = t
		}
		size, err := b.rp.FileSize(ctx, v.entry.Target)
		if err != nil {
			return err
		}
		etag, err := s.etag(ctx, b.rp, v.entry)
		if err != nil {
			return err
		}
		res.Versions = append(res.Versions, versionXML{Key: enc(v.key), VersionId: vid(v), IsLatest: v.latest, LastModified: s3Time(modified),
			ETag: `"` + etag + `"`, Size: int64(size), StorageClass: "STANDARD", Owner: ow})
	}
	return writeXML(q.w, http.StatusOK, res)
}
