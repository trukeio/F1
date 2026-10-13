package s3

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/trukeio/f1/internal/hash"
	"github.com/trukeio/f1/internal/object"
	"github.com/trukeio/f1/internal/repo"
	"github.com/trukeio/f1/internal/serverdb"
)

// maxListKeys is the S3 page size limit.
const maxListKeys = 1000

type ownerXML struct {
	ID          string
	DisplayName string
}

type bucketXML struct {
	Name         string
	CreationDate string
	BucketRegion string `xml:",omitempty"`
}

type listBucketsResult struct {
	XMLName           struct{}    `xml:"ListAllMyBucketsResult"`
	NS                string      `xml:"xmlns,attr"`
	Owner             ownerXML    `xml:"Owner"`
	Buckets           []bucketXML `xml:"Buckets>Bucket"`
	ContinuationToken string      `xml:",omitempty"`
	Prefix            string      `xml:",omitempty"`
}

func owner(principal string) ownerXML {
	return ownerXML{ID: principal, DisplayName: strings.TrimPrefix(principal, "user:")}
}

// listBuckets lists the namespaces (and aliases) the caller can access.
func (s *Server) listBuckets(q *request) error {
	ctx := q.r.Context()
	g, err := s.ACL.For(ctx, q.caller, serverdb.ServerNamespace)
	if err != nil {
		return err
	}
	visible, err := s.DB.ACLNamespaces(ctx, q.caller.Principals())
	if err != nil {
		return err
	}
	regs, err := s.DB.Repositories(ctx)
	if err != nil {
		return err
	}
	created := map[string]time.Time{}
	for _, r := range regs {
		created[r.Name] = r.Created
	}
	res := listBucketsResult{NS: xmlNS, Owner: owner(q.caller.Principal), Buckets: []bucketXML{}}
	names := map[string]string{} // bucket → namespace
	for _, ns := range s.Repos.Names() {
		names[ns] = ns
	}
	for name, a := range s.Config.Aliases {
		names[name] = a.Namespace
	}
	query := q.r.URL.Query()
	prefix := query.Get("prefix")
	res.Prefix = prefix
	after := ""
	if t := query.Get("continuation-token"); t != "" {
		b, err := base64.RawURLEncoding.DecodeString(t)
		if err != nil {
			return errCode("InvalidArgument", "The continuation token provided is incorrect")
		}
		after = string(b)
	}
	max := 10000
	if v := query.Get("max-buckets"); v != "" {
		if max, err = strconv.Atoi(v); err != nil || max < 1 || max > 10000 {
			return errCode("InvalidArgument", "max-buckets must be 1 to 10000")
		}
	}
	for _, name := range sortedKeys(names) {
		ns := names[name]
		if name <= after || !strings.HasPrefix(name, prefix) || (!g.ServerAdmin() && !visible[ns]) {
			continue
		}
		if _, err := s.Repos.Get(ns); err != nil {
			continue
		}
		if len(res.Buckets) == max {
			res.ContinuationToken = base64.RawURLEncoding.EncodeToString([]byte(res.Buckets[max-1].Name))
			break
		}
		res.Buckets = append(res.Buckets, bucketXML{Name: name, CreationDate: s3Time(created[ns]), BucketRegion: s.Config.Region})
	}
	return writeXML(q.w, http.StatusOK, res)
}

// listItem is a key or a common prefix of a listing page.
type listItem struct {
	key    string
	prefix bool
	entry  object.Entry
	info   objInfo
}

