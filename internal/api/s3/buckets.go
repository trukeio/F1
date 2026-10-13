package s3

import (
	"errors"
	"net"
	"net/http"
	"regexp"
	"strings"

	"github.com/trukeio/f1/internal/policy"
	"github.com/trukeio/f1/internal/repo"
	"github.com/trukeio/f1/internal/serverdb"
)

// bucketName is the S3 bucket naming rule (without the IP-address and
// reserved-prefix exceptions); namespace rules apply as well.
var bucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// validBucketName applies the S3 bucket naming rules and the namespace
// rules.
func validBucketName(name string) bool {
	if !bucketName.MatchString(name) || policy.ValidNamespace(name) != nil {
		return false
	}
	if strings.Contains(name, "..") || strings.Contains(name, ".-") || strings.Contains(name, "-.") {
		return false
	}
	return net.ParseIP(name) == nil
}

// serverAdmin reports whether the caller is a server administrator.
func (s *Server) serverAdmin(q *request) (bool, error) {
	g, err := s.ACL.For(q.r.Context(), q.caller, serverdb.ServerNamespace)
	if err != nil {
		return false, err
	}
	return g.ServerAdmin(), nil
}

// createBucket creates a namespace with the S3 preset. Like namespace
// creation in the native API, it needs a server administrator (ADR-0006).
func (s *Server) createBucket(q *request) error {
	if !s.Config.CreateBucket {
		return errCode("AccessDenied", "bucket creation through S3 is disabled on this server")
	}
	admin, err := s.serverAdmin(q)
	if err != nil {
		return err
	}
	if _, ok := s.Config.Aliases[q.bucket]; ok {
		return errCode("BucketAlreadyExists", "The requested bucket name is not available.")
	}
	if !admin {
		if _, err := s.bucket(q); err == nil {
			return errCode("BucketAlreadyExists", "The requested bucket name is not available.")
		}
		return errAccessDenied
	}
	if !validBucketName(q.bucket) {
		return errCode("InvalidBucketName", "The specified bucket is not valid.")
	}
	if _, err := s.Repos.Get(q.bucket); err == nil {
		// As in us-east-1: re-creating one's own bucket succeeds and changes
		// nothing.
		q.w.Header().Set("Location", "/"+q.bucket)
		q.w.WriteHeader(http.StatusOK)
		return nil
	}
	cfg, err := policy.Preset("s3")
	if err != nil {
		return err
	}
	if _, err := s.Repos.Create(q.r.Context(), q.bucket, cfg); err != nil {
		if _, gerr := s.Repos.Get(q.bucket); gerr == nil {
			return errCode("BucketAlreadyOwnedByYou", "Your previous request to create the named bucket succeeded and you already own it.")
		}
		return err
	}
	s.Log.Info("bucket created", "bucket", q.bucket, "principal", q.caller.Principal)
	q.w.Header().Set("Location", "/"+q.bucket)
	q.w.WriteHeader(http.StatusOK)
	return nil
}

// deleteBucket removes an empty namespace: every ref points to an empty
// tree (or there are none). Open multipart uploads are discarded with it,
// as in S3. The directory is moved aside, not deleted (Manager.Delete).
func (s *Server) deleteBucket(q *request) error {
	b, err := s.bucket(q)
	if err != nil {
		return err
	}
	ctx := q.r.Context()
	admin, err := s.serverAdmin(q)
	if err != nil {
		return err
	}
	if !s.Config.CreateBucket || !admin {
		return errAccessDenied
	}
	if _, ok := s.Config.Aliases[q.bucket]; ok {
		return errCode("MethodNotAllowed", "a bucket alias cannot be deleted")
	}
	notEmpty := errCode("BucketNotEmpty", "The bucket you tried to delete is not empty")
	errFound := errors.New("found")
	for _, h := range b.rp.Refs() {
		err := b.rp.List(ctx, h, repo.ListOpts{Limit: 1}, func(repo.ListItem) error { return errFound })
		if errors.Is(err, errFound) {
			return notEmpty
		}
		if err != nil {
			return err
		}
	}
	if err := s.Repos.Delete(ctx, b.ns); err != nil {
		return err
	}
	s.Log.Info("bucket deleted", "bucket", q.bucket, "principal", q.caller.Principal)
	q.w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) headBucket(q *request) error {
	if _, err := s.bucket(q); err != nil {
		return err
	}
	q.w.Header().Set("x-amz-bucket-region", s.Config.Region)
	q.w.WriteHeader(http.StatusOK)
	return nil
}

type locationResult struct {
	XMLName struct{} `xml:"LocationConstraint"`
	NS      string   `xml:"xmlns,attr"`
	Region  string   `xml:",chardata"`
}

func (s *Server) getBucketLocation(q *request) error {
	if _, err := s.bucket(q); err != nil {
		return err
	}
	region := s.Config.Region
	if region == "us-east-1" {
		region = "" // S3 reports the default region as empty
	}
	return writeXML(q.w, http.StatusOK, locationResult{NS: xmlNS, Region: region})
}

type versioningResult struct {
	XMLName struct{} `xml:"VersioningConfiguration"`
	NS      string   `xml:"xmlns,attr"`
	Status  string   `xml:",omitempty"`
}

// getBucketVersioning reports Enabled for namespaces that keep history
// (retention other than "none"); others have never been versioned.
func (s *Server) getBucketVersioning(q *request) error {
	b, err := s.bucket(q)
	if err != nil {
		return err
	}
	res := versioningResult{NS: xmlNS}
	if b.versioned() {
		res.Status = "Enabled"
	}
	return writeXML(q.w, http.StatusOK, res)
}
