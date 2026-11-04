// Package server exposes Recura over HTTP with an OpenAI-compatible API, so
// any OpenAI SDK can use the gateway by changing its base URL.
package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Prm01/recura/internal/cache"
	sembed "github.com/Prm01/recura/internal/embed"
	"github.com/Prm01/recura/internal/metrics"
	"github.com/Prm01/recura/internal/privacy"
	"github.com/Prm01/recura/internal/provider"
	"github.com/Prm01/recura/internal/ratelimit"
	"github.com/Prm01/recura/internal/router"
)

//go:embed static
var static embed.FS

// Options wires the server's dependencies.
type Options struct {
	Cache        *cache.Cache
	Embedder     sembed.Embedder
	Router       *router.Router
	Limiter      ratelimit.Limiter
	Privacy      *privacy.Detector
	Metrics      *Metrics
	Log          *slog.Logger
	APIKeys      map[string]string // client key -> tenant; empty = open gateway
	AdminKey     string
	MaxPromptLen int
	CORSOrigins  []string
	Version      string
	NoCoalesce   bool // disable request coalescing (for benchmarks)
}

// Server is the HTTP front end.
type Server struct {
	o       Options
	mux     *http.ServeMux
	flights flights
	recent  *recent
	started time.Time
}

// Metrics are the gateway's Prometheus series.
type Metrics struct {
	Reg            *metrics.Registry
	Requests       metrics.Counter
	Duration       metrics.Histogram
	ProviderCalls  metrics.Counter
	ProviderTime   metrics.Histogram
	Similarity     metrics.Histogram
	TokensSaved    metrics.Counter
	LimiterErrors  metrics.Counter
	EmbedderErrors metrics.Counter
}

// NewMetrics registers Recura's metrics on a new registry.
func NewMetrics(c *cache.Cache) *Metrics {
	r := metrics.NewRegistry()
	m := &Metrics{
		Reg:            r,
		Requests:       r.Counter("recura_requests_total", "Chat requests by outcome.", "result"),
		Duration:       r.Histogram("recura_request_duration_seconds", "End-to-end request latency by outcome.", metrics.DefaultLatencyBuckets, "result"),
		ProviderCalls:  r.Counter("recura_provider_requests_total", "Upstream model calls.", "provider", "status"),
		ProviderTime:   r.Histogram("recura_provider_duration_seconds", "Upstream model latency.", metrics.DefaultLatencyBuckets, "provider"),
		Similarity:     r.Histogram("recura_cache_similarity", "Similarity of the nearest cache entry for each lookup.", []float64{0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.75, 0.8, 0.85, 0.9, 0.95, 1}),
		TokensSaved:    r.Counter("recura_tokens_saved_total", "Estimated output tokens served from cache instead of a model."),
		LimiterErrors:  r.Counter("recura_ratelimit_errors_total", "Rate limiter backend errors (requests were allowed)."),
		EmbedderErrors: r.Counter("recura_embedder_errors_total", "Embedding failures (requests bypassed the cache)."),
	}
	if c != nil {
		r.GaugeFunc("recura_cache_entries", "Live entries in the semantic cache.", func() float64 { return float64(c.Stats().Live) })
	}
	return m
}

// RouterObserver returns a router.Observer that records provider metrics.
func (m *Metrics) RouterObserver() router.Observer {
	return func(name string, d time.Duration, err error) {
		status := "ok"
		if err != nil {
			status = "error"
		}
		m.ProviderCalls.Inc(name, status)
		m.ProviderTime.Observe(d.Seconds(), name)
	}
}

