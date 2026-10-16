package metrics

import (
	"io"
	"net/http"
	"strconv"
	"time"
)

var (
	httpRequests = Default.NewCounter("f1_http_requests_total", "HTTP requests by API and status class.")
	httpDuration = Default.NewHistogram("f1_http_request_duration_seconds", "HTTP request durations by API.", DurationBuckets)
	httpIn       = Default.NewCounter("f1_http_request_bytes_total", "HTTP request body bytes read, by API.")
	httpOut      = Default.NewCounter("f1_http_response_bytes_total", "HTTP response body bytes written, by API.")
)

// Instrument counts the requests of an API (native, s3, git): their
// status classes, durations and body bytes.
func Instrument(api string, next http.Handler) http.Handler {
	l := Labels{"api": api}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		var body *countingBody
		if r.Body != nil && r.Body != http.NoBody {
			body = &countingBody{ReadCloser: r.Body}
			r.Body = body
		}
		next.ServeHTTP(rw, r)
		httpRequests.Inc(Labels{"api": api, "code": strconv.Itoa(rw.status/100) + "xx"})
		httpDuration.Observe(l, time.Since(start).Seconds())
		if body != nil {
			httpIn.Add(l, float64(body.n))
		}
		httpOut.Add(l, float64(rw.n))
	})
}

// responseWriter records the status and counts bytes. It passes Flush
// through (Git side-band streams flush) and unwraps for
// http.ResponseController.
type responseWriter struct {
	http.ResponseWriter
	status int
	n      int64
	wrote  bool
}

func (w *responseWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *responseWriter) Write(p []byte) (int, error) {
	w.wrote = true
	n, err := w.ResponseWriter.Write(p)
	w.n += int64(n)
	return n, err
}

func (w *responseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

type countingBody struct {
	io.ReadCloser
	n int64
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.n += int64(n)
	return n, err
}

// Handler serves registries at /metrics; their metric names must not
// overlap.
func Handler(regs ...*Registry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		for _, r := range regs {
			if err := r.Write(w); err != nil {
				return
			}
		}
	})
}
