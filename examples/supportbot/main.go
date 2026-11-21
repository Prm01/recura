// Command supportbot is a small student-helpdesk chatbot that talks to a
// Recura gateway. It exists to answer one question with real data: when real
// people ask a real FAQ bot things, how often does Recura answer from cache,
// and do people find those cached answers helpful?
//
//	go run ./examples/supportbot            # chat UI on http://localhost:3000
//	go run ./examples/supportbot -report    # print results from the event log
//
// Everything is standard library. Each chat message is sent to Recura as a
// single-turn request (FAQ as the system prompt, the question as the user
// message), which is exactly what Recura can cache. Thumbs-down on an answer
// is forwarded to Recura's /v1/feedback so the bad entry stops being served.
package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed index.html
var indexHTML []byte

//go:embed faq.txt
var defaultFAQ string

const systemPrompt = `You are the student helpdesk assistant. Answer ONLY from the FAQ below, in one to three short sentences, in the same language the student used (English, Hindi or Hinglish). If the FAQ does not contain the answer, say you do not know and tell the student to contact the helpdesk. Never invent dates, fees, phone numbers or rules.

FAQ:
`

// event is one line of the JSONL log. Type is "chat" or "feedback".
type event struct {
	Type     string  `json:"type"`
	TS       string  `json:"ts"`
	ID       string  `json:"id"`
	Session  string  `json:"session,omitempty"`
	Question string  `json:"question,omitempty"`
	Cache    string  `json:"cache,omitempty"` // hit | miss | bypass | error
	Sim      float64 `json:"similarity,omitempty"`
	GwMs     float64 `json:"gateway_ms,omitempty"`
	RttMs    float64 `json:"rtt_ms,omitempty"`
	Provider string  `json:"provider,omitempty"`
	Entry    string  `json:"entry,omitempty"`
	Helpful  *bool   `json:"helpful,omitempty"`
}

type bot struct {
	recuraURL string
	recuraKey string
	system    string
	logPath   string
	client    *http.Client

	mu      sync.Mutex
	entries map[string]string // chat id -> recura entry id
}

