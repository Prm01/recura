// Package ratelimit implements token-bucket rate limiting, in memory for a
// single instance or in Redis for a fleet of gateways.
package ratelimit

import (
	"context"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/Prm01/sieve/internal/resp"
)

// Decision is the outcome of one Allow call.
type Decision struct {
	Allowed    bool
	Remaining  float64
	RetryAfter time.Duration
}

// Limiter decides whether a key may make another request.
type Limiter interface {
	Allow(ctx context.Context, key string) (Decision, error)
}

// Config is a bucket of Capacity tokens refilled at RefillPerSec.
type Config struct {
	Capacity     float64
	RefillPerSec float64
}

func (c Config) retryAfter(tokens float64) time.Duration {
	if c.RefillPerSec <= 0 {
		return time.Hour
	}
	need := 1 - tokens
	if need <= 0 {
		return 0
	}
	return time.Duration(math.Ceil(need / c.RefillPerSec * float64(time.Second)))
}

// Memory is a per-process limiter. Idle buckets are swept periodically.
type Memory struct {
	cfg     Config
	mu      sync.Mutex
	buckets map[string]*bucket
	now     func() time.Time
	lastGC  time.Time
}

type bucket struct {
	tokens float64
	ts     time.Time
}

func NewMemory(cfg Config) *Memory {
	return &Memory{cfg: cfg, buckets: map[string]*bucket{}, now: time.Now}
}

func (m *Memory) Allow(_ context.Context, key string) (Decision, error) {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.buckets[key]
	if b == nil {
		b = &bucket{tokens: m.cfg.Capacity, ts: now}
		m.buckets[key] = b
	}
	b.tokens = math.Min(m.cfg.Capacity, b.tokens+now.Sub(b.ts).Seconds()*m.cfg.RefillPerSec)
	b.ts = now
	d := Decision{}
	if b.tokens >= 1 {
		b.tokens--
		d.Allowed = true
	} else {
		d.RetryAfter = m.cfg.retryAfter(b.tokens)
	}
	d.Remaining = math.Floor(b.tokens)
	if now.Sub(m.lastGC) > time.Minute {
		m.gc(now)
	}
	return d, nil
}

func (m *Memory) gc(now time.Time) {
	m.lastGC = now
	full := time.Duration(m.cfg.Capacity/math.Max(m.cfg.RefillPerSec, 1e-9)*float64(time.Second)) + time.Minute
	for k, b := range m.buckets {
		if now.Sub(b.ts) > full {
			delete(m.buckets, k)
		}
	}
}

// Redis is a fleet-wide limiter. The bucket update runs as one Lua script,
// so concurrent gateways cannot double-spend a token.
type Redis struct {
	C      *resp.Client
	Cfg    Config
	Prefix string
}

const script = `
local cap  = tonumber(ARGV[1])
local rate = tonumber(ARGV[2])
local now  = tonumber(ARGV[3])
local b = redis.call('HMGET', KEYS[1], 't', 'ts')
local tokens = tonumber(b[1])
local ts = tonumber(b[2])
if tokens == nil then tokens = cap; ts = now end
tokens = math.min(cap, tokens + math.max(0, now - ts) * rate / 1000)
local allowed = 0
if tokens >= 1 then tokens = tokens - 1; allowed = 1 end
redis.call('HSET', KEYS[1], 't', tostring(tokens), 'ts', tostring(now))
local ttl = 60000
if rate > 0 then ttl = math.ceil(cap / rate * 1000) + 60000 end
redis.call('PEXPIRE', KEYS[1], ttl)
return {allowed, tostring(tokens)}
`

func (r *Redis) Allow(ctx context.Context, key string) (Decision, error) {
	p := r.Prefix
	if p == "" {
		p = "sieve:rl:"
	}
	v, err := r.C.Do(ctx, "EVAL", script, "1", p+key,
		strconv.FormatFloat(r.Cfg.Capacity, 'f', -1, 64),
		strconv.FormatFloat(r.Cfg.RefillPerSec, 'f', -1, 64),
		strconv.FormatInt(time.Now().UnixMilli(), 10))
	if err != nil {
		return Decision{}, err
	}
	if len(v.Array) != 2 {
		return Decision{}, resp.Err("unexpected EVAL reply")
	}
	tokens, _ := strconv.ParseFloat(v.Array[1].Str, 64)
	d := Decision{Allowed: v.Array[0].Int == 1, Remaining: math.Floor(tokens)}
	if !d.Allowed {
		d.RetryAfter = r.Cfg.retryAfter(tokens)
	}
	return d, nil
}

// FailOpen wraps a limiter so an outage of its backend lets traffic
// through instead of taking the gateway down. The error is still returned
// so callers can log and count it.
type FailOpen struct{ L Limiter }

func (f FailOpen) Allow(ctx context.Context, key string) (Decision, error) {
	d, err := f.L.Allow(ctx, key)
	if err != nil {
		return Decision{Allowed: true, Remaining: -1}, err
	}
	return d, nil
}
