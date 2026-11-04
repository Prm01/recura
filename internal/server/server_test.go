package server

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Prm01/recura/internal/cache"
	"github.com/Prm01/recura/internal/embed"
	"github.com/Prm01/recura/internal/privacy"
	"github.com/Prm01/recura/internal/provider"
	"github.com/Prm01/recura/internal/ratelimit"
	"github.com/Prm01/recura/internal/router"
)

func newTestServer(t *testing.T, opts func(*Options)) (*httptest.Server, *Options) {
	t.Helper()
	c := cache.New(cache.Config{Threshold: 0.8}, nil, nil)
	det, _ := privacy.New(nil)
	m := NewMetrics(c)
	o := Options{
		Cache:    c,
		Embedder: embed.NewHash(512, embed.DefaultStopwords, nil),
		Router:   router.New(router.Config{}, []provider.Provider{&provider.Mock{ID: "mock", Latency: 5 * time.Millisecond}}, m.RouterObserver()),
		Limiter:  ratelimit.NewMemory(ratelimit.Config{Capacity: 100, RefillPerSec: 100}),
		Privacy:  det,
		Metrics:  m,
	}
	if opts != nil {
		opts(&o)
	}
	ts := httptest.NewServer(New(o))
	t.Cleanup(ts.Close)
	return ts, &o
}

func chat(t *testing.T, url, body string, hdr map[string]string) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", url+"/v1/chat/completions", strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func content(out map[string]any) string {
	ch := out["choices"].([]any)[0].(map[string]any)
	return ch["message"].(map[string]any)["content"].(string)
}

func TestMissThenSemanticHit(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	r1, o1 := chat(t, ts.URL, `{"messages":[{"role":"user","content":"How do I reset my password?"}]}`, nil)
	if r1.Header.Get("X-Recura-Cache") != "MISS" || r1.Header.Get("X-Recura-Provider") != "mock" {
		t.Fatalf("first request should miss: %v", r1.Header)
	}
	r2, o2 := chat(t, ts.URL, `{"messages":[{"role":"user","content":"pls how to reset my password"}]}`, nil)
	if r2.Header.Get("X-Recura-Cache") != "HIT" {
		t.Fatalf("paraphrase should hit, similarity %s", r2.Header.Get("X-Recura-Similarity"))
	}
	if content(o1) != content(o2) {
		t.Fatal("hit must return the cached answer")
	}
	if o2["recura"].(map[string]any)["entry_id"] != r1.Header.Get("X-Recura-Entry") {
		t.Fatal("hit should name the entry it matched")
	}
}

func TestSystemPromptPartitions(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	chat(t, ts.URL, `{"messages":[{"role":"system","content":"Answer in French"},{"role":"user","content":"what is redis"}]}`, nil)
	r, _ := chat(t, ts.URL, `{"messages":[{"role":"user","content":"what is redis"}]}`, nil)
	if r.Header.Get("X-Recura-Cache") != "MISS" {
		t.Fatal("a different system prompt must not share cache entries")
	}
}

func TestBypassCases(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	cases := map[string]string{
		"personal":   `{"messages":[{"role":"user","content":"where is my order #4417"}]}`,
		"multi-turn": `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":"and redis?"}]}`,
	}
	for name, body := range cases {
		if r, _ := chat(t, ts.URL, body, nil); r.Header.Get("X-Recura-Cache") != "BYPASS" {
			t.Errorf("%s should bypass, got %s", name, r.Header.Get("X-Recura-Cache"))
		}
	}
	if r, _ := chat(t, ts.URL, `{"messages":[{"role":"user","content":"explain dns"}]}`, map[string]string{"X-Recura-Cache": "off"}); r.Header.Get("X-Recura-Cache") != "BYPASS" {
		t.Error("X-Recura-Cache: off should bypass")
	}
}

