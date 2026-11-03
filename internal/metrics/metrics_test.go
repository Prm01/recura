package metrics

import (
	"strings"
	"testing"
)

func TestExposition(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("x_total", "help", "result")
	c.Inc("hit")
	c.Add(2, "miss")
	h := r.Histogram("lat_seconds", "help", []float64{0.1, 1}, "result")
	h.Observe(0.05, "hit")
	h.Observe(0.5, "hit")
	h.Observe(5, "hit")
	r.GaugeFunc("g", "help", func() float64 { return 7 })
	var b strings.Builder
	r.Write(&b)
	out := b.String()
	for _, want := range []string{
		`x_total{result="hit"} 1`, `x_total{result="miss"} 2`,
		`lat_seconds_bucket{result="hit",le="0.1"} 1`, `lat_seconds_bucket{result="hit",le="1"} 2`,
		`lat_seconds_bucket{result="hit",le="+Inf"} 3`, `lat_seconds_count{result="hit"} 3`, "g 7",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
