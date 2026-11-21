// Package metrics is a small in-process metrics registry with the Prometheus
// text exposition format (version 0.0.4): counters, gauges and histograms
// with label sets. It has no dependencies (see docs/OBSERVABILITY.md for why
// prometheus/client_golang is not used yet).
//
// Labels must have bounded cardinality: route patterns, status codes,
// outcomes. Never use tenant / merchant ids, phone numbers, RFCs or any
// other personal data as a label value.
package metrics

import (
	"crypto/subtle"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Registry holds metric families in registration order.
type Registry struct {
	mu       sync.Mutex
	families []*family
	byName   map[string]*family
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{byName: map[string]*family{}} }

// Default is the process-wide registry served on /metrics.
var Default = NewRegistry()

type kind int

const (
	kindCounter kind = iota
	kindGauge
	kindHistogram
)

func (k kind) String() string {
	switch k {
	case kindCounter:
		return "counter"
	case kindGauge:
		return "gauge"
	}
	return "histogram"
}

// maxSeries bounds the label combinations of one family (a cardinality
// leak must not exhaust memory); extra combinations are dropped.
const maxSeries = 2000

type family struct {
	name, help string
	kind       kind
	labels     []string
	buckets    []float64
	mu         sync.Mutex
	series     map[string]*series
}

type series struct {
	labelValues []string
	value       float64  // counter / gauge
	counts      []uint64 // histogram, per bucket (non-cumulative)
	sum         float64  // histogram
	count       uint64   // histogram
}

func (r *Registry) register(name, help string, k kind, labels []string, buckets []float64) *family {
	r.mu.Lock()
	defer r.mu.Unlock()
	if f, ok := r.byName[name]; ok {
		if f.kind != k || strings.Join(f.labels, ",") != strings.Join(labels, ",") {
			panic(fmt.Sprintf("metrics: %s re-registered with a different type or labels", name))
		}
		return f
	}
	f := &family{name: name, help: help, kind: k, labels: append([]string(nil), labels...), buckets: buckets, series: map[string]*series{}}
	r.families = append(r.families, f)
	r.byName[name] = f
	return f
}

func (f *family) get(values []string) *series {
	if len(values) != len(f.labels) {
		panic(fmt.Sprintf("metrics: %s wants %d label values, got %d", f.name, len(f.labels), len(values)))
	}
	key := strings.Join(values, "\xff")
	s, ok := f.series[key]
	if !ok {
		if len(f.series) >= maxSeries {
			return nil
		}
		s = &series{labelValues: append([]string(nil), values...)}
		if f.kind == kindHistogram {
			s.counts = make([]uint64, len(f.buckets))
		}
		f.series[key] = s
	}
	return s
}

// Counter is a monotonically increasing value per label set.
type Counter struct{ f *family }

// NewCounter registers (or returns the existing) counter family.
func (r *Registry) NewCounter(name, help string, labels ...string) *Counter {
	return &Counter{r.register(name, help, kindCounter, labels, nil)}
}

// Inc adds one.
func (c *Counter) Inc(labelValues ...string) { c.Add(1, labelValues...) }

// Add adds v (negative values are ignored).
func (c *Counter) Add(v float64, labelValues ...string) {
	if v < 0 || math.IsNaN(v) {
		return
	}
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	if s := c.f.get(labelValues); s != nil {
		s.value += v
	}
}

// Value returns the current value (tests).
func (c *Counter) Value(labelValues ...string) float64 { return value(c.f, labelValues) }

// Gauge is a value that goes up and down per label set.
type Gauge struct{ f *family }

// NewGauge registers (or returns the existing) gauge family.
func (r *Registry) NewGauge(name, help string, labels ...string) *Gauge {
	return &Gauge{r.register(name, help, kindGauge, labels, nil)}
}

// Set sets the value.
func (g *Gauge) Set(v float64, labelValues ...string) {
	g.f.mu.Lock()
	defer g.f.mu.Unlock()
	if s := g.f.get(labelValues); s != nil {
		s.value = v
	}
}

// Value returns the current value (tests).
func (g *Gauge) Value(labelValues ...string) float64 { return value(g.f, labelValues) }

func value(f *family, labelValues []string) float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.series[strings.Join(labelValues, "\xff")]; ok {
		return s.value
	}
	return 0
}

