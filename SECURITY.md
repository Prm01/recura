# Security policy

Please report vulnerabilities privately by email to **pramodyadav2948@gmail.com** rather than opening a public issue. You will get a reply within 72 hours.

Things that count as security issues:

- one tenant reading another tenant's cached answers
- bypassing API-key authentication or the admin key
- personal data being cached despite matching a privacy pattern
- leaking provider API keys in logs, headers or error messages

Sieve stores prompts and answers in memory and, if configured, in Redis. Protect Redis with a password and a private network, and set `SIEVE_API_KEYS` and `SIEVE_ADMIN_KEY` on any gateway reachable from the internet.
