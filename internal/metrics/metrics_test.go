package metrics

import (
	"bytes"
	"strings"
	"testing"
)

func TestExposition(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("f1_test_total", "A test counter.")
	c.Inc(Labels{"api": "s3", "code": "2xx"})
	c.Add(Labels{"api": "s3", "code": "2xx"}, 2)
	c.Inc(nil)
	h := r.NewHistogram("f1_test_seconds", "A test histogram.", []float64{0.1, 1})
	h.Observe(Labels{"api": "git"}, 0.05)
	h.Observe(Labels{"api": "git"}, 0.5)
	h.Observe(Labels{"api": "git"}, 5)
	r.Collect(func(emit func(name, help, typ string, l Labels, v float64)) {
		emit("f1_test_gauge", "A gauge.", "gauge", Labels{"namespace": `a"b`}, 7)
	})
	var b bytes.Buffer
	if err := r.Write(&b); err != nil {
		t.Fatal(err)
	}
	want := `# HELP f1_test_gauge A gauge.
# TYPE f1_test_gauge gauge
f1_test_gauge{namespace="a\"b"} 7
# HELP f1_test_seconds A test histogram.
# TYPE f1_test_seconds histogram
f1_test_seconds_bucket{api="git",le="0.1"} 1
f1_test_seconds_bucket{api="git",le="1"} 2
f1_test_seconds_bucket{api="git",le="+Inf"} 3
f1_test_seconds_sum{api="git"} 5.55
f1_test_seconds_count{api="git"} 3
# HELP f1_test_total A test counter.
# TYPE f1_test_total counter
f1_test_total 1
f1_test_total{api="s3",code="2xx"} 3
`
	if got := b.String(); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if strings.Count(b.String(), "# TYPE") != 3 {
		t.Fatal("families")
	}
}
