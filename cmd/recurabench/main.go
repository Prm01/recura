// Command recurabench replays a realistic workload through a real Recura
// server (in-process HTTP, mock model with configurable latency) and
// reports hit rate, wrong-answer rate, latency and throughput.
//
//	go run ./cmd/recurabench                      # one run at the default threshold
//	go run ./cmd/recurabench -sweep               # compare thresholds
//	go run ./cmd/recurabench -n 5000 -c 64        # more load
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Prm01/recura/internal/cache"
	"github.com/Prm01/recura/internal/embed"
	"github.com/Prm01/recura/internal/privacy"
	"github.com/Prm01/recura/internal/provider"
	"github.com/Prm01/recura/internal/ratelimit"
	"github.com/Prm01/recura/internal/router"
	"github.com/Prm01/recura/internal/server"
)

var intents = map[string][]string{
	"reset_pw":     {"how do i reset my password", "i forgot my password", "password bhool gaya kya karu", "reset password please", "cant log in forgot password", "how to change my password"},
	"refund":       {"what is your refund policy", "can i get my money back", "how do refunds work", "refund kab milega", "i want a refund"},
	"cancel":       {"how do i cancel my subscription", "cancel my plan", "stop my subscription", "subscription cancel kaise kare"},
	"hours":        {"what are your support hours", "when is support open", "customer care timing kya hai"},
	"shipping":     {"how long does shipping take", "delivery kitne din mein hoti hai", "when will my package arrive", "shipping time"},
	"change_email": {"how do i change my email", "update my email address", "email change kaise kare"},
	"delete_acct":  {"how do i delete my account", "remove my account permanently", "account delete kaise kare"},
}

var (
	intentOrder = []string{"reset_pw", "refund", "cancel", "hours", "shipping", "change_email", "delete_acct"}
	templates   = []string{"explain %s simply", "what is %s used for", "give me an example of %s", "pros and cons of %s"}
	topics      = []string{"kubernetes", "docker", "redis", "kafka", "graphql", "oauth", "rust", "big o notation", "cap theorem", "vector databases", "tcp vs udp", "load balancers"}
	prefixes    = []string{"", "", "hey ", "hi team ", "quick question ", "urgent ", "pls help "}
	suffixes    = []string{"", "", "?", " asap", " thanks", " plz"}
)

type query struct {
	text, intent string
}

func workload(n int, seed int64) []query {
	r := rand.New(rand.NewSource(seed))
	weights := make([]float64, len(intentOrder))
	var total float64
	for i := range weights {
		weights[i] = 1 / float64(i+1) // Zipf: a few questions dominate
		total += weights[i]
	}
	noisy := func(s string) string {
		w := strings.Fields(s)
		if r.Float64() < 0.35 { // swap two letters in one word: a typo
			i := r.Intn(len(w))
			if x := []rune(w[i]); len(x) > 3 {
				j := 1 + r.Intn(len(x)-2)
				x[j-1], x[j] = x[j], x[j-1]
				w[i] = string(x)
			}
		}
		return prefixes[r.Intn(len(prefixes))] + strings.Join(w, " ") + suffixes[r.Intn(len(suffixes))]
	}
	out := make([]query, 0, n)
	for i := 0; i < n; i++ {
		x := r.Float64()
		switch {
		case x < 0.10:
			out = append(out, query{fmt.Sprintf("where is my order #%d", 1000+r.Intn(9000)), "personal"})
		case x < 0.34:
			t, p := r.Intn(len(templates)), r.Intn(len(topics))
			out = append(out, query{noisy(fmt.Sprintf(templates[t], topics[p])), fmt.Sprintf("lt%d_%d", t, p)})
		default:
			p := r.Float64() * total
			k := 0
			for p > weights[k] {
				p -= weights[k]
				k++
			}
			list := intents[intentOrder[k]]
			out = append(out, query{noisy(list[r.Intn(len(list))]), intentOrder[k]})
		}
	}
	return out
}

type sample struct {
	cache  string
	ms     float64
	wrong  bool
	status int
}

type report struct {
	Threshold   float64
	N           int
	HitRate     float64
	WrongRate   float64
	BypassRate  float64
	P50Hit      float64
	P99Hit      float64
	P50Miss     float64
	P50All      float64
	P95All      float64
	Throughput  float64
	CallsSaved  int
	ModelCalls  int
	BaselineP50 float64
}

