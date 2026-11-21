# Support bot: Recura with real questions

A small student-helpdesk chatbot that sits on top of Recura. It exists to
measure, with real people, how often Recura answers from cache and whether
people find those cached answers helpful.

- Chat page with a thumbs up/down on every answer
- Each message is a single-turn request to Recura (FAQ as the system prompt)
- A thumbs-down is forwarded to Recura's `/v1/feedback`, so the bad cached entry stops being served
- Every question is logged to `events.jsonl` (no names, no IPs, a random session id only)
- `-report` prints hit rate, latency and helpfulness from the log

Standard library only.

## Run it

1. Start Recura with a real model. For example OpenRouter:

   ```bash
   export OPENAI_API_KEY=sk-or-...                 # your OpenRouter key, never commit it
   export OPENAI_BASE_URL=https://openrouter.ai/api/v1
   export RECURA_OPENAI_MODEL=<a model id from openrouter.ai/models>
   export RECURA_RATE_CAPACITY=200                 # the bot is one client to Recura
   export RECURA_RATE_REFILL_PER_SEC=5
   go run ./cmd/recura
   ```

2. Replace `faq.txt` with your real FAQ (or point `BOT_FAQ` at your own file), then:

   ```bash
   go run ./examples/supportbot          # http://localhost:3000
   ```

3. After people have used it:

   ```bash
   go run ./examples/supportbot -report
   ```

| Variable | Default | Meaning |
|---|---|---|
| `RECURA_URL` | `http://localhost:8080` | Where Recura runs |
| `RECURA_KEY` | none | Bearer key if Recura has `RECURA_API_KEYS` set |
| `BOT_ADDR` | `:3000` | Bot listen address |
| `BOT_LOG` | `events.jsonl` | Event log path |
| `BOT_FAQ` | built-in sample | Path to your FAQ text file |

## Reading the results

- **From cache** is the real-traffic hit rate. It will differ from the synthetic benchmark, and that difference is the interesting part.
- **Thumbs-down on a cached answer** is the closest real-world measure of a wrong cache hit.
- Small samples mean wide error bars. Report the number of questions and students next to every percentage.