func TestStreaming(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	for _, want := range []string{"MISS", "HIT"} {
		resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"stream":true,"messages":[{"role":"user","content":"explain kafka simply"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		if resp.Header.Get("Content-Type") != "text/event-stream" || resp.Header.Get("X-Recura-Cache") != want {
			t.Fatalf("want SSE %s, got %v", want, resp.Header)
		}
		var text strings.Builder
		var done bool
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			line := strings.TrimPrefix(sc.Text(), "data: ")
			if line == "[DONE]" {
				done = true
				break
			}
			var ch struct {
				Choices []struct {
					Delta struct{ Content string } `json:"delta"`
				} `json:"choices"`
			}
			if json.Unmarshal([]byte(line), &ch) == nil && len(ch.Choices) > 0 {
				text.WriteString(ch.Choices[0].Delta.Content)
			}
		}
		resp.Body.Close()
		if !done || !strings.Contains(text.String(), "kafka") {
			t.Fatalf("%s: stream incomplete: done=%v text=%q", want, done, text.String())
		}
	}
}

func TestAuthAndRateLimit(t *testing.T) {
	ts, _ := newTestServer(t, func(o *Options) {
		o.APIKeys = map[string]string{"k1": "acme"}
		o.Limiter = ratelimit.NewMemory(ratelimit.Config{Capacity: 2, RefillPerSec: 0.001})
	})
	if r, _ := chat(t, ts.URL, `{"messages":[{"role":"user","content":"x"}]}`, nil); r.StatusCode != 401 {
		t.Fatalf("missing key: want 401, got %d", r.StatusCode)
	}
	auth := map[string]string{"Authorization": "Bearer k1"}
	for i := 0; i < 2; i++ {
		if r, _ := chat(t, ts.URL, `{"messages":[{"role":"user","content":"x"}]}`, auth); r.StatusCode != 200 {
			t.Fatalf("request %d: want 200, got %d", i, r.StatusCode)
		}
	}
	r, out := chat(t, ts.URL, `{"messages":[{"role":"user","content":"x"}]}`, auth)
	if r.StatusCode != 429 || r.Header.Get("Retry-After") == "" || out["error"] == nil {
		t.Fatalf("want 429 with Retry-After, got %d %v", r.StatusCode, r.Header)
	}
}

func TestFeedbackFlags(t *testing.T) {
	ts, o := newTestServer(t, nil)
	r1, _ := chat(t, ts.URL, `{"messages":[{"role":"user","content":"what is your refund policy"}]}`, nil)
	id := r1.Header.Get("X-Recura-Entry")
	resp, _ := http.Post(ts.URL+"/v1/feedback", "application/json", strings.NewReader(`{"entry_id":"`+id+`","helpful":false}`))
	if resp.StatusCode != 200 {
		t.Fatalf("feedback: %d", resp.StatusCode)
	}
	if r, _ := chat(t, ts.URL, `{"messages":[{"role":"user","content":"what is your refund policy"}]}`, nil); r.Header.Get("X-Recura-Cache") != "MISS" {
		t.Fatal("flagged entry must not be served")
	}
	if o.Cache.Stats().Flagged != 1 {
		t.Fatal("stats should count the flag")
	}
}

func TestAdminAndBadRequests(t *testing.T) {
	ts, _ := newTestServer(t, func(o *Options) { o.AdminKey = "secret" })
	if resp, _ := http.Get(ts.URL + "/v1/cache/stats"); resp.StatusCode != 401 {
		t.Fatal("stats need the admin key")
	}
	req, _ := http.NewRequest("GET", ts.URL+"/v1/cache/stats", nil)
	req.Header.Set("Authorization", "Bearer secret")
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 200 {
		t.Fatal("admin key should open stats")
	}
	for _, body := range []string{`not json`, `{"messages":[]}`, `{"messages":[{"role":"robot","content":"x"}]}`} {
		if r, _ := chat(t, ts.URL, body, nil); r.StatusCode != 400 {
			t.Errorf("%s: want 400, got %d", body, r.StatusCode)
		}
	}
	if r, _ := chat(t, ts.URL, `{"messages":[{"role":"user","content":[{"type":"text","text":"parts work"}]}]}`, nil); r.StatusCode != 200 {
		t.Error("array-of-parts content should be accepted")
	}
}

func TestAllProvidersDown(t *testing.T) {
	ts, o := newTestServer(t, nil)
	o.Router.SetDisabled("mock", true)
	if r, _ := chat(t, ts.URL, `{"messages":[{"role":"user","content":"anything"}]}`, nil); r.StatusCode != 503 {
		t.Fatalf("want 503, got %d", r.StatusCode)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	chat(t, ts.URL, `{"messages":[{"role":"user","content":"hello there"}]}`, nil)
	resp, _ := http.Get(ts.URL + "/metrics")
	var b strings.Builder
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		b.WriteString(sc.Text() + "\n")
	}
	for _, want := range []string{`recura_requests_total{result="miss"} 1`, `recura_provider_requests_total{provider="mock",status="ok"} 1`, "recura_cache_entries 1"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}

func TestCoalescing(t *testing.T) {
	slow := &provider.Mock{ID: "mock", Latency: 300 * time.Millisecond}
	ts, o := newTestServer(t, func(o *Options) {
		o.Router = router.New(router.Config{}, []provider.Provider{slow}, o.Metrics.RouterObserver())
	})
	var wg sync.WaitGroup
	results := make([]string, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			prompts := []string{"how do i cancel my subscription", "pls how do i cancel my subscription"}
			r, out := chat(t, ts.URL, `{"messages":[{"role":"user","content":"`+prompts[i%2]+`"}]}`, nil)
			results[i] = r.Header.Get("X-Recura-Cache") + "/" + r.Header.Get("X-Recura-Coalesced")
			if out["choices"] == nil {
				t.Errorf("request %d got no answer", i)
			}
		}(i)
	}
	wg.Wait()
	if calls := o.Metrics.ProviderCalls.Value("mock", "ok"); calls != 1 {
		t.Fatalf("10 concurrent similar requests should make 1 model call, made %v (%v)", calls, results)
	}
	if got := o.Metrics.Requests.Value("coalesced"); got != 9 {
		t.Fatalf("want 9 coalesced followers, got %v", got)
	}
}

func TestCoalescingLeaderFailure(t *testing.T) {
	failing := &provider.Mock{ID: "mock", Latency: 100 * time.Millisecond, FailRate: 1}
	ts, _ := newTestServer(t, func(o *Options) {
		o.Router = router.New(router.Config{}, []provider.Provider{failing}, nil)
	})
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r, _ := chat(t, ts.URL, `{"messages":[{"role":"user","content":"what is kafka"}]}`, nil); r.StatusCode != 502 {
				t.Errorf("followers of a failed leader should try themselves and report the error, got %d", r.StatusCode)
			}
		}()
	}
	wg.Wait()
}

