# Architecture and design decisions

This document explains why Sieve is built the way it is. Each decision lists the alternative that was considered and what the choice costs.

## Request path

```
auth → rate limit → cacheable? → embed → nearest-neighbour search → hit: serve
                                                                  → in flight: wait for it
                                                                  → miss: route → stream → store
```

The cache check takes under a millisecond whether it hits or misses (see the benchmarks in the README), so a miss costs almost nothing extra while a hit saves the full model call.

## 1. Similarity threshold of 0.85

**Decision.** A cached answer is served only when the nearest prompt scores at least 0.85.

**Why.** `make sweep` shows the trade-off directly. Below 0.80 the hit rate keeps rising but wrong answers climb quickly: 2.9% at 0.70 and 12.8% at 0.50. At 0.85 the benchmark served 70.5% of requests from cache with no wrong answers.

**Cost.** Some genuine paraphrases score below 0.85 and still go to the model. A wrong cached answer is worse than an extra model call: it is fast, confident and wrong, and nobody notices.

**Tuning.** The `sieve_cache_similarity` histogram shows how close misses were. If many misses sit just under the threshold and spot checks show they are true paraphrases, lower it a little.

## 2. Only single-turn requests are cached

**Decision.** Requests with more than one user message, or any assistant message, bypass the cache.

**Why.** In a conversation, "and what about Redis?" means something different every time. Caching on the last message alone would return answers that ignore the conversation.

**Cost.** Chat-style apps get less benefit. A future version could key on a summary of the conversation.

## 3. Cache partitions: tenant, model and system prompt

**Decision.** Entries are only matched within the same tenant, the same requested model and the same system prompt (hashed).

**Why.** Tenants must never see each other's answers. A system prompt such as "answer in French" or "you are a pirate" changes the right answer to the same question.

**Cost.** Each partition warms up separately.

## 4. Personal prompts bypass the cache

**Decision.** Prompts matching privacy patterns (order numbers, emails, Indian mobile numbers, card-like digit runs, PAN and Aadhaar shapes, "my order", "my account", typed OTPs) are never cached or served from cache.

**Why.** "Where is my order 4417?" has a different correct answer for every user. Serving one user's answer to another would leak data.

**Cost.** Pattern matching produces false positives (one extra model call) and false negatives (a personal prompt that slips through). Patterns err towards bypassing. Tenant partitioning limits the damage of a false negative to one tenant.

## 5. Users can flag a cached answer

**Decision.** `POST /v1/feedback` with `helpful: false` flags the entry; flagged entries are never served again.

**Why.** One bad cached answer would otherwise be repeated to every later user who asks something similar.

**Cost.** Flags can be abused to evict good answers. In a public deployment, route flags to review instead of acting on them immediately.

## 6. Ordered fallback with a circuit breaker

**Decision.** Providers are tried in configured order. Timeouts, 408, 409, 429 and 5xx responses fall through to the next provider. Three consecutive failures open that provider's breaker for 30 seconds, so it is skipped rather than retried on every request.

**Why.** During an outage, retrying the failing provider first adds its timeout to every request. The breaker keeps latency normal until the provider recovers.

**Cost.** Different models answer in different styles, so fallback trades consistency for availability. 400 and 401 errors never fall back, because they are the caller's or the operator's mistake and would fail everywhere.

**Streaming rule.** Once any text has been streamed to the client, the router does not switch providers. Doing so would splice two different answers together.

## 7. Token bucket rate limiting, failing open

**Decision.** Each tenant (or client IP) has a bucket of `capacity` tokens refilled at `refill_per_sec`. With Redis, the read-refill-spend cycle runs as one Lua script.

**Why.** Token buckets allow the short bursts real users make while capping sustained abuse. A Lua script makes the update atomic, so gateways on several machines cannot spend the same token twice.

**Cost.** One Redis round trip per request (about a millisecond on a local network). If Redis is unreachable, requests are allowed and `sieve_ratelimit_errors_total` counts it: an outage of the limiter should not become an outage of the product.

## 8. In-memory index mirrored through Redis

**Decision.** Each replica keeps every entry in memory for search, writes new entries to Redis, and pulls changes from a Redis sorted set every 2 seconds.

**Why.** Searching in memory keeps hits sub-millisecond. Redis is already needed for rate limiting, so the cache needs no extra infrastructure. The change log makes each sync incremental.

**Cost.** Replicas are eventually consistent: a new answer reaches other replicas within about 2 seconds, during which they may make one redundant model call. (Coalescing works within one replica; across replicas it would need a distributed lock, which costs a Redis round trip on every miss.) Memory per replica grows with the cache (about 2 KB per 512-dimension entry plus the text).

**Alternatives.** pgvector or Redis Stack vector search would move search out of process and scale past memory, at the cost of a network hop per lookup and a heavier dependency. Both are on the roadmap as optional backends.

## 9. Exact brute-force search

**Decision.** Lookups compare against every live entry in the partition with a 4-way unrolled dot product on unit vectors.

**Why.** It is exact, simple and fast enough: 0.31 ms for 1,000 entries and 4.2 ms for 10,000 on one core.

**Cost.** Linear in entries. An approximate index (HNSW) would keep lookups near constant time for very large caches, with a small recall loss.

## 10. TTL with pinned entries

**Decision.** Entries expire after 7 days by default. Pinned entries never expire. When a partition is full, the least recently used unpinned entry is evicted.

**Why.** Answers go stale when prices or policies change. Pinning keeps the most important answers warm.

**Cost.** Pinned entries need a manual refresh when their facts change.

## 11. Zero dependencies

**Decision.** Only the Go standard library is used, including a small RESP2 Redis client and a Prometheus text exporter.

**Why.** One static binary, quick security review, no supply-chain surprises, and nothing to update when a library changes its API.

**Cost.** The Redis client supports only what Sieve needs (no cluster mode, no TLS yet). Using `rediss://` or Redis Cluster would need a full client library.

## 12. Request coalescing

**Decision.** On a cache miss, the request checks a list of prompts currently being answered in its partition. If one is similar enough (the same threshold as the cache), it waits for that answer instead of calling the model. Otherwise it registers itself as the leader.

**Why.** A cache only helps after the first answer exists. When many people ask the same thing at the same moment, every request misses together: the "thundering herd". On a cold-cache spike of 200 concurrent requests, coalescing cut model calls from 268 to 134.

**Cost.** Followers wait for the leader, so their latency equals the leader's remaining time. If the leader fails, each follower calls the model itself, so a failure never becomes a wrong answer. In-flight state is per replica.
