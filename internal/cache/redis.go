package cache

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math"
	"strconv"
	"time"

	"github.com/Prm01/sieve/internal/resp"
)

// Redis persists entries so that replicas share one cache.
//
// Layout (prefix defaults to "sieve:"):
//
//	{p}e:{id}       STRING  JSON entry (vector base64-encoded), expires with TTL
//	{p}changes      ZSET    id -> last update time in ms (sync cursor)
//	{p}deleted      ZSET    id -> delete time in ms
//	{p}hits         HASH    id -> hit count (HINCRBY, so replicas never race)
type Redis struct {
	C      *resp.Client
	Prefix string
	TTL    time.Duration
}

type storedEntry struct {
	Entry
	Vector string `json:"vector"`
}

func (r *Redis) k(s string) string {
	p := r.Prefix
	if p == "" {
		p = "sieve:"
	}
	return p + s
}

func encodeVec(v []float32) string {
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(x))
	}
	return base64.StdEncoding.EncodeToString(b)
}

func decodeVec(s string) []float32 {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil
	}
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return v
}

func ms(t time.Time) string { return strconv.FormatInt(t.UnixMilli(), 10) }

func (r *Redis) Save(ctx context.Context, e *Entry) error {
	se := storedEntry{Entry: *e, Vector: encodeVec(e.Vector)}
	raw, err := json.Marshal(se)
	if err != nil {
		return err
	}
	args := []string{"SET", r.k("e:" + e.ID), string(raw)}
	if r.TTL > 0 && !e.Pinned {
		remaining := r.TTL - time.Since(e.CreatedAt)
		if remaining < time.Second {
			remaining = time.Second
		}
		args = append(args, "PX", strconv.FormatInt(remaining.Milliseconds(), 10))
	}
	if _, err := r.C.Do(ctx, args...); err != nil {
		return err
	}
	_, err = r.C.Do(ctx, "ZADD", r.k("changes"), ms(e.UpdatedAt), e.ID)
	return err
}

func (r *Redis) Remove(ctx context.Context, id string) error {
	if _, err := r.C.Do(ctx, "DEL", r.k("e:"+id)); err != nil {
		return err
	}
	if _, err := r.C.Do(ctx, "ZREM", r.k("changes"), id); err != nil {
		return err
	}
	if _, err := r.C.Do(ctx, "HDEL", r.k("hits"), id); err != nil {
		return err
	}
	_, err := r.C.Do(ctx, "ZADD", r.k("deleted"), ms(time.Now()), id)
	return err
}

func (r *Redis) IncrHits(ctx context.Context, id string) error {
	_, err := r.C.Do(ctx, "HINCRBY", r.k("hits"), id, "1")
	return err
}

// syncOverlap re-reads a short window before the cursor so that writes
// landing out of order across replicas are not missed. Applying an entry
// twice is harmless.
const syncOverlap = 2 * time.Second

func (r *Redis) Changes(ctx context.Context, since time.Time) ([]*Entry, []string, time.Time, error) {
	now := time.Now()
	from := "-inf"
	if !since.IsZero() {
		from = ms(since.Add(-syncOverlap))
	}
	ids, err := r.C.Do(ctx, "ZRANGEBYSCORE", r.k("changes"), from, "+inf")
	if err != nil {
		return nil, nil, since, err
	}
	var ups []*Entry
	idList := ids.Strings()
	const batch = 200
	for i := 0; i < len(idList); i += batch {
		chunk := idList[i:min(i+batch, len(idList))]
		keys := make([]string, 0, len(chunk)+1)
		keys = append(keys, "MGET")
		for _, id := range chunk {
			keys = append(keys, r.k("e:"+id))
		}
		vals, err := r.C.Do(ctx, keys...)
		if err != nil {
			return nil, nil, since, err
		}
		hargs := append([]string{"HMGET", r.k("hits")}, chunk...)
		hits, err := r.C.Do(ctx, hargs...)
		if err != nil {
			return nil, nil, since, err
		}
		var expired []string
		for j, v := range vals.Array {
			if v.Null {
				expired = append(expired, chunk[j])
				continue
			}
			var se storedEntry
			if json.Unmarshal([]byte(v.Str), &se) != nil {
				continue
			}
			e := se.Entry
			e.Vector = decodeVec(se.Vector)
			if j < len(hits.Array) && !hits.Array[j].Null {
				e.Hits, _ = strconv.ParseInt(hits.Array[j].Str, 10, 64)
			}
			ups = append(ups, &e)
		}
		// Keys that expired through TTL: drop them from the change log.
		for _, id := range expired {
			_, _ = r.C.Do(ctx, "ZREM", r.k("changes"), id)
		}
	}
	var dels []string
	if !since.IsZero() {
		d, err := r.C.Do(ctx, "ZRANGEBYSCORE", r.k("deleted"), from, "+inf")
		if err != nil {
			return nil, nil, since, err
		}
		dels = d.Strings()
		// Tombstones only need to outlive the slowest replica's sync.
		_, _ = r.C.Do(ctx, "ZREMRANGEBYSCORE", r.k("deleted"), "-inf", ms(now.Add(-time.Hour)))
	}
	return ups, dels, now, nil
}
