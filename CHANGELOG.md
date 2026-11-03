# Changelog

## v0.1.0

First public release.

- OpenAI-compatible `/v1/chat/completions` with streaming
- Semantic cache with threshold, TTL, LRU eviction, pinning and user flags
- Providers: Anthropic, OpenAI-compatible (OpenAI, Ollama, vLLM, Groq and others), Gemini, mock
- Request coalescing for concurrent similar misses
- Ordered fallback with per-provider circuit breakers
- Token-bucket rate limiting in memory or Redis
- Redis-backed shared cache across replicas
- Prometheus metrics, Grafana dashboard
- Web console with live pipeline, latency chart, similarity meter, provider controls and rate-limit test
- `sievebench` workload replay and threshold sweep
