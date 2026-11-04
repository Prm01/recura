# Contributing to Recura

Thanks for helping. Small, focused pull requests are the easiest to review.

## Set up

You need Go 1.22 or newer. Redis is optional (only for the integration tests).

```bash
git clone https://github.com/Prm01/recura && cd recura
make test          # unit and HTTP tests, a few seconds
make test-redis    # adds Redis integration tests (needs redis on :6379)
make race          # race detector
make sweep         # check that cache quality did not regress
```

## Guidelines

- Keep the zero-dependency rule. If something truly needs a library, open an issue first to discuss it.
- Add a test with every behaviour change. Server behaviour is tested over real HTTP in `internal/server/server_test.go`.
- If you change matching (embedder, stopwords, threshold logic), include `make sweep` output before and after in the pull request.
- Run `make lint` before pushing. CI runs the same checks.
- Write commit messages in the imperative: "Add pgvector backend", not "Added".

## Good first issues

- Stream Gemini responses with `streamGenerateContent`.
- `rediss://` (TLS) support in `internal/resp`.
- A `GET /v1/cache/entries` endpoint with paging.
- An HNSW index behind the same `Lookup` interface.

## Reporting bugs

Open an issue with the Recura version (`recura -version`), your config with secrets removed, the request you sent and the response headers you got back.

## Releasing

Publish a release on GitHub with a new tag such as `v0.2.0`. The `release` workflow runs the tests, builds downloads for Windows, macOS and Linux with `scripts/package.sh`, and attaches them to the release. Leave the description empty to get download instructions filled in automatically.
