package server

import (
	"sync"

	"github.com/Prm01/sieve/internal/embed"
)

// Request coalescing. When the cache is cold and many users ask the same
// thing at once (a launch, a viral post, an outage page), every request
// would miss and call the model. Instead the first request becomes the
// leader; later requests whose prompt is similar enough to an in-flight
// one wait for the leader's answer and are served from it.

type flight struct {
	partition string
	prompt    string
	vec       []float32
	done      chan struct{}

	// Set by the leader before done is closed.
	ok       bool
	text     string
	entryID  string
	provider string
}

type flights struct {
	mu sync.Mutex
	m  map[string][]*flight
}

// join returns an in-flight request similar to vec (leader=false), or
// registers a new one with the caller as leader (leader=true).
func (fs *flights) join(partition, prompt string, vec []float32, threshold float32) (f *flight, score float32, leader bool) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	var best *flight
	bestScore := float32(-2)
	for _, g := range fs.m[partition] {
		if s := embed.Dot(vec, g.vec); s > bestScore {
			best, bestScore = g, s
		}
	}
	if best != nil && bestScore >= threshold {
		return best, bestScore, false
	}
	if fs.m == nil {
		fs.m = map[string][]*flight{}
	}
	f = &flight{partition: partition, prompt: prompt, vec: vec, done: make(chan struct{})}
	fs.m[partition] = append(fs.m[partition], f)
	return f, 1, true
}

// finish publishes the leader's result to its followers.
func (fs *flights) finish(f *flight) {
	fs.mu.Lock()
	list := fs.m[f.partition]
	for i, g := range list {
		if g == f {
			list = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(list) == 0 {
		delete(fs.m, f.partition)
	} else {
		fs.m[f.partition] = list
	}
	fs.mu.Unlock()
	close(f.done)
}
