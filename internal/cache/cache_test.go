package cache

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
	"time"
)

func unit(x, y float32) []float32 { return []float32{x, y} }

func TestLookupThreshold(t *testing.T) {
	c := New(Config{Threshold: 0.9}, nil, nil)
	ctx := context.Background()
	c.Insert(ctx, &Entry{Partition: "p", Prompt: "a", Response: "A", Vector: unit(1, 0)})
	if m := c.Lookup("p", unit(0.99, 0.141)); !m.Hit {
		t.Fatalf("expected hit, score %.3f", m.Score)
	}
	m := c.Lookup("p", unit(0.6, 0.8))
	if m.Hit || m.Entry == nil {
		t.Fatalf("expected near-miss with nearest entry reported, got %+v", m)
	}
	if m := c.Lookup("other", unit(1, 0)); m.Entry != nil {
		t.Fatal("partitions must be isolated")
	}
}

func TestTTLFlagAndPinned(t *testing.T) {
	c := New(Config{Threshold: 0.5, TTL: time.Hour}, nil, nil)
	now := time.Now()
	c.now = func() time.Time { return now }
	ctx := context.Background()
	c.Insert(ctx, &Entry{ID: "old", Partition: "p", Vector: unit(1, 0)})
	c.Insert(ctx, &Entry{ID: "pin", Partition: "q", Vector: unit(1, 0), Pinned: true})
	now = now.Add(2 * time.Hour)
	if c.Lookup("p", unit(1, 0)).Entry != nil {
		t.Fatal("expired entry served")
	}
	if !c.Lookup("q", unit(1, 0)).Hit {
		t.Fatal("pinned entry should not expire")
	}
	c.Flag(ctx, "pin")
	if c.Lookup("q", unit(1, 0)).Entry != nil {
		t.Fatal("flagged entry served")
	}
}

func TestLRUEviction(t *testing.T) {
	c := New(Config{Threshold: 0.5, MaxEntries: 2}, nil, nil)
	now := time.Now()
	c.now = func() time.Time { now = now.Add(time.Second); return now }
	ctx := context.Background()
	c.Insert(ctx, &Entry{ID: "a", Partition: "p", Vector: unit(1, 0)})
	c.Insert(ctx, &Entry{ID: "b", Partition: "p", Vector: unit(0, 1)})
	c.RecordHit(ctx, "a") // b is now least recently used
	c.Insert(ctx, &Entry{ID: "c", Partition: "p", Vector: unit(-1, 0)})
	if _, ok := c.Get("b"); ok {
		t.Fatal("least recently used entry should be evicted")
	}
	if _, ok := c.Get("a"); !ok {
		t.Fatal("recently hit entry should survive")
	}
}

func TestStats(t *testing.T) {
	c := New(Config{Threshold: 0.5}, nil, nil)
	ctx := context.Background()
	c.Insert(ctx, &Entry{ID: "a", Partition: "p", Prompt: "x", Vector: unit(1, 0)})
	c.RecordHit(ctx, "a")
	st := c.Stats()
	if st.Live != 1 || st.TopEntries[0].Hits != 1 {
		t.Fatalf("unexpected stats %+v", st)
	}
}

func TestVectorCodec(t *testing.T) {
	v := []float32{0.25, -1.5, 3e-7}
	got := decodeVec(encodeVec(v))
	for i := range v {
		if got[i] != v[i] {
			t.Fatalf("round trip mismatch: %v != %v", got, v)
		}
	}
}

func BenchmarkLookup(b *testing.B) {
	for _, n := range []int{1000, 10000} {
		b.Run(fmt.Sprintf("entries=%d/dim=512", n), func(b *testing.B) {
			r := rand.New(rand.NewSource(1))
			c := New(Config{Threshold: 0.9}, nil, nil)
			vec := func() []float32 {
				v := make([]float32, 512)
				for i := range v {
					v[i] = r.Float32() - 0.5
				}
				return v
			}
			for i := 0; i < n; i++ {
				c.Insert(context.Background(), &Entry{Partition: "p", Vector: vec()})
			}
			q := vec()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				c.Lookup("p", q)
			}
		})
	}
}