// Histogram counts observations into cumulative buckets.
type Histogram struct{ f *family }

// DefaultLatencyBuckets are HTTP latency buckets in seconds.
var DefaultLatencyBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// NewHistogram registers (or returns the existing) histogram family. buckets
// are upper bounds in increasing order (+Inf is implicit).
func (r *Registry) NewHistogram(name, help string, buckets []float64, labels ...string) *Histogram {
	b := append([]float64(nil), buckets...)
	sort.Float64s(b)
	return &Histogram{r.register(name, help, kindHistogram, labels, b)}
}

// Observe records v.
func (h *Histogram) Observe(v float64, labelValues ...string) {
	if math.IsNaN(v) {
		return
	}
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	s := h.f.get(labelValues)
	if s == nil {
		return
	}
	for i, ub := range h.f.buckets {
		if v <= ub {
			s.counts[i]++
			break
		}
	}
	s.sum += v
	s.count++
}

// Count returns the number of observations (tests).
func (h *Histogram) Count(labelValues ...string) uint64 {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	if s, ok := h.f.series[strings.Join(labelValues, "\xff")]; ok {
		return s.count
	}
	return 0
}

// WriteText writes every family in the Prometheus text format.
func (r *Registry) WriteText(w io.Writer) error {
	r.mu.Lock()
	fams := append([]*family(nil), r.families...)
	r.mu.Unlock()
	var b strings.Builder
	for _, f := range fams {
		f.mu.Lock()
		keys := make([]string, 0, len(f.series))
		for k := range f.series {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", f.name, escapeHelp(f.help), f.name, f.kind)
		for _, k := range keys {
			s := f.series[k]
			switch f.kind {
			case kindHistogram:
				var cum uint64
				for i, ub := range f.buckets {
					cum += s.counts[i]
					fmt.Fprintf(&b, "%s_bucket%s %d\n", f.name, labelString(f.labels, s.labelValues, "le", formatFloat(ub)), cum)
				}
				fmt.Fprintf(&b, "%s_bucket%s %d\n", f.name, labelString(f.labels, s.labelValues, "le", "+Inf"), s.count)
				fmt.Fprintf(&b, "%s_sum%s %s\n", f.name, labelString(f.labels, s.labelValues, "", ""), formatFloat(s.sum))
				fmt.Fprintf(&b, "%s_count%s %d\n", f.name, labelString(f.labels, s.labelValues, "", ""), s.count)
			default:
				fmt.Fprintf(&b, "%s%s %s\n", f.name, labelString(f.labels, s.labelValues, "", ""), formatFloat(s.value))
			}
		}
		f.mu.Unlock()
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func formatFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func escapeHelp(s string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(s)
}

func escapeLabel(s string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`).Replace(s)
}

func labelString(names, values []string, extraName, extraValue string) string {
	if len(names) == 0 && extraName == "" {
		return ""
	}
	parts := make([]string, 0, len(names)+1)
	for i, n := range names {
		parts = append(parts, n+`="`+escapeLabel(values[i])+`"`)
	}
	if extraName != "" {
		parts = append(parts, extraName+`="`+escapeLabel(extraValue)+`"`)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// Handler serves the registry. A non-empty token requires
// "Authorization: Bearer <token>" (constant-time compare).
func Handler(r *Registry, token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if token != "" && !bearerMatches(req.Header.Get("Authorization"), token) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = r.WriteText(w)
	})
}

func bearerMatches(header, token string) bool {
	got, ok := strings.CutPrefix(header, "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

// Since returns the seconds elapsed since t (histogram helper).
func Since(t time.Time) float64 { return time.Since(t).Seconds() }
