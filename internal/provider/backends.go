package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// OpenAI talks to any OpenAI-compatible /chat/completions endpoint:
// OpenAI, Azure OpenAI, Groq, Together, OpenRouter, Ollama, vLLM, LM Studio.
type OpenAI struct {
	ID      string // name shown in metrics and headers
	BaseURL string // e.g. https://api.openai.com/v1
	APIKey  string
	Model   string
}

func (p *OpenAI) Name() string { return p.ID }

func (p *OpenAI) Complete(ctx context.Context, req Request, onDelta DeltaFunc) (Result, error) {
	body := map[string]any{"model": p.Model, "messages": req.Messages}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	stream := onDelta != nil
	if stream {
		body["stream"] = true
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	h := map[string]string{}
	if p.APIKey != "" {
		h["Authorization"] = "Bearer " + p.APIKey
	}
	resp, err := post(ctx, p.ID, strings.TrimRight(p.BaseURL, "/")+"/chat/completions", h, body)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	res := Result{Model: p.Model}
	if !stream {
		var out struct {
			Model   string `json:"model"`
			Choices []struct {
				Message Message `json:"message"`
			} `json:"choices"`
			Usage struct {
				Prompt     int `json:"prompt_tokens"`
				Completion int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return Result{}, &Error{Provider: p.ID, Message: "decode: " + err.Error(), Retryable: true}
		}
		if len(out.Choices) == 0 {
			return Result{}, &Error{Provider: p.ID, Message: "no choices", Retryable: true}
		}
		res.Text = out.Choices[0].Message.Content
		res.InputTokens, res.OutputTokens = out.Usage.Prompt, out.Usage.Completion
		if out.Model != "" {
			res.Model = out.Model
		}
		return res, nil
	}
	var sb strings.Builder
	err = sse(resp.Body, func(_, data string) error {
		if data == "[DONE]" {
			return errStop
		}
		var ch struct {
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				Prompt     int `json:"prompt_tokens"`
				Completion int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &ch) != nil {
			return nil
		}
		if ch.Model != "" {
			res.Model = ch.Model
		}
		if ch.Usage != nil {
			res.InputTokens, res.OutputTokens = ch.Usage.Prompt, ch.Usage.Completion
		}
		if len(ch.Choices) > 0 && ch.Choices[0].Delta.Content != "" {
			sb.WriteString(ch.Choices[0].Delta.Content)
			onDelta(ch.Choices[0].Delta.Content)
		}
		return nil
	})
	if err != nil && err != errStop {
		return Result{}, streamErr(ctx, p.ID, err)
	}
	res.Text = sb.String()
	return res, nil
}

// Anthropic talks to the Claude Messages API.
type Anthropic struct {
	ID      string
	BaseURL string // default https://api.anthropic.com
	APIKey  string
	Model   string
}

func (p *Anthropic) Name() string { return p.ID }

func (p *Anthropic) Complete(ctx context.Context, req Request, onDelta DeltaFunc) (Result, error) {
	var system []string
	var msgs []Message
	for _, m := range req.Messages {
		if m.Role == "system" {
			system = append(system, m.Content)
			continue
		}
		msgs = append(msgs, m)
	}
	maxTok := req.MaxTokens
	if maxTok <= 0 {
		maxTok = 1024
	}
	body := map[string]any{"model": p.Model, "max_tokens": maxTok, "messages": msgs}
	if len(system) > 0 {
		body["system"] = strings.Join(system, "\n\n")
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	stream := onDelta != nil
	if stream {
		body["stream"] = true
	}
	base := p.BaseURL
	if base == "" {
		base = "https://api.anthropic.com"
	}
	h := map[string]string{"x-api-key": p.APIKey, "anthropic-version": "2023-06-01"}
	resp, err := post(ctx, p.ID, strings.TrimRight(base, "/")+"/v1/messages", h, body)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	res := Result{Model: p.Model}
	if !stream {
		var out struct {
			Model   string `json:"model"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Usage struct {
				In  int `json:"input_tokens"`
				Out int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return Result{}, &Error{Provider: p.ID, Message: "decode: " + err.Error(), Retryable: true}
		}
		var sb strings.Builder
		for _, c := range out.Content {
			if c.Type == "text" {
				sb.WriteString(c.Text)
			}
		}
		res.Text, res.InputTokens, res.OutputTokens = sb.String(), out.Usage.In, out.Usage.Out
		return res, nil
	}
	var sb strings.Builder
	err = sse(resp.Body, func(event, data string) error {
		var ev struct {
			Type  string `json:"type"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
			Message struct {
				Usage struct {
					In int `json:"input_tokens"`
				} `json:"usage"`
			} `json:"message"`
			Usage struct {
				Out int `json:"output_tokens"`
			} `json:"usage"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(data), &ev) != nil {
			return nil
		}
		switch ev.Type {
		case "message_start":
			res.InputTokens = ev.Message.Usage.In
		case "content_block_delta":
			if ev.Delta.Type == "text_delta" && ev.Delta.Text != "" {
				sb.WriteString(ev.Delta.Text)
				onDelta(ev.Delta.Text)
			}
		case "message_delta":
			res.OutputTokens = ev.Usage.Out
		case "message_stop":
			return errStop
		case "error":
			return &Error{Provider: p.ID, Message: ev.Error.Type + ": " + ev.Error.Message, Retryable: ev.Error.Type == "overloaded_error"}
		}
		return nil
	})
	if err != nil && err != errStop {
		return Result{}, streamErr(ctx, p.ID, err)
	}
	res.Text = sb.String()
	return res, nil
}

// Gemini talks to the Google Gemini generateContent API. Responses are
// fetched whole and delivered as one delta.
type Gemini struct {
	ID      string
	BaseURL string // default https://generativelanguage.googleapis.com
	APIKey  string
	Model   string
}

func (p *Gemini) Name() string { return p.ID }

func (p *Gemini) Complete(ctx context.Context, req Request, onDelta DeltaFunc) (Result, error) {
	type part struct {
		Text string `json:"text"`
	}
	type content struct {
		Role  string `json:"role,omitempty"`
		Parts []part `json:"parts"`
	}
	var contents []content
	var system []part
	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			system = append(system, part{m.Content})
		case "assistant":
			contents = append(contents, content{Role: "model", Parts: []part{{m.Content}}})
		default:
			contents = append(contents, content{Role: "user", Parts: []part{{m.Content}}})
		}
	}
	body := map[string]any{"contents": contents}
	if len(system) > 0 {
		body["systemInstruction"] = content{Parts: system}
	}
	gc := map[string]any{}
	if req.MaxTokens > 0 {
		gc["maxOutputTokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		gc["temperature"] = *req.Temperature
	}
	if len(gc) > 0 {
		body["generationConfig"] = gc
	}
	base := p.BaseURL
	if base == "" {
		base = "https://generativelanguage.googleapis.com"
	}
	url := fmt.Sprintf("%s/v1beta/models/%s:generateContent", strings.TrimRight(base, "/"), p.Model)
	resp, err := post(ctx, p.ID, url, map[string]string{"x-goog-api-key": p.APIKey}, body)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	var out struct {
		Candidates []struct {
			Content content `json:"content"`
		} `json:"candidates"`
		Usage struct {
			In  int `json:"promptTokenCount"`
			Out int `json:"candidatesTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Result{}, &Error{Provider: p.ID, Message: "decode: " + err.Error(), Retryable: true}
	}
	if len(out.Candidates) == 0 {
		return Result{}, &Error{Provider: p.ID, Message: "no candidates (possibly blocked by safety settings)", Retryable: false}
	}
	var sb strings.Builder
	for _, pt := range out.Candidates[0].Content.Parts {
		sb.WriteString(pt.Text)
	}
	text := sb.String()
	if onDelta != nil && text != "" {
		onDelta(text)
	}
	return Result{Text: text, Model: p.Model, InputTokens: out.Usage.In, OutputTokens: out.Usage.Out}, nil
}

// Mock answers without calling any API. It lets Sieve run with zero
// configuration and makes fallback and load testing reproducible.
type Mock struct {
	ID       string
	Latency  time.Duration // base latency per request
	Jitter   time.Duration // extra random latency up to this value
	FailRate float64       // probability of a retryable failure, 0..1
	Answer   func(prompt string) string

	mu  sync.Mutex
	rng *rand.Rand
}

func (p *Mock) Name() string { return p.ID }

func (p *Mock) rand() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rng == nil {
		p.rng = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	return p.rng.Float64()
}

func (p *Mock) Complete(ctx context.Context, req Request, onDelta DeltaFunc) (Result, error) {
	if p.FailRate > 0 && p.rand() < p.FailRate {
		return Result{}, &Error{Provider: p.ID, Status: 503, Message: "mock failure", Retryable: true}
	}
	var prompt string
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			prompt = req.Messages[i].Content
			break
		}
	}
	text := "Mock answer to: " + prompt
	if p.Answer != nil {
		text = p.Answer(prompt)
	}
	total := p.Latency + time.Duration(p.rand()*float64(p.Jitter))
	words := strings.SplitAfter(text, " ")
	ttft := total / 3
	per := (total - ttft) / time.Duration(max(1, len(words)))
	if err := sleep(ctx, ttft); err != nil {
		return Result{}, err
	}
	for _, w := range words {
		if onDelta != nil {
			onDelta(w)
		}
		if err := sleep(ctx, per); err != nil {
			return Result{}, err
		}
	}
	return Result{Text: text, Model: "mock", InputTokens: len(prompt) / 4, OutputTokens: len(text) / 4}, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func streamErr(ctx context.Context, id string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if _, ok := err.(*Error); ok {
		return err
	}
	if err == io.ErrUnexpectedEOF {
		return &Error{Provider: id, Message: "stream ended early", Retryable: true}
	}
	return &Error{Provider: id, Message: "stream: " + err.Error(), Retryable: true}
}