func TestPublicStatsAndPages(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	chat(t, ts.URL, `{"messages":[{"role":"user","content":"how do i reset my password"}]}`, nil)
	chat(t, ts.URL, `{"messages":[{"role":"user","content":"pls how do i reset my password"}]}`, nil)
	resp, err := http.Get(ts.URL + "/v1/public/stats")
	if err != nil {
		t.Fatal(err)
	}
	var st struct {
		HitRate  float64 `json:"hit_rate"`
		Cache    int     `json:"cache_entries"`
		Median   map[string]*float64
		Recent   []map[string]any `json:"recent"`
		Requests map[string]float64
	}
	_ = json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if st.HitRate != 0.5 || st.Cache != 1 || len(st.Recent) != 2 || st.Recent[0]["result"] != "hit" {
		t.Fatalf("unexpected public stats: %+v", st)
	}
	for _, e := range st.Recent {
		if _, leaks := e["prompt"]; leaks {
			t.Fatal("public stats must never include prompt text")
		}
	}
	for path, want := range map[string]string{"/": "Stop paying twice", "/console": "Recura Console", "/docs": "Recura documentation", "/site.css": "--accent", "/fonts.css": "@font-face"} {
		r, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		sc := bufio.NewScanner(r.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			b.WriteString(sc.Text())
		}
		r.Body.Close()
		if r.StatusCode != 200 || !strings.Contains(b.String(), want) {
			t.Errorf("%s: status %d, missing %q", path, r.StatusCode, want)
		}
	}
}

func TestDemoVideoServedWithRanges(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	req, _ := http.NewRequest("GET", ts.URL+"/media/recura-demo.mp4", nil)
	req.Header.Set("Range", "bytes=0-15")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || resp.Header.Get("Content-Type") != "video/mp4" {
		t.Fatalf("video must support range requests for seeking: status %d, type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}
