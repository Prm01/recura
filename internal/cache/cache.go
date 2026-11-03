// Package cache is Sieve's semantic cache: an in-memory vector index of
// past prompts and their answers, optionally mirrored to Redis so every
// gateway replica shares the same cache.
package cache

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/Prm01/sieve/internal/embed"
)

// Entry is one cached prompt and its answer.
type Entry struct {
	ID        string    `json:"id"`
	Partition string    `json:"partition"` // tenant|model|system-prompt hash
	Prompt    string    `json:"prompt"`
	Response  string    `json:"response"`
	Vector    []float32 `json:"vector"`
	Provider  string    `json:"provider"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	LastHit   time.Time `json:"last_hit"`
	Hits      int64     `json:"hits"`
	Flagged   bool      `json:"flagged"`
	Pinned    bool      `json:"pinned"`
}

// Persister stores entries outside the process. The cache keeps working
// from memory if a persister call fails; failures are logged.
type Persister interface {
	Save(ctx context.Context, e *Entry) error
	Remove(ctx context.Context, id string) error
	IncrHits(ctx context.Context, id string) error
	// Changes returns entries updated and ids deleted at or after since,
	// plus the cursor to pass next time.
	Changes(ctx context.Context, since time.Time) (upserts []*Entry, deletes []string, next time.Time, err error)
}

// Config controls matching and retention.
type Config struct {
	Threshold  float32       // minimum cosine similarity for a hit
	TTL        time.Duration // 0 = never expire
	MaxEntries int           // per partition; 0 = unlimited
}

// Cache is safe for concurrent use.
type Cache struct {
	cfg   Config
	mu    sync.RWMutex
	parts map[string]map[string]*Entry
	byID  map[string]*Entry
	store Persister
	log   *slog.Logger
	now   func() time.Time
}

// New returns a cache. store may be nil for a memory-only cache.
func New(cfg Config, store Persister, log *slog.Logger) *Cache {
	if log == nil {
		log = slog.Default()
	}
	return &Cache{cfg: cfg, parts: map[string]map[string]*Entry{}, byID: map[string]*Entry{}, store: store, log: log, now: time.Now}
}

// Threshold returns the configured similarity threshold.
func (c *Cache) Threshold() float32 { return c.cfg.Threshold }

func (c *Cache) live(e *Entry, now time.Time) bool {
	if e.Flagged {
		return false
	}
	return e.Pinned || c.cfg.TTL == 0 || now.Sub(e.CreatedAt) < c.cfg.TTL
}

// Match is the result of a lookup.
type Match struct {
	Entry *Entry  // nearest live entry, nil when the partition is empty
	Score float32 // similarity of Entry
	Hit   bool    // Score >= threshold
}

// Lookup finds the nearest live entry in a partition. It returns the
// nearest entry even when it falls below the threshold, so callers can
// log how close a miss was.
func (c *Cache) Lookup(partition string, vec []float32) Match {
	vec = embed.Normalize(append([]float32(nil), vec...))
	now := c.now()
	c.mu.RLock()
	defer c.mu.RUnlock()
	var best *Entry
	bestScore := float32(-2)
	for _, e := range c.parts[partition] {
		if !c.live(e, now) {
			continue
		}
		if s := embed.Dot(vec, e.Vector); s > bestScore {
			best, bestScore = e, s
		}
	}
	if best == nil {
		return Match{}
	}
	return Match{Entry: best, Score: bestScore, Hit: bestScore >= c.cfg.Threshold}
}

// Insert adds an entry, evicting the least recently used entry in its
// partition if the partition is full.
func (c *Cache) Insert(ctx context.Context, e *Entry) {
	now := c.now()
	if e.ID == "" {
		e.ID = newID()
	}
	e.Vector = embed.Normalize(append([]float32(nil), e.Vector...))
	if e.CreatedAt.IsZero() {
		e.CreatedAt = now
	}
	e.UpdatedAt = now
	var evicted string
	c.mu.Lock()
	p := c.parts[e.Partition]
	if p == nil {
		p = map[string]*Entry{}
		c.parts[e.Partition] = p
	}
	if c.cfg.MaxEntries > 0 && len(p) >= c.cfg.MaxEntries {
		evicted = c.lruLocked(p)
		if evicted != "" {
			delete(p, evicted)
			delete(c.byID, evicted)
		}
	}
	p[e.ID] = e
	c.byID[e.ID] = e
	c.mu.Unlock()
	if c.store != nil {
		if err := c.store.Save(ctx, e); err != nil {
			c.log.Warn("cache persist failed", "id", e.ID, "err", err)
		}
		if evicted != "" {
			_ = c.store.Remove(ctx, evicted)
		}
	}
}

func (c *Cache) lruLocked(p map[string]*Entry) string {
	var id string
	var oldest time.Time
	for k, e := range p {
		if e.Pinned {
			continue
		}
		t := e.LastHit
		if t.IsZero() {
			t = e.CreatedAt
		}
		if id == "" || t.Before(oldest) {
			id, oldest = k, t
		}
	}
	return id
}

// RecordHit updates hit statistics for an entry.
func (c *Cache) RecordHit(ctx context.Context, id string) {
	c.mu.Lock()
	e := c.byID[id]
	if e != nil {
		e.Hits++
		e.LastHit = c.now()
	}
	c.mu.Unlock()
	if e != nil && c.store != nil {
		if err := c.store.IncrHits(ctx, id); err != nil {
			c.log.Warn("cache hit persist failed", "id", id, "err", err)
		}
	}
}

// Flag stops an entry from being served (user reported a wrong answer).
func (c *Cache) Flag(ctx context.Context, id string) bool {
	c.mu.Lock()
	e := c.byID[id]
	var cp Entry
	if e != nil {
		e.Flagged = true
		e.UpdatedAt = c.now()
		cp = *e
	}
	c.mu.Unlock()
	if e == nil {
		return false
	}
	if c.store != nil {
		if err := c.store.Save(ctx, &cp); err != nil {
			c.log.Warn("cache flag persist failed", "id", id, "err", err)
		}
	}
	return true
}

// Delete removes an entry.
func (c *Cache) Delete(ctx context.Context, id string) bool {
	c.mu.Lock()
	e := c.byID[id]
	if e != nil {
		delete(c.byID, id)
		delete(c.parts[e.Partition], id)
	}
	c.mu.Unlock()
	if e != nil && c.store != nil {
		_ = c.store.Remove(ctx, id)
	}
	return e != nil
}

// Get returns a copy of an entry.
func (c *Cache) Get(id string) (Entry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e := c.byID[id]
	if e == nil {
		return Entry{}, false
	}
	return *e, true
}

// Stats summarises the cache.
type Stats struct {
	Entries    int        `json:"entries"`
	Live       int        `json:"live"`
	Flagged    int        `json:"flagged"`
	Partitions int        `json:"partitions"`
	Threshold  float32    `json:"threshold"`
	TopEntries []TopEntry `json:"top_entries"`
}

// TopEntry is an entry summary for the stats endpoint.
type TopEntry struct {
	ID     string `json:"id"`
	Prompt string `json:"prompt"`
	Hits   int64  `json:"hits"`
}

func (c *Cache) Stats() Stats {
	now := c.now()
	c.mu.RLock()
	defer c.mu.RUnlock()
	s := Stats{Entries: len(c.byID), Partitions: len(c.parts), Threshold: c.cfg.Threshold}
	var top []TopEntry
	for _, e := range c.byID {
		if e.Flagged {
			s.Flagged++
		}
		if c.live(e, now) {
			s.Live++
			top = append(top, TopEntry{ID: e.ID, Prompt: e.Prompt, Hits: e.Hits})
		}
	}
	sort.Slice(top, func(i, j int) bool { return top[i].Hits > top[j].Hits })
	if len(top) > 10 {
		top = top[:10]
	}
	s.TopEntries = top
	return s
}

// Sync pulls changes from the persister until ctx ends. Each replica runs
// it so entries written by other replicas appear here within interval.
func (c *Cache) Sync(ctx context.Context, interval time.Duration) {
	if c.store == nil {
		return
	}
	var since time.Time
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		ups, dels, next, err := c.store.Changes(ctx, since)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			c.log.Warn("cache sync failed", "err", err)
		} else {
			c.apply(ups, dels)
			since = next
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (c *Cache) apply(ups []*Entry, dels []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range ups {
		if old := c.byID[e.ID]; old != nil {
			if old.UpdatedAt.After(e.UpdatedAt) {
				continue
			}
			if old.Partition != e.Partition {
				delete(c.parts[old.Partition], e.ID)
			}
			if old.Hits > e.Hits {
				e.Hits = old.Hits
			}
			if old.LastHit.After(e.LastHit) {
				e.LastHit = old.LastHit
			}
		}
		p := c.parts[e.Partition]
		if p == nil {
			p = map[string]*Entry{}
			c.parts[e.Partition] = p
		}
		p[e.ID] = e
		c.byID[e.ID] = e
	}
	for _, id := range dels {
		if e := c.byID[id]; e != nil {
			delete(c.parts[e.Partition], id)
			delete(c.byID, id)
		}
	}
}

func newID() string {
	var b [10]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
