// Package provider adapts model APIs (OpenAI-compatible, Anthropic,
// Gemini, and a mock) to one streaming interface.
package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Message is one chat turn.
type Message struct {
	Role    string `json:"role"` // system, user, assistant
	Content string `json:"content"`
}

// Request is a provider-neutral chat request.
type Request struct {
	Messages    []Message
	MaxTokens   int
	Temperature *float64
}

// Result is a completed answer.
type Result struct {
	Text         string
	Model        string
	InputTokens  int
	OutputTokens int
}

// DeltaFunc receives streamed text. It may be nil for a non-streaming call.
type DeltaFunc func(delta string)

// Provider is one model backend.
type Provider interface {
	Name() string
	Complete(ctx context.Context, req Request, onDelta DeltaFunc) (Result, error)
}

// Error is an upstream failure. Retryable errors let the router fall back
// to the next provider.
type Error struct {
	Provider  string
	Status    int
	Message   string
	Retryable bool
}

func (e *Error) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("%s: HTTP %d: %s", e.Provider, e.Status, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Provider, e.Message)
}

// IsRetryable reports whether err should trigger a fallback.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false // the client went away
	}
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Retryable
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.ErrUnexpectedEOF)
}

func statusRetryable(code int) bool {
	return code == 408 || code == 409 || code == 429 || code >= 500
}

var defaultClient = &http.Client{Timeout: 120 * time.Second}

func post(ctx context.Context, name, url string, headers map[string]string, body any) (*http.Response, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := defaultClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &Error{Provider: name, Message: err.Error(), Retryable: true}
	}
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		return nil, &Error{Provider: name, Status: resp.StatusCode, Message: strings.TrimSpace(string(msg)), Retryable: statusRetryable(resp.StatusCode)}
	}
	return resp, nil
}

// sse calls fn with the data payload of every server-sent event.
func sse(r io.Reader, fn func(event, data string) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	var event string
	var data strings.Builder
	flush := func() error {
		if data.Len() == 0 {
			event = ""
			return nil
		}
		d := strings.TrimSuffix(data.String(), "\n")
		data.Reset()
		err := fn(event, d)
		event = ""
		return err
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if err := flush(); err != nil {
				return err
			}
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(line[6:])
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimPrefix(line[5:], " "))
			data.WriteByte('\n')
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return flush()
}

var errStop = errors.New("stop")