func New(o Options) *Server {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.MaxPromptLen <= 0 {
		o.MaxPromptLen = 8000
	}
	s := &Server{o: o, mux: http.NewServeMux(), recent: newRecent(500), started: time.Now()}
	s.mux.HandleFunc("POST /v1/chat/completions", s.chat)
	s.mux.HandleFunc("GET /v1/models", s.models)
	s.mux.HandleFunc("POST /v1/feedback", s.feedback)
	s.mux.HandleFunc("GET /v1/cache/stats", s.admin(s.cacheStats))
	s.mux.HandleFunc("GET /v1/cache/entries/{id}", s.admin(s.getEntry))
	s.mux.HandleFunc("DELETE /v1/cache/entries/{id}", s.admin(s.deleteEntry))
	s.mux.HandleFunc("GET /v1/providers", s.admin(s.providers))
	s.mux.HandleFunc("POST /v1/providers/{name}/{action}", s.admin(s.providerAction))
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok", "version": o.Version})
	})
	if o.Metrics != nil {
		s.mux.Handle("GET /metrics", o.Metrics.Reg.Handler())
	}
	sub, _ := fs.Sub(static, "static")
	s.mux.HandleFunc("GET /v1/public/stats", s.publicStats)
	s.mux.HandleFunc("GET /console", page(sub, "console.html"))
	s.mux.HandleFunc("GET /docs", page(sub, "docs.html"))
	s.mux.Handle("GET /", http.FileServer(http.FS(sub)))
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if len(s.o.CORSOrigins) > 0 {
		origin := r.Header.Get("Origin")
		for _, o := range s.o.CORSOrigins {
			if o == "*" || o == origin {
				w.Header().Set("Access-Control-Allow-Origin", o)
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Recura-Cache")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Expose-Headers", "X-Recura-Cache, X-Recura-Coalesced, X-Recura-Similarity, X-Recura-Entry, X-Recura-Provider, X-Recura-Latency-Ms, X-RateLimit-Remaining, Retry-After")
				break
			}
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	s.mux.ServeHTTP(w, r)
}

// ---- chat ------------------------------------------------------------

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []wireMessage `json:"messages"`
	Stream      bool          `json:"stream"`
	MaxTokens   int           `json:"max_tokens"`
	Temperature *float64      `json:"temperature"`
}

type wireMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// text accepts both "content": "..." and the array-of-parts form.
func (m wireMessage) text() (string, error) {
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &parts); err != nil {
		return "", errors.New("content must be a string or an array of text parts")
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String(), nil
}

