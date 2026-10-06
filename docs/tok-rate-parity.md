# tok/s parity: pi (via yardmaster) vs the ZCode desktop app

Measured live 2026-10-04, glm-5.3-flash, same machine, same Z.ai coding-plan
key. Answers: "why does the tok/s pi shows via yardmaster look much slower
than the ZCode app for the same model?"

## TL;DR

The upstream serving is identical — the displayed number is computed
differently, and it divides by a window that Z.ai dominates.

1. **Z.ai holds every glm-5.3-flash request server-side before the first
   byte** (~3–8s, invariant). It batches on every coding-plan ingress —
   `api.z.ai/api/anthropic`, the signed ultra route
   `zcode.z.ai/api/v1/ultra-zai/anthropic`, with or without
   `speed: fast` / `fast-mode-2026-02-01` / signing / thinking variants.
   After the hold, decode runs at 60–230+ tok/s, often released in bursts.
2. **pi-sub displays `output / full wall time`** (measured
   `before_provider_request` → `message_end`), which folds that server-side
   hold into the divisor: short generations read 13–35 tok/s.
3. **ZCode displays `output / (duration − timeToFirstContent)`** (from its
   own telemetry schema: `tokensPerSecond = TZt(output, durationMs −
   timeToFirstContentMs)`) — it excludes the hold: the same requests display
   126–232 tok/s.
4. **yardmaster adds ≈0**: interleaved A/B, byte-identical body, proxied vs
   direct — TTFT median 7.1s proxied vs 8.3s direct (noise), wall 7.5s vs
   8.4s. ZCode's own request telemetry (same machine/model/period) shows the
   same shape: TTFT 2.7–7.8s, wall tok/s 17.5–35.

## The numbers

| measurement | value |
|---|---|
| Z.ai first-byte hold (direct, all routes/shapes) | 2.5–7.9s |
| same body via yardmaster (interleaved A/B, n=4×2) | 7.1s median vs 8.3s direct |
| decode rate after first token (probe, thinking adaptive+low) | 60–230 tok/s |
| pi-sub display (wall) for those turns | 13–21 tok/s |
| ZCode app telemetry, own session (GLM-5.3-Flash) | TTFT 2.7–7.8s, wall 17.5–35 tok/s, display-formula 126–232 tok/s |

## What this means for yardmaster

- The ZCode-parity request (signing, identity headers, `speed: fast`,
  `fast-mode-2026-02-01`, adaptive thinking + `output_config.effort`,
  cache_control markers) is protocol-faithful — verified against the ZCode
  3.14 bundle's `sendSigned` (`{apiKeyId}\n{ts}\n{clientVersion}\n{sessionId}\n{nonce}`,
  8-bit PoW, app id `zcode`). Upstream treats it identically; there is no
  serving tier to unlock by "being more like ZCode".
- Parity version tracking matters: the client version rides the *signed
  message* and identity headers. `internal/zcode` now ships `3.14.4`
  (matches the installed desktop release; re-check when ZCode updates).
- The dashboard Requests tab shows **tok/s = output ÷ (duration − TTFT)**
  (ZCode's convention) next to ttft/dur, so the wall-clock vs decode-phase
  distinction is visible in yardmaster's own stats. `web/src/api.ts genTps`.
- TTFT is now recorded for translated streams too — `forwardTranslateStream`
  / `forwardTranslateFull` used to return `0`, so every pi-shaped
  (openai→anthropic) request logged ttft 0ms and skewed the latency stats.
- The only proxy-side wall-time contributions are the per-key dispatch
  throttle (`dispatch_interval_ms` spacing beyond `dispatch_burst`, visible
  as `queue_ms`) and the 100ms-per-hop failover backoff — both only under
  concurrency, both already surfaced per request.

## Fixing the number pi displays

pi-sub owns the displayed figure. ZCode-style would record the first content
delta (`message_update` with a `text_delta`/`thinking_delta`) and compute
`output / (message_end − first_delta)`; keep the wall figure as a second
value. That is a pi-sub change; yardmaster cannot move pi's divisor.

## Prompt-cache health (verified 2026-10-05, closes the "0% cache" era)

The 2026-09-21 "0% cache hit through yardmaster" finding does **not**
reproduce on current builds. Live capture — real pi 1.0.2 with the full
ceulen extension bundle, 5-turn session with tool use, via a local yardmaster
with `YARDMASTER_DUMP_DIR` body dumps, glm-5.3-flash on the coding plan:

| request | tok_in | cache_read | hit |
|---|---|---|---|
| turn 1 main (cold session) | 34,536 | 0 | 0% (expected — nothing to reuse) |
| turn 1 follow-up (+244 tok) | 284 | 34,496 | 99.2% |
| turns 2–5, every request (tool round-trips included) | 43–153 | 34,752–34,944 | **99%+** |

Body dumps prove the cache head is byte-stable across turns: system prompt
(65,631 chars) and all 74 tools identical in every request; only the message
tail grows. pi's per-turn section diffs never mutated the head in this
capture, and Z.ai's implicit cache ignores `cache_control` marker placement
(markers move to the new last message each turn without breaking hits).

What still pays full prefill — structural, not a bug:
- **First request of every conversation namespace**: each new pi session,
  subagent, and advisor review prefills its own ~35k once (~7.9 credits
  flash / ~23.8 glm-5.3 at current rates; a 90%-cached turn costs ~2.5 /
  ~7.7 — the ~3x input saving that caching buys from turn 2 on).
- **Head mutations** rewrite the prefix and cost one full re-prefill each.
  Candidates audited in the ceulen bundle: rtk availability gate (re-probes
  every 30s when the binary looks missing — flips only if availability
  changes mid-session), advisor pause line (vanishes after 3 consecutive
  reviewer failures), tool-set churn (plan-mode toggles), mid-session
  RULES.md/AGENTS.md edits. None fired in the capture; frequency in real
  interactive use is unknown — watch the dashboard Cache tab.

The old 0% was two artifacts: yardmaster not reading Z.ai's stream usage
from the terminal `message_delta` (fixed 2026-09-22), and comparing parallel
requests (main turn vs advisor/subagent — separate cache namespaces, zero
shared prefix by design).

Capture harness (reusable): `router-local` provider in `~/.pi/agent/models.json`
→ `http://127.0.0.1:8791/v1`, run
`pi -p --provider router-local --model zai/glm-5.3-flash …` chained with
`--continue`, with `YARDMASTER_DUMP_DIR` set on a local instance.
