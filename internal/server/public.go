package server

import (
	"io/fs"
	"net/http"
	"sort"
	"sync"
	"time"
)

// recentEvent is one finished chat request, without its prompt text, so
// it is safe to show on a public page.
type recentEvent struct {
	Result     string  `json:"result"`
	Ms         float64 `json:"ms"`
	Provider   string  `json:"provider,omitempty"`
	Similarity float32 `json:"similarity,omitempty"`
	At         int64   `json:"at"` // unix milliseconds
}

// recent keeps the last N requests in a ring buffer.
type recent struct {
	mu   sync.Mutex
	buf  []recentEvent
	next int
	full bool
}

func newRecent(n int) *recent { return &recent{buf: make([]recentEvent, n)} }

func (r *recent) add(e recentEvent) {
	r.mu.Lock()
	r.buf[r.next] = e
	r.next = (r.next + 1) % len(r.buf)
	if r.next == 0 {
		r.full = true
	}
	r.mu.Unlock()
}

// snapshot returns events newest first.
func (r *recent) snapshot() []recentEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.next
	if r.full {
		n = len(r.buf)
	}
	out := make([]recentEvent, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, r.buf[(r.next-i+len(r.buf))%len(r.buf)])
	}
	return out
}

func medianMs(events []recentEvent, keep func(string) bool) *float64 {
	var v []float64
	for _, e := range events {
		if keep(e.Result) {
			v = append(v, e.Ms)
		}
	}
	if len(v) == 0 {
		return nil
	}
	sort.Float64s(v)
	m := v[len(v)/2]
	return &m
}

// publicStats serves aggregate numbers for the website. It never includes
// prompts, answers or tenant names, so it needs no key.
func (s *Server) publicStats(w http.ResponseWriter, _ *http.Request) {
	events := s.recent.snapshot()
	isHit := func(r string) bool { return r == "hit" || r == "coalesced" }
	isModel := func(r string) bool { return r == "miss" || r == "bypass" }
	out := map[string]any{
		"version":        s.o.Version,
		"uptime_seconds": int64(time.Since(s.started).Seconds()),
		"threshold":      s.o.Cache.Threshold(),
		"embedder":       s.o.Embedder.Name(),
		"cache_entries":  s.o.Cache.Stats().Live,
		"median_ms":      map[string]any{"hit": medianMs(events, isHit), "model": medianMs(events, isModel)},
		"window":         len(events),
	}
	if s.o.Metrics != nil {
		req := map[string]float64{}
		var served, hits float64
		for _, k := range []string{"hit", "coalesced", "miss", "bypass", "rate_limited", "error", "cancelled"} {
			v := s.o.Metrics.Requests.Value(k)
			req[k] = v
			switch k {
			case "hit", "coalesced":
				hits += v
				served += v
			case "miss", "bypass":
				served += v
			}
		}
		out["requests"] = req
		if served > 0 {
			out["hit_rate"] = hits / served
		}
	}
	feed := events
	if len(feed) > 40 {
		feed = feed[:40]
	}
	out["recent"] = feed
	var provs []map[string]string
	for _, p := range s.o.Router.Status() {
		provs = append(provs, map[string]string{"name": p.Name, "state": p.State})
	}
	out["providers"] = provs
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, out)
}

// page serves one embedded HTML file at a clean path such as /docs.
func page(sub fs.FS, name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := fs.ReadFile(sub, name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(b)
	}
}