type outcome struct {
	result     string // hit, miss, bypass, rate_limited, error, bad_request
	similarity float32
	entryID    string
	provider   string
	tenant     string
	start      time.Time
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	oc := outcome{start: time.Now()}
	defer func() { s.record(r, oc) }()

	tenant, rateKey, ok := s.authenticate(r)
	if !ok {
		oc.result = "unauthorized"
		apiError(w, 401, "invalid_api_key", "Missing or unknown API key. Send it as: Authorization: Bearer <key>.")
		return
	}
	oc.tenant = tenant

	d, err := s.o.Limiter.Allow(r.Context(), rateKey)
	if err != nil {
		s.o.Log.Warn("rate limiter unavailable, allowing request", "err", err)
		if s.o.Metrics != nil {
			s.o.Metrics.LimiterErrors.Inc()
		}
	}
	if d.Remaining >= 0 {
		w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(int(d.Remaining)))
	}
	if !d.Allowed {
		oc.result = "rate_limited"
		w.Header().Set("Retry-After", strconv.Itoa(int(d.RetryAfter.Seconds()+0.999)))
		apiError(w, 429, "rate_limit_exceeded", fmt.Sprintf("Rate limit reached. Retry in %.1fs.", d.RetryAfter.Seconds()))
		return
	}

	var req chatRequest
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		oc.result = "bad_request"
		apiError(w, 400, "invalid_request", "Body must be JSON: "+err.Error())
		return
	}
	msgs, userText, system, cacheable, err := s.normalize(req)
	if err != nil {
		oc.result = "bad_request"
		apiError(w, 400, "invalid_request", err.Error())
		return
	}
	if strings.EqualFold(r.Header.Get("X-Recura-Cache"), "off") || s.o.Privacy.Personal(userText) {
		cacheable = false
	}
	model := req.Model
	if model == "" {
		model = "recura"
	}

	var vec []float32
	var lead *flight
	partition := partitionKey(tenant, model, system)
	if cacheable {
		vec, err = s.o.Embedder.Embed(r.Context(), userText)
		if err != nil {
			s.o.Log.Warn("embedding failed, bypassing cache", "err", err)
			if s.o.Metrics != nil {
				s.o.Metrics.EmbedderErrors.Inc()
			}
			cacheable = false
		}
	}
	if cacheable {
		m := s.o.Cache.Lookup(partition, vec)
		if m.Entry != nil {
			oc.similarity = m.Score
			if s.o.Metrics != nil {
				s.o.Metrics.Similarity.Observe(float64(m.Score))
			}
			w.Header().Set("X-Recura-Similarity", strconv.FormatFloat(float64(m.Score), 'f', 4, 32))
		}
		if m.Hit {
			oc.result, oc.entryID, oc.provider = "hit", m.Entry.ID, m.Entry.Provider
			s.o.Cache.RecordHit(r.Context(), m.Entry.ID)
			if s.o.Metrics != nil {
				s.o.Metrics.TokensSaved.Add(float64(len(m.Entry.Response) / 4))
			}
			w.Header().Set("X-Recura-Cache", "HIT")
			w.Header().Set("X-Recura-Entry", m.Entry.ID)
			w.Header().Set("X-Recura-Latency-Ms", fmtMs(time.Since(oc.start)))
			meta := map[string]any{"cache": "hit", "similarity": m.Score, "entry_id": m.Entry.ID, "matched_prompt": m.Entry.Prompt, "provider": m.Entry.Provider}
			if req.Stream {
				sw := newStream(w, model)
				sw.delta(m.Entry.Response)
				sw.finish(meta)
				return
			}
			writeJSON(w, 200, completion(model, m.Entry.Response, len(userText)/4, len(m.Entry.Response)/4, meta))
			return
		}
		oc.result = "miss"
		w.Header().Set("X-Recura-Cache", "MISS")
		f, score, leader := (*flight)(nil), float32(0), true
		if !s.o.NoCoalesce {
			f, score, leader = s.flights.join(partition, userText, vec, s.o.Cache.Threshold())
		}
		if !leader {
			select {
			case <-f.done:
			case <-r.Context().Done():
				oc.result = "cancelled"
				return
			}
			if f.ok {
				s.serveCoalesced(w, &oc, req.Stream, model, userText, f, score)
				return
			}
			// The leader failed; try the model ourselves.
		} else if f != nil {
			lead = f
			defer s.flights.finish(lead)
		}
	} else {
		oc.result = "bypass"
		w.Header().Set("X-Recura-Cache", "BYPASS")
	}

	preq := provider.Request{Messages: msgs, MaxTokens: req.MaxTokens, Temperature: req.Temperature}
	var res provider.Result
	var used string
	var attempts []router.Attempt
	if req.Stream {
		var sw *stream
		res, used, attempts, err = s.o.Router.Complete(r.Context(), preq, func(p, delta string) {
			if sw == nil {
				w.Header().Set("X-Recura-Provider", p)
				sw = newStream(w, model)
			}
			sw.delta(delta)
		})
		oc.provider = used
		if err != nil {
			oc.result = errResult(err)
			if sw == nil {
				s.upstreamError(w, err, attempts)
				return
			}
			sw.fail(err)
			return
		}
		if sw == nil { // empty answer
			w.Header().Set("X-Recura-Provider", used)
			sw = newStream(w, model)
		}
		entryID := s.store(r.Context(), cacheable, partition, userText, vec, res.Text, used)
		lead.publish(res.Text, entryID, used)
		sw.finish(map[string]any{"cache": strings.ToLower(w.Header().Get("X-Recura-Cache")), "entry_id": entryID, "provider": used, "attempts": attempts})
		oc.entryID = entryID
		return
	}
	res, used, attempts, err = s.o.Router.Complete(r.Context(), preq, nil)
	oc.provider = used
	if err != nil {
		oc.result = errResult(err)
		s.upstreamError(w, err, attempts)
		return
	}
	entryID := s.store(r.Context(), cacheable, partition, userText, vec, res.Text, used)
	lead.publish(res.Text, entryID, used)
	oc.entryID = entryID
	w.Header().Set("X-Recura-Provider", used)
	if entryID != "" {
		w.Header().Set("X-Recura-Entry", entryID)
	}
	w.Header().Set("X-Recura-Latency-Ms", fmtMs(time.Since(oc.start)))
	meta := map[string]any{"cache": strings.ToLower(w.Header().Get("X-Recura-Cache")), "entry_id": entryID, "provider": used, "attempts": attempts}
	writeJSON(w, 200, completion(model, res.Text, res.InputTokens, res.OutputTokens, meta))
}

