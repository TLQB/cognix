# Latency baseline & methodology — zai-proxy agent path

Measurements: 2026-09-10, production-style instance (`--agent-mode`, live token),
bench harness `scripts/e2e/bench_latency.py`.

## How to measure

```sh
# production-style instance on a spare port (needs QBLESS_FILE when run
# outside the repo, else captcha minting falls back to the empty DB reserve)
PORT=3014 QBLESS_FILE=$PWD/qbless.json ./zai-proxy --agent-mode

python3 scripts/e2e/bench_latency.py 3014 --runs 3 --json out.json
```

TTFT = first delta carrying content or a tool_call fragment (role-only deltas
don't count — measuring those reports ~0s and hides the real model cost).

Always run ≥3 samples; single runs hit Z.AI capacity bursts (one plain run
measured 35-59s purely from account quota contention, not proxy overhead).

## Baseline (v1.0.9, before the 2026-09-10 optimizations)

| Scenario | TTFT median | Total median |
|---|---|---|
| plain (user msg, thinking on) | 3-5s | 3-5s |
| tool_one (last=user, thinking on) | 4.7s | 4.7s |
| mid-loop (last=tool result, throttle on) | 3.6s | 3.6s |
| depth8 / long_ctx (throttle on) | 4-5s | 4-5s |
| burst6 | — | wall 8.5s |
| burst12 | — | wall 11.8s, p50 6.9s, 0 err |

Phase breakdown (LOG_LEVEL=debug): acquire session + captcha cache hit ~0.1s,
upstream 200 headers ~2s, stream model ~1.4s. **TTFT is model time, not proxy
time.** The proxy-side wins are in burst behavior and capacity stalls.

## What was found (and fixed) on 2026-09-10

1. **Pool refill waited on the DELETE** (`Release` retired the used session
   upstream before stocking a replacement). Creation is free (a local UUID
   mint), the DELETE is a paced upstream round-trip (~1s observed). Every
   release therefore starved the pool for a second; a burst drained the
   batch and later requests waited on refills. Fix: refill first, delete
   after (`session_pool.go`). Pinned by
   `TestPoolReleaseRefillsBeforeDelete`.

2. **Capacity rejects were handled remotely** (MODEL_CONCURRENCY_LIMIT →
   2s-step backoff up to 14s; 31 stalls in one production day). Fix: an AIMD
   concurrency gate (`concurrency_gate.go`) — local queueing at an adaptive
   in-flight limit (halved per capacity reject, +1 after 8 clean
   completions, floor 1 / ceiling 8, 30s fail-open timeout) so bursts ride
   out at the gate instead of through repeated remote rejects. The remote
   retry loop stays as the safety net. Disabled in mock/CI mode.

Interleaved A/B, burst12 ×3 rounds, both proxies on the same account token
(new build on :3014 vs v1.0.9 on :3002):

| Build | wall median | capacity rejects during bench |
|---|---|---|
| v1.0.9 (old) | 10.30s (9.42-13.69) | 31 in the day's log |
| gate + refill-first | 8.72s (8.68-10.76) | 0 |

~15% burst wall-time improvement and — more importantly for stability —
zero remote capacity rejects: the gate keeps the proxy under quota, so the
multi-second reject backoffs never trigger.

## Known non-issues (measured, don't re-chase)

- glm-4.7 thinking turns (last=user with thinking on) take 4-35s — model
  behavior, not proxy. Mid-loop turns (throttle: thinking off) confirm the
  transport path is 3.6s.
- Prompt growth in the native variant: ~2.2KB contract + ~220B/exchange
  → ~9.3KB at depth 32. Not a latency factor at realistic depths.
- `pacedTransport` (50-150ms gap between upstream calls) is deliberate WAF
  protection — see ratelimit.go before touching it.
