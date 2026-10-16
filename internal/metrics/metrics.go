// Package metrics keeps counters, gauges and histograms and writes them in
// the Prometheus text exposition format (version 0.0.4), without a client
// library (doc/plan.md, "Operations"; ADR-0012).
package metrics

import (
	"fmt"
	"io"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Registry holds metric families.
type Registry struct {
	mu       sync.Mutex
	families map[string]*family
	collect  []func(emit func(name, help, typ string, labels Labels, value float64))
}

// Default is the process-wide registry.
var Default = NewRegistry()

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{families: map[string]*family{}} }

// Labels are label name-value pairs.
type Labels map[string]string

type family struct {
	name, help, typ string
	buckets         []float64 // histograms
	mu              sync.Mutex
	series          map[string]*series // by encoded labels
}

type series struct {
	labels  string
	value   atomic.Uint64 // float64 bits (counter, gauge)
	counts  []atomic.Uint64
	sumBits atomic.Uint64
	count   atomic.Uint64
}

func (r *Registry) family(name, help, typ string, buckets []float64) *family {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.families[name]
	if !ok {
		f = &family{name: name, help: help, typ: typ, buckets: buckets, series: map[string]*series{}}
		r.families[name] = f
	}
	return f
}

func (f *family) with(l Labels) *series {
	key := encode(l)
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.series[key]
	if !ok {
		s = &series{labels: key}
		if f.buckets != nil {
			s.counts = make([]atomic.Uint64, len(f.buckets))
		}
		f.series[key] = s
	}
	return s
}

func encode(l Labels) string {
	if len(l) == 0 {
		return ""
	}
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s=%s", k, strconv.Quote(l[k]))
	}
	return b.String()
}

func addFloat(v *atomic.Uint64, d float64) {
	for {
		old := v.Load()
		if v.CompareAndSwap(old, math.Float64bits(math.Float64frombits(old)+d)) {
			return
		}
	}
}

// Counter is a monotonically increasing value.
type Counter struct{ f *family }

// NewCounter registers a counter family.
func (r *Registry) NewCounter(name, help string) Counter {
	return Counter{r.family(name, help, "counter", nil)}
}

// Add adds d to the series with the given labels.
func (c Counter) Add(l Labels, d float64) { addFloat(&c.f.with(l).value, d) }

// Inc adds one.
func (c Counter) Inc(l Labels) { c.Add(l, 1) }

// Histogram counts observations in buckets.
type Histogram struct{ f *family }

// DurationBuckets are the default buckets for durations in seconds.
var DurationBuckets = []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60}

// NewHistogram registers a histogram family.
func (r *Registry) NewHistogram(name, help string, buckets []float64) Histogram {
	return Histogram{r.family(name, help, "histogram", buckets)}
}

// Observe records a value.
func (h Histogram) Observe(l Labels, v float64) {
	s := h.f.with(l)
	for i, b := range h.f.buckets {
		if v <= b {
			s.counts[i].Add(1)
		}
	}
	addFloat(&s.sumBits, v)
	s.count.Add(1)
}

// Collect registers a function that emits gauges (or counters) computed at
// scrape time.
func (r *Registry) Collect(fn func(emit func(name, help, typ string, labels Labels, value float64))) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.collect = append(r.collect, fn)
}

// Write writes every metric in the text exposition format.
func (r *Registry) Write(w io.Writer) error {
	r.mu.Lock()
	fams := make([]*family, 0, len(r.families))
	for _, f := range r.families {
		fams = append(fams, f)
	}
	collect := slices.Clone(r.collect)
	r.mu.Unlock()

	// Collected metrics, grouped by name.
	type sample struct {
		labels string
		value  float64
	}
	type collected struct {
		help, typ string
		samples   []sample
	}
	byName := map[string]*collected{}
	for _, fn := range collect {
		fn(func(name, help, typ string, l Labels, v float64) {
			c := byName[name]
			if c == nil {
				c = &collected{help: help, typ: typ}
				byName[name] = c
			}
			c.samples = append(c.samples, sample{encode(l), v})
		})
	}

	var b strings.Builder
	header := func(name, help, typ string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	}
	line := func(name, labels string, v float64) {
		if labels != "" {
			fmt.Fprintf(&b, "%s{%s} %s\n", name, labels, formatValue(v))
		} else {
			fmt.Fprintf(&b, "%s %s\n", name, formatValue(v))
		}
	}
	names := make([]string, 0, len(fams)+len(byName))
	famByName := map[string]*family{}
	for _, f := range fams {
		names = append(names, f.name)
		famByName[f.name] = f
	}
	for n := range byName {
		if famByName[n] == nil {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		if c := byName[n]; c != nil && famByName[n] == nil {
			header(n, c.help, c.typ)
			sort.Slice(c.samples, func(i, j int) bool { return c.samples[i].labels < c.samples[j].labels })
			for _, s := range c.samples {
				line(n, s.labels, s.value)
			}
			continue
		}
		f := famByName[n]
		header(f.name, f.help, f.typ)
		f.mu.Lock()
		keys := make([]string, 0, len(f.series))
		for k := range f.series {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			s := f.series[k]
			if f.typ != "histogram" {
				line(f.name, k, math.Float64frombits(s.value.Load()))
				continue
			}
			sep := ""
			if k != "" {
				sep = ","
			}
			for i, bound := range f.buckets {
				line(f.name+"_bucket", k+sep+`le="`+formatValue(bound)+`"`, float64(s.counts[i].Load()))
			}
			line(f.name+"_bucket", k+sep+`le="+Inf"`, float64(s.count.Load()))
			line(f.name+"_sum", k, math.Float64frombits(s.sumBits.Load()))
			line(f.name+"_count", k, float64(s.count.Load()))
		}
		f.mu.Unlock()
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func formatValue(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}