func (f *flight) publish(text, entryID, provider string) {
	if f == nil || text == "" {
		return
	}
	f.ok, f.text, f.entryID, f.provider = true, text, entryID, provider
}

// serveCoalesced answers a follower with its leader's result.
func (s *Server) serveCoalesced(w http.ResponseWriter, oc *outcome, stream bool, model, userText string, f *flight, score float32) {
	oc.result, oc.entryID, oc.provider, oc.similarity = "coalesced", f.entryID, f.provider, score
	if s.o.Metrics != nil {
		s.o.Metrics.TokensSaved.Add(float64(len(f.text) / 4))
	}
	h := w.Header()
	h.Set("X-Recura-Cache", "HIT")
	h.Set("X-Recura-Coalesced", "true")
	h.Set("X-Recura-Similarity", strconv.FormatFloat(float64(score), 'f', 4, 32))
	if f.entryID != "" {
		h.Set("X-Recura-Entry", f.entryID)
	}
	h.Set("X-Recura-Provider", f.provider)
	h.Set("X-Recura-Latency-Ms", fmtMs(time.Since(oc.start)))
	meta := map[string]any{"cache": "hit", "coalesced": true, "similarity": score, "entry_id": f.entryID, "matched_prompt": f.prompt, "provider": f.provider}
	if stream {
		sw := newStream(w, model)
		sw.delta(f.text)
		sw.finish(meta)
		return
	}
	writeJSON(w, 200, completion(model, f.text, len(userText)/4, len(f.text)/4, meta))
}

// normalize validates messages and decides whether the request can use
// the cache. Only single-turn requests (system prompts plus one user
// message) are cached: in a longer conversation the right answer depends
// on earlier turns, so a lookup on the last message alone could be wrong.
func (s *Server) normalize(req chatRequest) (msgs []provider.Message, user, system string, cacheable bool, err error) {
	if len(req.Messages) == 0 {
		return nil, "", "", false, errors.New("messages must not be empty")
	}
	var users, assistants int
	var sys []string
	total := 0
	for i, m := range req.Messages {
		t, err := m.text()
		if err != nil {
			return nil, "", "", false, fmt.Errorf("messages[%d]: %w", i, err)
		}
		switch m.Role {
		case "system", "developer":
			sys = append(sys, t)
			m.Role = "system"
		case "user":
			users++
			user = t
		case "assistant":
			assistants++
		default:
			return nil, "", "", false, fmt.Errorf("messages[%d]: unsupported role %q", i, m.Role)
		}
		total += len(t)
		msgs = append(msgs, provider.Message{Role: m.Role, Content: t})
	}
	if users == 0 {
		return nil, "", "", false, errors.New("at least one user message is required")
	}
	if total > s.o.MaxPromptLen {
		return nil, "", "", false, fmt.Errorf("prompt is %d characters; the limit is %d", total, s.o.MaxPromptLen)
	}
	return msgs, user, strings.Join(sys, "\n"), users == 1 && assistants == 0 && strings.TrimSpace(user) != "", nil
}

