# Changelog

## Unreleased

- Built-in website: home page with a live demo, live traffic visualisation, savings calculator, interactive request pipeline and benchmark charts; documentation at /docs; console moved to /console
- `GET /v1/public/stats` for live aggregate numbers, with no prompt text
- Fonts ship inside the binary, so the site works offline

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
