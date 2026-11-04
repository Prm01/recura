package cache

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Prm01/recura/internal/resp"
)

// Integration test: run with RECURA_TEST_REDIS=redis://localhost:6379/15
func TestRedisSharedAcrossReplicas(t *testing.T) {
	url := os.Getenv("RECURA_TEST_REDIS")
	if url == "" {
		t.Skip("RECURA_TEST_REDIS not set")
	}
	ctx := context.Background()
	rc, err := resp.Dial(ctx, url, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	prefix := "recuratest:" + time.Now().Format("150405.000") + ":"
	a := New(Config{Threshold: 0.9}, &Redis{C: rc, Prefix: prefix}, nil)
	b := New(Config{Threshold: 0.9}, &Redis{C: rc, Prefix: prefix}, nil)
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go b.Sync(sctx, 50*time.Millisecond)

	a.Insert(ctx, &Entry{ID: "e1", Partition: "p", Prompt: "hi", Response: "hello", Vector: []float32{1, 0}})
	waitFor(t, func() bool { return b.Lookup("p", []float32{1, 0}).Hit })

	a.RecordHit(ctx, "e1")
	a.Flag(ctx, "e1")
	waitFor(t, func() bool { return b.Lookup("p", []float32{1, 0}).Entry == nil })

	a.Insert(ctx, &Entry{ID: "e2", Partition: "p", Vector: []float32{0, 1}})
	waitFor(t, func() bool { _, ok := b.Get("e2"); return ok })
	a.Delete(ctx, "e2")
	waitFor(t, func() bool { _, ok := b.Get("e2"); return !ok })
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met within 3s")
}
