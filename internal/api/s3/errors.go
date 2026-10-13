package s3

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"

	"github.com/trukeio/f1/internal/auth"
	"github.com/trukeio/f1/internal/cache"
	"github.com/trukeio/f1/internal/content"
	"github.com/trukeio/f1/internal/repo"
	"github.com/trukeio/f1/internal/store"
)

// Error is an S3 error response.
type Error struct {
	Status  int
	Code    string
	Message string
	Key     string
	Header  map[string]string // extra response headers
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func s3err(status int, code, format string, args ...any) *Error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

var statusByCode = map[string]int{
	"AccessDenied":                      http.StatusForbidden,
	"AuthorizationHeaderMalformed":      http.StatusBadRequest,
	"AuthorizationQueryParametersError": http.StatusBadRequest,
	"BadDigest":                         http.StatusBadRequest,
	"BucketAlreadyExists":               http.StatusConflict,
	"BucketAlreadyOwnedByYou":           http.StatusConflict,
	"BucketNotEmpty":                    http.StatusConflict,
	"EntityTooSmall":                    http.StatusBadRequest,
	"EntityTooLarge":                    http.StatusBadRequest,
	"IncompleteBody":                    http.StatusBadRequest,
	"InternalError":                     http.StatusInternalServerError,
	"InvalidAccessKeyId":                http.StatusForbidden,
	"InvalidArgument":                   http.StatusBadRequest,
	"InvalidBucketName":                 http.StatusBadRequest,
	"InvalidDigest":                     http.StatusBadRequest,
	"InvalidPart":                       http.StatusBadRequest,
	"InvalidPartOrder":                  http.StatusBadRequest,
	"InvalidRange":                      http.StatusRequestedRangeNotSatisfiable,
	"InvalidRequest":                    http.StatusBadRequest,
	"InvalidURI":                        http.StatusBadRequest,
	"KeyTooLongError":                   http.StatusBadRequest,
	"MalformedXML":                      http.StatusBadRequest,
	"MetadataTooLarge":                  http.StatusBadRequest,
	"MethodNotAllowed":                  http.StatusMethodNotAllowed,
	"MissingContentLength":              http.StatusLengthRequired,
	"NoSuchBucket":                      http.StatusNotFound,
	"NoSuchKey":                         http.StatusNotFound,
	"NoSuchUpload":                      http.StatusNotFound,
	"NoSuchVersion":                     http.StatusNotFound,
	"NotImplemented":                    http.StatusNotImplemented,
	"PreconditionFailed":                http.StatusPreconditionFailed,
	"RequestTimeTooSkewed":              http.StatusForbidden,
	"SignatureDoesNotMatch":             http.StatusForbidden,
	"SlowDown":                          http.StatusServiceUnavailable,
	"XAmzContentSHA256Mismatch":         http.StatusBadRequest,
}

// errCode returns an error with the usual status of an S3 error code.
func errCode(code, format string, args ...any) *Error {
	status, ok := statusByCode[code]
	if !ok {
		status = http.StatusBadRequest
	}
	return s3err(status, code, format, args...)
}

var (
	errAccessDenied = errCode("AccessDenied", "Access Denied")
	errNoSuchKey    = errCode("NoSuchKey", "The specified key does not exist.")
	errPrecondition = errCode("PreconditionFailed", "At least one of the pre-conditions you specified did not hold")
	errNoSuchUpload = errCode("NoSuchUpload", "The specified multipart upload does not exist. The upload ID might be invalid, or the upload might have been aborted or completed.")
)

func notImplemented(what string) *Error {
	return errCode("NotImplemented", "%s is not implemented by F1", what)
}

type errorBody struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string
	Message   string
	Key       string `xml:",omitempty"`
	Resource  string `xml:",omitempty"`
	RequestID string `xml:"RequestId"`
}

// toS3Error maps core errors to S3 errors.
func (s *Server) toS3Error(r *http.Request, err error) *Error {
	var e *Error
	var se *auth.SigV4Error
	var le *repo.LockedError
	switch {
	case errors.As(err, &e):
		return e
	case errors.As(err, &se):
		return errCode(se.Code, "%s", se.Message)
	case errors.As(err, &le):
		if le.Owner == "" {
			return errCode("AccessDenied", "path %s needs a lock held by the writer", le.Path)
		}
		return errCode("AccessDenied", "path is locked by %s", le.Owner)
	case errors.Is(err, repo.ErrFrozen):
		return errCode("SlowDown", "%s", err.Error())
	case errors.Is(err, repo.ErrConflict):
		return s3err(http.StatusConflict, "OperationAborted", "a conflicting operation is in progress; retry")
	case errors.Is(err, repo.ErrPathConflict):
		return s3err(http.StatusConflict, "InvalidRequest", "the key conflicts with the namespace hierarchy (tree layout): %v", err)
	case errors.Is(err, repo.ErrInvalidPath):
		return errCode("InvalidArgument", "%v; keys like this need a namespace with layout = flat", err)
	case errors.Is(err, repo.ErrNotFound), errors.Is(err, repo.ErrIsDir):
		return errNoSuchKey
	case errors.Is(err, cache.ErrNoUpload):
		return errNoSuchUpload
	case errors.Is(err, content.ErrSizeMismatch):
		return errCode("IncompleteBody", "%v", err)
	case errors.Is(err, store.ErrMissingReference):
		return errCode("InternalError", "%v", err)
	}
	s.Log.Error("S3 request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	return errCode("InternalError", "We encountered an internal error. Please try again.")
}

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
		return // the client went away
	}
	e := s.toS3Error(r, err)
	for k, v := range e.Header {
		w.Header().Set(k, v)
	}
	if e.Status == http.StatusNotModified || r.Method == http.MethodHead {
		w.WriteHeader(e.Status)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(e.Status)
	body := errorBody{Code: e.Code, Message: e.Message, Key: e.Key, Resource: r.URL.Path, RequestID: w.Header().Get("x-amz-request-id")}
	w.Write([]byte(xml.Header))
	xml.NewEncoder(w).Encode(body)
}