func main() {
	addr := flag.String("addr", env("BOT_ADDR", ":3000"), "listen address")
	logPath := flag.String("log", env("BOT_LOG", "events.jsonl"), "event log path")
	report := flag.Bool("report", false, "print a results report from the log and exit")
	flag.Parse()

	if *report {
		printReport(*logPath)
		return
	}

	faq := defaultFAQ
	if p := os.Getenv("BOT_FAQ"); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			log.Fatalf("BOT_FAQ: %v", err)
		}
		faq = string(b)
	}
	var kept []string
	for _, line := range strings.Split(faq, "\n") {
		if t := strings.TrimSpace(line); t != "" && !strings.HasPrefix(t, "#") {
			kept = append(kept, t)
		}
	}
	faq = strings.Join(kept, "\n")

	b := &bot{
		recuraURL: strings.TrimRight(env("RECURA_URL", "http://localhost:8080"), "/"),
		recuraKey: os.Getenv("RECURA_KEY"),
		system:    systemPrompt + strings.TrimSpace(faq),
		logPath:   *logPath,
		client:    &http.Client{Timeout: 90 * time.Second},
		entries:   map[string]string{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	mux.HandleFunc("POST /api/chat", b.chat)
	mux.HandleFunc("POST /api/feedback", b.feedback)
	mux.HandleFunc("GET /api/stats", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(computeStats(readEvents(b.logPath)))
	})

	log.Printf("support bot on %s -> Recura at %s (log: %s)", *addr, b.recuraURL, b.logPath)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func newID() string {
	var x [6]byte
	rand.Read(x[:])
	return hex.EncodeToString(x[:])
}

func (b *bot) chat(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Message string `json:"message"`
		Session string `json:"session"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		jsonErr(w, 400, "bad request")
		return
	}
	msg := strings.TrimSpace(in.Message)
	if msg == "" || len(msg) > 500 {
		jsonErr(w, 400, "Ask a question of up to 500 characters.")
		return
	}
	session := in.Session
	if len(session) > 24 {
		session = session[:24]
	}

	body, _ := json.Marshal(map[string]any{
		"model": "recura",
		"messages": []map[string]string{
			{"role": "system", "content": b.system},
			{"role": "user", "content": msg},
		},
	})
	req, _ := http.NewRequestWithContext(r.Context(), "POST", b.recuraURL+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if b.recuraKey != "" {
		req.Header.Set("Authorization", "Bearer "+b.recuraKey)
	}
	// Recura rate-limits per client IP, so pass the student's address along
	// instead of letting every student share the bot's own bucket.
	req.Header.Set("X-Forwarded-For", clientIP(r))

	start := time.Now()
	resp, err := b.client.Do(req)
	rtt := float64(time.Since(start).Microseconds()) / 1000
	id := newID()
	ev := event{Type: "chat", TS: time.Now().UTC().Format(time.RFC3339), ID: id, Session: session, Question: msg, RttMs: rtt}
	if err != nil {
		ev.Cache = "error"
		b.record(ev)
		jsonErr(w, 502, "The gateway is not reachable right now.")
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode == 429 {
		ev.Cache = "error"
		b.record(ev)
		jsonErr(w, 429, "Too many questions too fast. Please wait a few seconds.")
		return
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if resp.StatusCode != 200 || json.Unmarshal(raw, &out) != nil || len(out.Choices) == 0 {
		ev.Cache = "error"
		b.record(ev)
		jsonErr(w, 502, "The model could not answer right now. Please try again.")
		return
	}

	ev.Cache = strings.ToLower(resp.Header.Get("X-Recura-Cache"))
	ev.Sim, _ = strconv.ParseFloat(resp.Header.Get("X-Recura-Similarity"), 64)
	ev.GwMs, _ = strconv.ParseFloat(resp.Header.Get("X-Recura-Latency-Ms"), 64)
	ev.Provider = resp.Header.Get("X-Recura-Provider")
	ev.Entry = resp.Header.Get("X-Recura-Entry")
	b.record(ev)
	if ev.Entry != "" {
		b.mu.Lock()
		b.entries[id] = ev.Entry
		b.mu.Unlock()
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"id":         id,
		"answer":     strings.TrimSpace(out.Choices[0].Message.Content),
		"cache":      ev.Cache,
		"similarity": ev.Sim,
		"gateway_ms": ev.GwMs,
		"rtt_ms":     rtt,
	})
}

func (b *bot) feedback(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID      string `json:"id"`
		Helpful *bool  `json:"helpful"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&in); err != nil || in.ID == "" || in.Helpful == nil {
		jsonErr(w, 400, "bad request")
		return
	}
	b.record(event{Type: "feedback", TS: time.Now().UTC().Format(time.RFC3339), ID: in.ID, Helpful: in.Helpful})

	// A thumbs-down tells Recura to stop serving that cached answer.
	if !*in.Helpful {
		b.mu.Lock()
		entry := b.entries[in.ID]
		b.mu.Unlock()
		if entry != "" {
			fb, _ := json.Marshal(map[string]any{"entry_id": entry, "helpful": false})
			req, _ := http.NewRequest("POST", b.recuraURL+"/v1/feedback", bytes.NewReader(fb))
			req.Header.Set("Content-Type", "application/json")
			if b.recuraKey != "" {
				req.Header.Set("Authorization", "Bearer "+b.recuraKey)
			}
			if resp, err := b.client.Do(req); err == nil {
				resp.Body.Close()
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

func (b *bot) record(e event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	f, err := os.OpenFile(b.logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Printf("log: %v", err)
		return
	}
	defer f.Close()
	line, _ := json.Marshal(e)
	f.Write(append(line, '\n'))
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

func jsonErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// ---- results ----------------------------------------------------------

func readEvents(path string) []event {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var e event
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out
}

type stats struct {
	Questions    int     `json:"questions"`
	Students     int     `json:"students"`
	Hits         int     `json:"hits"`
	Misses       int     `json:"misses"`
	Bypassed     int     `json:"bypassed"`
	Errors       int     `json:"errors"`
	HitRate      float64 `json:"hit_rate_pct"`
	MedianHitMs  float64 `json:"median_hit_ms"`
	MedianMissMs float64 `json:"median_miss_ms"`
	Rated        int     `json:"rated_answers"`
	HelpfulHit   string  `json:"helpful_cached"`
	HelpfulMiss  string  `json:"helpful_model"`
	DownCached   int     `json:"thumbs_down_on_cached"`
}

func computeStats(evs []event) stats {
	var s stats
	byID := map[string]event{}
	sessions := map[string]bool{}
	var hitMs, missMs []float64
	for _, e := range evs {
		if e.Type != "chat" {
			continue
		}
		byID[e.ID] = e
		if e.Session != "" {
			sessions[e.Session] = true
		}
		switch e.Cache {
		case "hit":
			s.Hits++
			hitMs = append(hitMs, e.RttMs)
		case "miss":
			s.Misses++
			missMs = append(missMs, e.RttMs)
		case "bypass":
			s.Bypassed++
		default:
			s.Errors++
		}
	}
	s.Questions = s.Hits + s.Misses + s.Bypassed
	s.Students = len(sessions)
	if s.Questions > 0 {
		s.HitRate = round1(100 * float64(s.Hits) / float64(s.Questions))
	}
	s.MedianHitMs, s.MedianMissMs = median(hitMs), median(missMs)

	// Last vote per answer wins.
	vote := map[string]bool{}
	for _, e := range evs {
		if e.Type == "feedback" && e.Helpful != nil {
			vote[e.ID] = *e.Helpful
		}
	}
	var hUp, hAll, mUp, mAll int
	for id, up := range vote {
		c, ok := byID[id]
		if !ok {
			continue
		}
		s.Rated++
		if c.Cache == "hit" {
			hAll++
			if up {
				hUp++
			} else {
				s.DownCached++
			}
		} else {
			mAll++
			if up {
				mUp++
			}
		}
	}
	s.HelpfulHit, s.HelpfulMiss = frac(hUp, hAll), frac(mUp, mAll)
	return s
}

func frac(a, b int) string {
	if b == 0 {
		return "no votes yet"
	}
	return fmt.Sprintf("%d of %d (%.0f%%)", a, b, 100*float64(a)/float64(b))
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	sort.Float64s(v)
	return round1(v[len(v)/2])
}

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }

func printReport(path string) {
	evs := readEvents(path)
	s := computeStats(evs)
	if s.Questions == 0 {
		fmt.Println("No questions logged yet in", path)
		return
	}
	fmt.Printf("Recura support-bot results\n==========================\n")
	fmt.Printf("Questions answered   %d from %d students\n", s.Questions, s.Students)
	fmt.Printf("From cache           %d (%.1f%%)\n", s.Hits, s.HitRate)
	fmt.Printf("Model calls          %d\n", s.Misses)
	fmt.Printf("Bypassed (personal)  %d\n", s.Bypassed)
	fmt.Printf("Failed requests      %d (not counted above)\n", s.Errors)
	fmt.Printf("Median round trip    %.1f ms cached, %.1f ms model call\n", s.MedianHitMs, s.MedianMissMs)
	fmt.Printf("Rated answers        %d\n", s.Rated)
	fmt.Printf("  helpful, cached    %s\n", s.HelpfulHit)
	fmt.Printf("  helpful, model     %s\n", s.HelpfulMiss)
	fmt.Printf("  thumbs-down on a cached answer: %d  (the closest real-world measure of a wrong cache hit)\n", s.DownCached)

	fmt.Println("\nMost repeated questions that hit the cache:")
	count := map[string]int{}
	for _, e := range evs {
		if e.Type == "chat" && e.Cache == "hit" {
			count[strings.ToLower(e.Question)]++
		}
	}
	type kv struct {
		q string
		n int
	}
	var top []kv
	for q, n := range count {
		top = append(top, kv{q, n})
	}
	sort.Slice(top, func(i, j int) bool { return top[i].n > top[j].n })
	for i, t := range top {
		if i == 5 {
			break
		}
		fmt.Printf("  %dx  %s\n", t.n, t.q)
	}
}