func (s *Server) store(ctx context.Context, cacheable bool, partition, prompt string, vec []float32, answer, prov string) string {
	if !cacheable || strings.TrimSpace(answer) == "" {
		return ""
	}
	e := &cache.Entry{Partition: partition, Prompt: prompt, Response: answer, Vector: vec, Provider: prov}
	// Store with a detached context: the client may have gone, but the
	// answer is still worth keeping.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	s.o.Cache.Insert(sctx, e)
	return e.ID
}

func partitionKey(tenant, model, system string) string {
	h := sha256.Sum256([]byte(system))
	return tenant + "|" + model + "|" + hex.EncodeToString(h[:6])
}

func (s *Server) authenticate(r *http.Request) (tenant, rateKey string, ok bool) {
	if len(s.o.APIKeys) == 0 {
		return "public", "ip:" + clientIP(r), true
	}
	key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if key == "" {
		key = r.Header.Get("X-API-Key")
	}
	for k, t := range s.o.APIKeys {
		if subtle.ConstantTimeCompare([]byte(k), []byte(key)) == 1 {
			return t, "tenant:" + t, true
		}
	}
	return "", "", false
}

func clientIP(r *http.Request) string {
	if xf := r.Header.Get("X-Forwarded-For"); xf != "" {
		ip, _, _ := strings.Cut(xf, ",")
		return strings.TrimSpace(ip)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) upstreamError(w http.ResponseWriter, err error, attempts []router.Attempt) {
	status, code := 502, "upstream_error"
	if errors.Is(err, router.ErrNoProvider) {
		status, code = 503, "no_provider_available"
	}
	if errors.Is(err, context.Canceled) {
		status, code = 499, "client_closed_request"
	}
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": err.Error(), "type": code, "code": code, "attempts": attempts}})
}

func errResult(err error) string {
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	return "error"
}

func (s *Server) record(r *http.Request, oc outcome) {
	d := time.Since(oc.start)
	if oc.result != "" && oc.result != "unauthorized" && oc.result != "bad_request" {
		s.recent.add(recentEvent{Result: oc.result, Ms: float64(d.Microseconds()) / 1000, Provider: oc.provider, Similarity: oc.similarity, At: time.Now().UnixMilli()})
	}
	if s.o.Metrics != nil {
		s.o.Metrics.Requests.Inc(oc.result)
		s.o.Metrics.Duration.Observe(d.Seconds(), oc.result)
	}
	s.o.Log.Info("chat", "result", oc.result, "ms", fmtMs(d), "similarity", oc.similarity, "provider", oc.provider, "entry", oc.entryID, "tenant", oc.tenant, "remote", clientIP(r))
}

// ---- other endpoints ---------------------------------------------------

func (s *Server) models(w http.ResponseWriter, _ *http.Request) {
	data := []map[string]any{{"id": "recura", "object": "model", "owned_by": "recura"}}
	for _, p := range s.o.Router.Status() {
		data = append(data, map[string]any{"id": p.Name, "object": "model", "owned_by": "recura"})
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

func (s *Server) feedback(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.authenticate(r); !ok {
		apiError(w, 401, "invalid_api_key", "Missing or unknown API key.")
		return
	}
	var body struct {
		EntryID string `json:"entry_id"`
		Helpful *bool  `json:"helpful"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil || body.EntryID == "" {
		apiError(w, 400, "invalid_request", `Send {"entry_id": "...", "helpful": false}.`)
		return
	}
	if body.Helpful != nil && *body.Helpful {
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	if !s.o.Cache.Flag(r.Context(), body.EntryID) {
		apiError(w, 404, "not_found", "No cache entry with that id.")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "flagged": body.EntryID})
}

func (s *Server) admin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.o.AdminKey != "" {
			key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(key), []byte(s.o.AdminKey)) != 1 {
				apiError(w, 401, "invalid_admin_key", "This endpoint needs the admin key: Authorization: Bearer <RECURA_ADMIN_KEY>.")
				return
			}
		}
		h(w, r)
	}
}

func (s *Server) cacheStats(w http.ResponseWriter, _ *http.Request) {
	st := s.o.Cache.Stats()
	out := map[string]any{"cache": st, "embedder": s.o.Embedder.Name()}
	if s.o.Metrics != nil {
		req := map[string]float64{}
		for _, k := range []string{"hit", "coalesced", "miss", "bypass", "rate_limited", "error"} {
			req[k] = s.o.Metrics.Requests.Value(k)
		}
		out["requests"] = req
		if t := req["hit"] + req["coalesced"] + req["miss"]; t > 0 {
			out["hit_rate"] = (req["hit"] + req["coalesced"]) / t
		}
	}
	writeJSON(w, 200, out)
}

func (s *Server) getEntry(w http.ResponseWriter, r *http.Request) {
	e, ok := s.o.Cache.Get(r.PathValue("id"))
	if !ok {
		apiError(w, 404, "not_found", "No cache entry with that id.")
		return
	}
	e.Vector = nil
	writeJSON(w, 200, e)
}

func (s *Server) deleteEntry(w http.ResponseWriter, r *http.Request) {
	if !s.o.Cache.Delete(r.Context(), r.PathValue("id")) {
		apiError(w, 404, "not_found", "No cache entry with that id.")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) providers(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]any{"providers": s.o.Router.Status()})
}

func (s *Server) providerAction(w http.ResponseWriter, r *http.Request) {
	var disable bool
	switch r.PathValue("action") {
	case "disable":
		disable = true
	case "enable":
	default:
		apiError(w, 404, "not_found", "Use /disable or /enable.")
		return
	}
	if !s.o.Router.SetDisabled(r.PathValue("name"), disable) {
		apiError(w, 404, "not_found", "Unknown provider.")
		return
	}
	writeJSON(w, 200, map[string]any{"providers": s.o.Router.Status()})
}

// ---- wire helpers ----------------------------------------------------

func completion(model, text string, in, out int, meta map[string]any) map[string]any {
	return map[string]any{
		"id":      "chatcmpl-" + randID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{"index": 0, "message": map[string]string{"role": "assistant", "content": text}, "finish_reason": "stop"}},
		"usage":   map[string]int{"prompt_tokens": in, "completion_tokens": out, "total_tokens": in + out},
		"recura":  meta,
	}
}

type stream struct {
	w     http.ResponseWriter
	f     http.Flusher
	id    string
	model string
	mu    sync.Mutex
}

func newStream(w http.ResponseWriter, model string) *stream {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	f, _ := w.(http.Flusher)
	s := &stream{w: w, f: f, id: "chatcmpl-" + randID(), model: model}
	s.send(map[string]any{"role": "assistant", "content": ""}, nil, nil)
	return s
}

func (s *stream) send(delta map[string]any, finish any, extra map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	chunk := map[string]any{
		"id": s.id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": s.model,
		"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
	}
	for k, v := range extra {
		chunk[k] = v
	}
	raw, _ := json.Marshal(chunk)
	fmt.Fprintf(s.w, "data: %s\n\n", raw)
	if s.f != nil {
		s.f.Flush()
	}
}

func (s *stream) delta(t string) { s.send(map[string]any{"content": t}, nil, nil) }

func (s *stream) finish(meta map[string]any) {
	s.send(map[string]any{}, "stop", map[string]any{"recura": meta})
	s.done()
}

func (s *stream) fail(err error) {
	s.mu.Lock()
	raw, _ := json.Marshal(map[string]any{"error": map[string]string{"message": err.Error(), "type": "upstream_error"}})
	fmt.Fprintf(s.w, "data: %s\n\n", raw)
	s.mu.Unlock()
	s.done()
}

func (s *stream) done() {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprint(s.w, "data: [DONE]\n\n")
	if s.f != nil {
		s.f.Flush()
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func apiError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"message": msg, "type": code, "code": code}})
}

func fmtMs(d time.Duration) string {
	return strconv.FormatFloat(float64(d.Microseconds())/1000, 'f', 2, 64)
}

func randID() string {
	var b [8]byte
	_, _ = randRead(b[:])
	return hex.EncodeToString(b[:])
}
