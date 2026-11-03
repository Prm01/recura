package ratelimit

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Prm01/sieve/internal/resp"
)

func TestMemoryBucket(t *testing.T) {
	m := NewMemory(Config{Capacity: 3, RefillPerSec: 1})
	now := time.Now()
	m.now = func() time.Time { return now }
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if d, _ := m.Allow(ctx, "k"); !d.Allowed {
			t.Fatalf("request %d should pass", i)
		}
	}
	d, _ := m.Allow(ctx, "k")
	if d.Allowed || d.RetryAfter <= 0 || d.RetryAfter > time.Second {
		t.Fatalf("4th request should be limited with retry <=1s, got %+v", d)
	}
	if d, _ := m.Allow(ctx, "other"); !d.Allowed {
		t.Fatal("keys must have separate buckets")
	}
	now = now.Add(1500 * time.Millisecond)
	if d, _ := m.Allow(ctx, "k"); !d.Allowed {
		t.Fatal("a token should have refilled")
	}
}

type failing struct{}

func (failing) Allow(context.Context, string) (Decision, error) { return Decision{}, resp.Err("down") }

func TestFailOpen(t *testing.T) {
	d, err := FailOpen{failing{}}.Allow(context.Background(), "k")
	if !d.Allowed || err == nil {
		t.Fatal("fail-open should allow and still report the error")
	}
}

func TestRedisBucket(t *testing.T) {
	url := os.Getenv("SIEVE_TEST_REDIS")
	if url == "" {
		t.Skip("SIEVE_TEST_REDIS not set")
	}
	ctx := context.Background()
	c, err := resp.Dial(ctx, url, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r := &Redis{C: c, Cfg: Config{Capacity: 2, RefillPerSec: 0.5}, Prefix: "sievetest:rl:" + time.Now().Format("150405.000") + ":"}
	a, _ := r.Allow(ctx, "k")
	b, _ := r.Allow(ctx, "k")
	x, err := r.Allow(ctx, "k")
	if err != nil || !a.Allowed || !b.Allowed || x.Allowed {
		t.Fatalf("want pass, pass, limited; got %v %v %v (%v)", a.Allowed, b.Allowed, x.Allowed, err)
	}
	if x.RetryAfter <= 0 {
		t.Fatal("limited decision needs a retry-after")
	}
}
