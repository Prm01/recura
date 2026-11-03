// Package metrics is a small Prometheus-compatible registry: labelled
// counters, gauges and histograms rendered in the text exposition format.
package metrics

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type kind int

const (
	counterKind kind = iota
	gaugeKind
	histogramKind
)

type family struct {
	name, help string
	kind       kind
	labels     []string
	buckets    []float64
	mu         sync.Mutex
	series     map[string]*series
	gaugeFn    func() float64
}

type series struct {
	values []string
	value  float64
	counts []uint64 // histogram buckets (cumulative at render time)
	sum    float64
	count  uint64
}

// Registry holds metric families.
type Registry struct {
	mu       sync.Mutex
	families []*family
}

func NewRegistry() *Registry { return &Registry{} }

func (r *Registry) add(f *family) *family {
	f.series = map[string]*series{}
	r.mu.Lock()
	r.families = append(r.families, f)
	r.mu.Unlock()
	return f
}

// Counter is a monotonically increasing labelled value.
type Counter struct{ f *family }

// Histogram observes values into buckets.
type Histogram struct{ f *family }

// Gauge is a labelled value that can go up and down.
type Gauge struct{ f *family }

func (r *Registry) Counter(name, help string, labels ...string) Counter {
	return Counter{r.add(&family{name: name, help: help, kind: counterKind, labels: labels})}
}

func (r *Registry) Gauge(name, help string, labels ...string) Gauge {
	return Gauge{r.add(&family{name: name, help: help, kind: gaugeKind, labels: labels})}
}

// GaugeFunc reports a value computed at scrape time.
func (r *Registry) GaugeFunc(name, help string, fn func() float64) {
	r.add(&family{name: name, help: help, kind: gaugeKind, gaugeFn: fn})
}

func (r *Registry) Histogram(name, help string, buckets []float64, labels ...string) Histogram {
	b := append([]float64(nil), buckets...)
	sort.Float64s(b)
	return Histogram{r.add(&family{name: name, help: help, kind: histogramKind, labels: labels, buckets: b})}
}

// DefaultLatencyBuckets suit both sub-millisecond cache hits and
// multi-second model calls.
var DefaultLatencyBuckets = []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

func (f *family) get(values []string) *series {
	if len(values) != len(f.labels) {
		panic(fmt.Sprintf("metrics: %s wants %d labels, got %d", f.name, len(f.labels), len(values)))
	}
	key := strings.Join(values, "\xff")
	s := f.series[key]
	if s == nil {
		s = &series{values: append([]string(nil), values...)}
		if f.kind == histogramKind {
			s.counts = make([]uint64, len(f.buckets))
		}
		f.series[key] = s
	}
	return s
}

func (c Counter) Add(v float64, labels ...string) {
	c.f.mu.Lock()
	c.f.get(labels).value += v
	c.f.mu.Unlock()
}

func (c Counter) Inc(labels ...string) { c.Add(1, labels...) }

func (g Gauge) Set(v float64, labels ...string) {
	g.f.mu.Lock()
	g.f.get(labels).value = v
	g.f.mu.Unlock()
}

func (h Histogram) Observe(v float64, labels ...string) {
	h.f.mu.Lock()
	s := h.f.get(labels)
	for i, b := range h.f.buckets {
		if v <= b {
			s.counts[i]++
			break
		}
	}
	s.sum += v
	s.count++
	h.f.mu.Unlock()
}

// Value returns a counter's current value (for tests and the stats API).
func (c Counter) Value(labels ...string) float64 {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	return c.f.get(labels).value
}

func fmtFloat(v float64) string {
	if math.IsInf(v, 1) {
		return "+Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func labelStr(names, values []string, extra ...string) string {
	var parts []string
	for i, n := range names {
		parts = append(parts, fmt.Sprintf(`%s="%s"`, n, escape(values[i])))
	}
	for i := 0; i+1 < len(extra); i += 2 {
		parts = append(parts, fmt.Sprintf(`%s="%s"`, extra[i], extra[i+1]))
	}
	if len(parts) == 0 {
		return ""
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func escape(s string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`).Replace(s)
}

// Write renders every family in Prometheus text format.
func (r *Registry) Write(w io.Writer) {
	r.mu.Lock()
	fams := append([]*family(nil), r.families...)
	r.mu.Unlock()
	for _, f := range fams {
		typ := [...]string{"counter", "gauge", "histogram"}[f.kind]
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", f.name, f.help, f.name, typ)
		if f.gaugeFn != nil {
			fmt.Fprintf(w, "%s %s\n", f.name, fmtFloat(f.gaugeFn()))
			continue
		}
		f.mu.Lock()
		keys := make([]string, 0, len(f.series))
		for k := range f.series {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			s := f.series[k]
			if f.kind != histogramKind {
				fmt.Fprintf(w, "%s%s %s\n", f.name, labelStr(f.labels, s.values), fmtFloat(s.value))
				continue
			}
			var cum uint64
			for i, b := range f.buckets {
				cum += s.counts[i]
				fmt.Fprintf(w, "%s_bucket%s %d\n", f.name, labelStr(f.labels, s.values, "le", fmtFloat(b)), cum)
			}
			fmt.Fprintf(w, "%s_bucket%s %d\n", f.name, labelStr(f.labels, s.values, "le", "+Inf"), s.count)
			fmt.Fprintf(w, "%s_sum%s %s\n", f.name, labelStr(f.labels, s.values), fmtFloat(s.sum))
			fmt.Fprintf(w, "%s_count%s %d\n", f.name, labelStr(f.labels, s.values), s.count)
		}
		f.mu.Unlock()
	}
}

// Handler serves /metrics.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		r.Write(w)
	})
}
