package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOpenAIStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "bad", 400)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, tok := range []string{"Hel", "lo"} {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", tok)
		}
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	p := &OpenAI{ID: "oa", BaseURL: srv.URL + "/v1", APIKey: "k", Model: "m"}
	var got strings.Builder
	res, err := p.Complete(context.Background(), Request{Messages: []Message{{"user", "hi"}}}, func(d string) { got.WriteString(d) })
	if err != nil || res.Text != "Hello" || got.String() != "Hello" || res.OutputTokens != 2 {
		t.Fatalf("got %+v %q %v", res, got.String(), err)
	}
}

func TestAnthropicStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "k" || r.Header.Get("anthropic-version") == "" {
			http.Error(w, "bad", 401)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5}}}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi \"}}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"there\"}}\n\n")
		fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":2}}\n\n")
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()
	p := &Anthropic{ID: "an", BaseURL: srv.URL, APIKey: "k", Model: "m"}
	res, err := p.Complete(context.Background(), Request{Messages: []Message{{"system", "be nice"}, {"user", "hi"}}}, func(string) {})
	if err != nil || res.Text != "Hi there" || res.InputTokens != 5 || res.OutputTokens != 2 {
		t.Fatalf("got %+v %v", res, err)
	}
}

func TestHTTPErrorsClassified(t *testing.T) {
	for code, retry := range map[int]bool{400: false, 401: false, 429: true, 500: true, 503: true} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "x", code) }))
		p := &OpenAI{ID: "oa", BaseURL: srv.URL, Model: "m"}
		_, err := p.Complete(context.Background(), Request{}, nil)
		srv.Close()
		if IsRetryable(err) != retry {
			t.Errorf("HTTP %d: retryable=%v, want %v (%v)", code, IsRetryable(err), retry, err)
		}
	}
}

func TestMockCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := (&Mock{ID: "m", Latency: time.Second}).Complete(ctx, Request{Messages: []Message{{"user", "x"}}}, nil)
	if err == nil {
		t.Fatal("mock should honour cancellation")
	}
}
