package router

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Prm01/recura/internal/provider"
)

type fake struct {
	name   string
	err    error
	stream string
	calls  int
}

func (f *fake) Name() string { return f.name }
func (f *fake) Complete(_ context.Context, _ provider.Request, d provider.DeltaFunc) (provider.Result, error) {
	f.calls++
	if f.stream != "" && d != nil {
		d(f.stream)
	}
	if f.err != nil {
		return provider.Result{}, f.err
	}
	return provider.Result{Text: "ok from " + f.name}, nil
}

var retryable = &provider.Error{Provider: "x", Status: 503, Retryable: true}

func TestFallback(t *testing.T) {
	a := &fake{name: "a", err: retryable}
	b := &fake{name: "b"}
	r := New(Config{}, []provider.Provider{a, b}, nil)
	res, used, attempts, err := r.Complete(context.Background(), provider.Request{}, nil)
	if err != nil || used != "b" || res.Text != "ok from b" || len(attempts) != 2 {
		t.Fatalf("expected fallback to b, got %q %v %+v", used, err, attempts)
	}
}

func TestNonRetryableStops(t *testing.T) {
	a := &fake{name: "a", err: &provider.Error{Status: 400, Retryable: false}}
	b := &fake{name: "b"}
	r := New(Config{}, []provider.Provider{a, b}, nil)
	if _, _, _, err := r.Complete(context.Background(), provider.Request{}, nil); err == nil || b.calls != 0 {
		t.Fatal("a 400 is the caller's fault and must not fall back")
	}
}

func TestNoFallbackAfterStreaming(t *testing.T) {
	a := &fake{name: "a", err: retryable, stream: "partial"}
	b := &fake{name: "b"}
	r := New(Config{}, []provider.Provider{a, b}, nil)
	_, _, _, err := r.Complete(context.Background(), provider.Request{}, func(string, string) {})
	if err == nil || b.calls != 0 {
		t.Fatal("once text has streamed, the router must not switch providers")
	}
}

func TestBreakerOpensAndRecovers(t *testing.T) {
	a := &fake{name: "a", err: retryable}
	b := &fake{name: "b"}
	r := New(Config{FailureThreshold: 2, Cooldown: time.Minute}, []provider.Provider{a, b}, nil)
	now := time.Now()
	r.now = func() time.Time { return now }
	for i := 0; i < 2; i++ {
		_, _, _, _ = r.Complete(context.Background(), provider.Request{}, nil)
	}
	_, _, attempts, _ := r.Complete(context.Background(), provider.Request{}, nil)
	if !attempts[0].Skipped || a.calls != 2 {
		t.Fatalf("breaker should skip a after 2 failures: %+v", attempts)
	}
	if r.Status()[0].State != "open" {
		t.Fatal("status should report open")
	}
	now = now.Add(2 * time.Minute)
	a.err = nil
	_, used, _, _ := r.Complete(context.Background(), provider.Request{}, nil)
	if used != "a" {
		t.Fatal("after cooldown a should be tried again")
	}
}

func TestAllDown(t *testing.T) {
	a := &fake{name: "a"}
	r := New(Config{}, []provider.Provider{a}, nil)
	r.SetDisabled("a", true)
	_, _, _, err := r.Complete(context.Background(), provider.Request{}, nil)
	if !errors.Is(err, ErrNoProvider) {
		t.Fatalf("want ErrNoProvider, got %v", err)
	}
}