func run(threshold float64, n, conc int, latency, jitter time.Duration, seed int64, coalesce bool) report {
	syn := map[string]string{}
	if raw, err := os.ReadFile("examples/synonyms-hinglish.json"); err == nil {
		_ = json.Unmarshal(raw, &syn)
	}
	c := cache.New(cache.Config{Threshold: float32(threshold)}, nil, nil)
	det, _ := privacy.New(nil)
	m := server.NewMetrics(c)
	srv := server.New(server.Options{
		Cache:      c,
		Embedder:   embed.NewHash(512, embed.DefaultStopwords, syn),
		Router:     router.New(router.Config{}, []provider.Provider{&provider.Mock{ID: "mock", Latency: latency, Jitter: jitter}}, m.RouterObserver()),
		Limiter:    ratelimit.NewMemory(ratelimit.Config{Capacity: 1e9, RefillPerSec: 1e9}),
		Privacy:    det,
		Metrics:    m,
		Log:        quietLogger(),
		NoCoalesce: !coalesce,
	})
	ts := httptest.NewServer(srv)
	defer ts.Close()
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: conc * 2}}

	qs := workload(n, seed)
	intentOf := map[string]string{}
	for _, q := range qs {
		intentOf[q.text] = q.intent
	}
	samples := make([]sample, len(qs))
	jobs := make(chan int)
	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				q := qs[i]
				body, _ := json.Marshal(map[string]any{"model": "bench", "messages": []map[string]string{{"role": "user", "content": q.text}}})
				t0 := time.Now()
				resp, err := client.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
				if err != nil {
					samples[i] = sample{status: -1}
					continue
				}
				var out struct {
					Recura struct {
						Matched string `json:"matched_prompt"`
					} `json:"recura"`
				}
				_ = json.NewDecoder(resp.Body).Decode(&out)
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				s := sample{cache: resp.Header.Get("X-Recura-Cache"), ms: float64(time.Since(t0).Microseconds()) / 1000, status: resp.StatusCode}
				if s.cache == "HIT" {
					s.wrong = intentOf[out.Recura.Matched] != q.intent
				}
				samples[i] = s
			}
		}()
	}
	for i := range qs {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	elapsed := time.Since(start)

	rep := report{Threshold: threshold, N: n, Throughput: float64(n) / elapsed.Seconds()}
	var hits, misses, all []float64
	var wrong, bypass int
	for _, s := range samples {
		all = append(all, s.ms)
		switch s.cache {
		case "HIT":
			hits = append(hits, s.ms)
			if s.wrong {
				wrong++
			}
		case "MISS":
			misses = append(misses, s.ms)
		case "BYPASS":
			bypass++
		}
	}
	rep.HitRate = float64(len(hits)) / float64(n)
	rep.WrongRate = float64(wrong) / float64(n)
	rep.BypassRate = float64(bypass) / float64(n)
	rep.P50Hit, rep.P99Hit = pct(hits, 50), pct(hits, 99)
	rep.P50Miss = pct(misses, 50)
	rep.P50All, rep.P95All = pct(all, 50), pct(all, 95)
	rep.CallsSaved = len(hits)
	rep.ModelCalls = n - len(hits)
	return rep
}

func pct(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	i := int(float64(len(s)-1) * p / 100)
	return s[i]
}

func ms(v float64) string {
	if v < 1 {
		return fmt.Sprintf("%.2f ms", v)
	}
	if v < 1000 {
		return fmt.Sprintf("%.0f ms", v)
	}
	return fmt.Sprintf("%.2f s", v/1000)
}

func main() {
	n := flag.Int("n", 2000, "requests to replay")
	conc := flag.Int("c", 32, "concurrent clients")
	th := flag.Float64("threshold", 0.85, "similarity threshold")
	lat := flag.Duration("latency", 800*time.Millisecond, "mock model base latency")
	jit := flag.Duration("jitter", 600*time.Millisecond, "mock model extra random latency")
	sweep := flag.Bool("sweep", false, "compare several thresholds")
	seed := flag.Int64("seed", 7, "workload seed")
	coal := flag.Bool("coalesce", true, "merge concurrent similar misses into one model call")
	flag.Parse()

	fmt.Printf("Workload: %d requests, %d concurrent clients, mock model %v + up to %v jitter\n", *n, *conc, *lat, *jit)
	fmt.Printf("Mix: support FAQs with typos and Hinglish (66%%), developer long tail (24%%), personal prompts (10%%)\n\n")
	if *sweep {
		fmt.Println("| Threshold | From cache | Wrong answers | Model calls avoided | Median latency | p95 latency |")
		fmt.Println("|---|---|---|---|---|---|")
		for _, t := range []float64{0.5, 0.6, 0.7, 0.75, 0.8, 0.85, 0.9, 0.95} {
			r := run(t, *n, *conc, *lat, *jit, *seed, *coal)
			fmt.Printf("| %.2f | %.1f%% | %.1f%% | %d of %d | %s | %s |\n", t, r.HitRate*100, r.WrongRate*100, r.CallsSaved, r.N, ms(r.P50All), ms(r.P95All))
		}
		return
	}
	r := run(*th, *n, *conc, *lat, *jit, *seed, *coal)
	fmt.Printf("Threshold            %.2f\n", r.Threshold)
	fmt.Printf("Served from cache    %.1f%%  (%d model calls avoided)\n", r.HitRate*100, r.CallsSaved)
	fmt.Printf("Wrong answers        %.1f%%\n", r.WrongRate*100)
	fmt.Printf("Bypassed (personal)  %.1f%%\n", r.BypassRate*100)
	fmt.Printf("Cache hit latency    p50 %s, p99 %s\n", ms(r.P50Hit), ms(r.P99Hit))
	fmt.Printf("Model calls made     %d\n", r.ModelCalls-int(r.BypassRate*float64(r.N)+0.5))
	fmt.Printf("Model call latency   p50 %s\n", ms(r.P50Miss))
	fmt.Printf("All requests         p50 %s, p95 %s\n", ms(r.P50All), ms(r.P95All))
	fmt.Printf("Throughput           %.0f req/s\n", r.Throughput)
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