// listPage lists up to max items after "after" (a key or a common prefix
// returned on an earlier page), filtered by the caller's read permission.
// Delimiter "/" uses the core's directory-aware listing; other delimiters
// are collapsed here.
func (s *Server) listPage(ctx context.Context, b *bucket, commit hash.Hash, prefix, delim, after string, max int) ([]listItem, bool, error) {
	var items []listItem
	truncated := false
	errFull := errors.New("full")
	add := func(it listItem) error {
		if len(items) == max {
			truncated = true
			return errFull
		}
		items = append(items, it)
		return nil
	}
	opts := repo.ListOpts{Prefix: prefix, After: after}
	if delim == "/" {
		opts.Delimiter = "/"
	}
	lastPrefix := ""
	err := b.rp.List(ctx, commit, opts, func(it repo.ListItem) error {
		if it.IsPrefix {
			if !b.g.Under(it.Path, serverdb.PermRead) {
				return nil
			}
			return add(listItem{key: it.Path, prefix: true})
		}
		if delim != "" && delim != "/" {
			if i := strings.Index(it.Path[len(prefix):], delim); i >= 0 {
				cp := it.Path[:len(prefix)+i+len(delim)]
				if cp <= after || cp == lastPrefix {
					return nil
				}
				if !b.g.Under(cp, serverdb.PermRead) {
					return nil
				}
				lastPrefix = cp
				return add(listItem{key: cp, prefix: true})
			}
		}
		if !readable(&it.Entry) || !b.g.Allow(it.Path, serverdb.PermRead) {
			return nil
		}
		return add(listItem{key: it.Path, entry: it.Entry})
	})
	if err != nil && !errors.Is(err, errFull) {
		return nil, false, err
	}
	if err := s.describe(ctx, b, commit, items); err != nil {
		return nil, false, err
	}
	return items, truncated, nil
}

// describe fills in size, ETag and LastModified of the keys of a page, with
// one history walk for the keys that have no attrs.mtime.
func (s *Server) describe(ctx context.Context, b *bucket, commit hash.Hash, items []listItem) error {
	var noMTime []string
	for i := range items {
		it := &items[i]
		if it.prefix {
			continue
		}
		it.info.entry = it.entry
		size, err := b.rp.FileSize(ctx, it.entry.Target)
		if err != nil {
			return err
		}
		it.info.size = int64(size)
		if it.info.etag, err = s.etag(ctx, b.rp, &it.entry); err != nil {
			return err
		}
		if a := it.entry.Attrs; a != nil && a.MTime != 0 {
			it.info.lastModified = time.Unix(0, a.MTime)
		} else {
			noMTime = append(noMTime, it.key)
		}
	}
	if len(noMTime) == 0 {
		return nil
	}
	lm, err := s.lastModified(ctx, b.rp, commit, noMTime)
	if err != nil {
		return err
	}
	for i := range items {
		if t, ok := lm[items[i].key]; ok && !items[i].prefix {
			items[i].info.lastModified = t
		}
	}
	return nil
}

type objectXML struct {
	Key               string
	LastModified      string
	ETag              string
	Size              int64
	StorageClass      string
	Owner             *ownerXML `xml:",omitempty"`
	ChecksumAlgorithm []string  `xml:",omitempty"`
	ChecksumType      string    `xml:",omitempty"`
}

type prefixXML struct {
	Prefix string
}

type listV2Result struct {
	XMLName               struct{} `xml:"ListBucketResult"`
	NS                    string   `xml:"xmlns,attr"`
	Name                  string
	Prefix                string
	Delimiter             string `xml:",omitempty"`
	MaxKeys               int
	EncodingType          string `xml:",omitempty"`
	KeyCount              int
	IsTruncated           bool
	ContinuationToken     *string     `xml:",omitempty"`
	NextContinuationToken string      `xml:",omitempty"`
	StartAfter            string      `xml:",omitempty"`
	Contents              []objectXML `xml:"Contents"`
	CommonPrefixes        []prefixXML `xml:"CommonPrefixes"`
}

type listV1Result struct {
	XMLName        struct{} `xml:"ListBucketResult"`
	NS             string   `xml:"xmlns,attr"`
	Name           string
	Prefix         string
	Marker         string
	NextMarker     string `xml:",omitempty"`
	Delimiter      string `xml:",omitempty"`
	MaxKeys        int
	EncodingType   string `xml:",omitempty"`
	IsTruncated    bool
	Contents       []objectXML `xml:"Contents"`
	CommonPrefixes []prefixXML `xml:"CommonPrefixes"`
}

