// Package router sends a request to the first healthy provider and falls
// back down the list on retryable failures, with a circuit breaker per
// provider so a failing backend is skipped instead of retried every time.
package router

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Prm01/recura/internal/provider"
)

// Breaker settings.
type Config struct {
	Timeout          time.Duration // per attempt; 0 = none
	FailureThreshold int           // consecutive failures that open the breaker
	Cooldown         time.Duration // how long an open breaker skips the provider
}

// Attempt records one provider call, for logs and response headers.
type Attempt struct {
	Provider string        `json:"provider"`
	Duration time.Duration `json:"duration"`
	Err      string        `json:"error,omitempty"`
	Skipped  bool          `json:"skipped,omitempty"`
}

// Observer is told about every attempt (used for metrics).
type Observer func(providerName string, d time.Duration, err error)

// Router is safe for concurrent use.
type Router struct {
	cfg       Config
	providers []provider.Provider
	mu        sync.Mutex
	state     map[string]*breaker
	observe   Observer
	now       func() time.Time
}

type breaker struct {
	fails     int
	openUntil time.Time
	disabled  bool // set by an operator through the admin API
}

// ErrNoProvider is returned when every provider is open or disabled.
var ErrNoProvider = errors.New("router: no healthy provider")

func New(cfg Config, providers []provider.Provider, observe Observer) *Router {
	if cfg.FailureThreshold <= 0 {
		cfg.FailureThreshold = 3
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = 30 * time.Second
	}
	r := &Router{cfg: cfg, providers: providers, state: map[string]*breaker{}, observe: observe, now: time.Now}
	for _, p := range providers {
		r.state[p.Name()] = &breaker{}
	}
	return r
}

// DeltaFunc receives streamed text together with the provider producing it.
type DeltaFunc func(providerName, delta string)

// Complete runs the request. If onDelta is non-nil the answer streams; once
// any text has been streamed the router can no longer fall back, because
// the client has already received part of an answer.
func (r *Router) Complete(ctx context.Context, req provider.Request, onDelta DeltaFunc) (provider.Result, string, []Attempt, error) {
	var attempts []Attempt
	var lastErr error
	for _, p := range r.providers {
		name := p.Name()
		if !r.available(name) {
			attempts = append(attempts, Attempt{Provider: name, Skipped: true})
			continue
		}
		actx, cancel := ctx, context.CancelFunc(func() {})
		if r.cfg.Timeout > 0 {
			actx, cancel = context.WithTimeout(ctx, r.cfg.Timeout)
		}
		streamed := false
		var cb provider.DeltaFunc
		if onDelta != nil {
			cb = func(d string) { streamed = true; onDelta(name, d) }
		}
		start := r.now()
		res, err := p.Complete(actx, req, cb)
		cancel()
		d := r.now().Sub(start)
		if r.observe != nil {
			r.observe(name, d, err)
		}
		a := Attempt{Provider: name, Duration: d}
		if err == nil {
			r.success(name)
			attempts = append(attempts, a)
			return res, name, attempts, nil
		}
		a.Err = err.Error()
		attempts = append(attempts, a)
		lastErr = err
		if ctx.Err() != nil {
			return provider.Result{}, name, attempts, ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) || provider.IsRetryable(err) {
			r.failure(name)
		}
		if streamed || !(provider.IsRetryable(err) || errors.Is(err, context.DeadlineExceeded)) {
			return provider.Result{}, name, attempts, err
		}
	}
	if lastErr == nil {
		lastErr = ErrNoProvider
	} else {
		lastErr = fmt.Errorf("all providers failed: %w", lastErr)
	}
	return provider.Result{}, "", attempts, lastErr
}

func (r *Router) available(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	b := r.state[name]
	return !b.disabled && !r.now().Before(b.openUntil)
}

func (r *Router) success(name string) {
	r.mu.Lock()
	r.state[name].fails = 0
	r.mu.Unlock()
}

func (r *Router) failure(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b := r.state[name]
	b.fails++
	if b.fails >= r.cfg.FailureThreshold {
		b.openUntil = r.now().Add(r.cfg.Cooldown)
		b.fails = 0
	}
}

// SetDisabled takes a provider out of rotation (or puts it back).
func (r *Router) SetDisabled(name string, disabled bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.state[name]
	if ok {
		b.disabled = disabled
		if !disabled {
			b.openUntil, b.fails = time.Time{}, 0
		}
	}
	return ok
}

// Status describes each provider for the admin API.
type Status struct {
	Name     string    `json:"name"`
	State    string    `json:"state"` // healthy, open, disabled
	OpenTill time.Time `json:"open_until,omitempty"`
}

func (r *Router) Status() []Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	out := make([]Status, 0, len(r.providers))
	for _, p := range r.providers {
		b := r.state[p.Name()]
		s := Status{Name: p.Name(), State: "healthy"}
		switch {
		case b.disabled:
			s.State = "disabled"
		case now.Before(b.openUntil):
			s.State, s.OpenTill = "open", b.openUntil
		}
		out = append(out, s)
	}
	return out
}
