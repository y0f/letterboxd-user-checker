# letterboxd-user-checker

Checks if Letterboxd username can really be registered. Go, one dep (uTLS). Every
proxy runs own workers off one shared queue, own connection joins at rate
limit. Free names go to text file. Same engine as github-user-checker.

Default run: every 2-char name (`a-z0-9_`, 37² = 1369).

## Why not profile 404

`letterboxd.com/<name>/` lies twice:

- `admin` has no profile, signup still refuses it (reserved).
- profile pages sit behind Cloudflare managed challenge. Non-browser client
  gets `403` + `Cf-Mitigated: challenge` for taken and free names alike.

This tool asks what signup form asks while you type:
`GET letterboxd.com/s/checkusername?q=<name>` →
`{"result":true,"csrf":"…","data":{"result":"TAKEN"}}`. Not challenged.

| `data.result` | meaning | output |
|---|---|---|
| `AVAILABLE` | free | `available.txt` |
| `TAKEN` | taken | — |
| `YOU` | yours (signed in only) | counted taken |
| `ILLEGAL` | reserved word, never registrable | `reserved.txt` |
| `INVALID` | not `a-z0-9_` | — |
| `TOO_SHORT` / `MISSING` | under 2 chars | — |
| `TOO_LONG` | over 15 chars | — |

Rules verified live 2026-10-29: `a` TOO_SHORT, `ab` TAKEN, `x_` TAKEN,
`a-b` INVALID, `admin` ILLEGAL, 16 chars TOO_LONG. Case insensitive.

## TLS fingerprint

Cloudflare challenges by TLS ClientHello, even on the JSON endpoint. Tested
2026-10-29 from one IP, same headers, 8 tries each:

| ClientHello | result |
|---|---|
| Go `crypto/tls` | 403 challenge |
| uTLS Chrome / Edge / Firefox / iOS / randomized | 403 challenge |
| uTLS Safari + Safari UA | **200** |
| curl on Windows (Schannel) | 200 |

So tunnel opened by hand (direct, HTTP CONNECT, SOCKS4/5), TLS spoken by
uTLS as Safari with ALPN `http/1.1`, User-Agent Safari 17.6 to match. If
Cloudflare changes this, every attempt turns `challenged` — run `-debug`.

## Every response classified

Letterboxd sits behind Cloudflare, so every genuine answer has `Cf-Ray`. No
`Cf-Ray` → proxy wrote it.

| response | verdict | what happens |
|---|---|---|
| network error, timeout, TLS fail | junk | proxy backoff (doubling), name requeued |
| no `Cf-Ray` (any status, 407, proxy 502/503/504) | junk | same |
| `200` but not the JSON envelope | junk | same |
| `Cf-Mitigated: challenge` / "Just a moment" | challenged | proxy paused `6×backoff`, doubling per challenge in a row; name not charged |
| `403` 1020 / 1006-1012 / bare | challenged | same |
| `429`, Cloudflare `1015` | limited | proxy paused `max(limit-pause, Retry-After)`, name tries +1 |
| `500-504`, Cloudflare `520-530` | server error | same as limited |
| `3xx`, `400`, `404`, `405`, unknown result, `result:false` | odd | name tries +1, logged with `-debug` |
| `200` + known `data.result` | answer | decided |

## No false positives

1. Only Cloudflare-fronted JSON with known result counts.
2. Every `AVAILABLE` re-checked through second route before written. If second
   route disagrees, both dropped, third asked.
3. Junk from never-proven proxy does not count against name; only proven
   proxy failures do (`-tries`).
4. Names gave up on stay out of `checked.txt`, rerun retries them.

## Run

```bash
go build -o lbcheck.exe .
./lbcheck.exe                                # all 2-char names
./lbcheck.exe -len 3 -charset abcdefghijklmnopqrstuvwxyz
./lbcheck.exe -in names.txt                  # own list instead
```

Hits → `available.txt` as found. Every decided name → `checked.txt`
(`name<TAB>verdict`). Ctrl+C, run again, resumes. `-list file` saves the
generated names.

## Flags

| flag | default | meaning |
|---|---|---|
| `-len` | `2` | generated name length (2..15) |
| `-charset` | `a-z0-9_` | chars for generated names |
| `-in` | — | wordlist instead of generator |
| `-list` | — | write generated names to file |
| `-out` | `available.txt` | free names |
| `-reserved` | `reserved.txt` | ILLEGAL names |
| `-done` | `checked.txt` | resume log |
| `-proxies` | `proxies.txt` | proxy list |
| `-direct` | `true` | own connection joins pool, rate limited |
| `-direct-rate` | `40` | direct requests per minute, `0` unlimited |
| `-per-proxy` | `2` | concurrent requests per proxy |
| `-tries` | `10` | trustworthy failures per name before giving up |
| `-backoff` | `10s` | pause after proxy fails, doubles per failure in a row |
| `-max-backoff` | `10m` | longest pause |
| `-limit-pause` | `30s` | pause after 429 / 5xx |
| `-reload` | `2m` | re-read proxy list, `0` off |
| `-confirm` | `true` | re-check hits via second route |
| `-timeout` | `10s` | per request |
| `-debug` | `false` | print why each attempt rejected, with status, ray, body |

## Proxies

`proxies.txt`, one `scheme://[user:pass@]host:port` per line (http, https,
socks4, socks5; bare `host:port` = http). Gitignored. Proxies must tunnel
https; one per exit IP is best, since many listed proxies share one address
and Letterboxd sees them as one visitor.

Dead proxies never retired, only paused and probed again. `-reload` picks up
a fresh list mid-run.