// keyEncoder returns the encoding of keys in listings: with
// encoding-type=url, URL encoding (spaces as %20).
func keyEncoder(encoding string) func(string) string {
	return func(s string) string {
		if encoding == "url" {
			return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
		}
		return s
	}
}

// maxKeys parses max-keys (default and limit 1000).
func maxKeys(q url.Values, name string) (int, error) {
	v := q.Get(name)
	if v == "" {
		return maxListKeys, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, errCode("InvalidArgument", "%s must be a non-negative integer", name)
	}
	return min(n, maxListKeys), nil
}

func (s *Server) listObjects(q *request, v2 bool) error {
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
	encoding := query.Get("encoding-type")
	if encoding != "" && encoding != "url" {
		return errCode("InvalidArgument", "Invalid Encoding Method specified in Request")
	}
	enc := keyEncoder(encoding)
	max, err := maxKeys(query, "max-keys")
	if err != nil {
		return err
	}
	after := ""
	token := query.Get("continuation-token")
	if v2 {
		after = query.Get("start-after")
		if token != "" {
			t, err := base64.RawURLEncoding.DecodeString(token)
			if err != nil {
				return errCode("InvalidArgument", "The continuation token provided is incorrect")
			}
			after = max2(after, string(t))
		}
	} else {
		after = query.Get("marker")
	}
	commit := b.head()
	var items []listItem
	truncated := false
	if max > 0 {
		if items, truncated, err = s.listPage(ctx, b, commit, prefix, delim, after, max); err != nil {
			return err
		}
	}
	var contents []objectXML
	var prefixes []prefixXML
	fetchOwner := !v2 || query.Get("fetch-owner") == "true"
	for _, it := range items {
		if it.prefix {
			prefixes = append(prefixes, prefixXML{Prefix: enc(it.key)})
			continue
		}
		o := objectXML{Key: enc(it.key), LastModified: s3Time(it.info.lastModified), ETag: `"` + it.info.etag + `"`,
			Size: it.info.size, StorageClass: "STANDARD"}
		if fetchOwner {
			ow := owner(q.caller.Principal)
			o.Owner = &ow
		}
		for k := range it.info.attrs().Checksums {
			alg, _, composite := strings.Cut(k, "-")
			o.ChecksumAlgorithm = append(o.ChecksumAlgorithm, strings.ToUpper(alg))
			o.ChecksumType = FullObject
			if composite {
				o.ChecksumType = Composite
			}
		}
		contents = append(contents, o)
	}
	next := ""
	if truncated && len(items) > 0 {
		next = items[len(items)-1].key
	}
	if v2 {
		res := listV2Result{NS: xmlNS, Name: b.name, Prefix: enc(prefix), Delimiter: enc(delim), MaxKeys: max, EncodingType: encoding,
			KeyCount: len(items), IsTruncated: truncated, StartAfter: enc(query.Get("start-after")),
			Contents: contents, CommonPrefixes: prefixes}
		if query.Has("continuation-token") {
			res.ContinuationToken = &token
		}
		if truncated {
			res.NextContinuationToken = base64.RawURLEncoding.EncodeToString([]byte(next))
		}
		return writeXML(q.w, http.StatusOK, res)
	}
	// Prefix is not encoded in v1 responses (botocore decodes only
	// Delimiter, Marker, NextMarker and keys there).
	res := listV1Result{NS: xmlNS, Name: b.name, Prefix: prefix, Marker: enc(after), Delimiter: enc(delim), MaxKeys: max,
		EncodingType: encoding, IsTruncated: truncated, Contents: contents, CommonPrefixes: prefixes}
	if truncated {
		res.NextMarker = enc(next)
	}
	return writeXML(q.w, http.StatusOK, res)
}

func max2(a, b string) string {
	if a > b {
		return a
	}
	return b
}
