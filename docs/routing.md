# Routing strategies — research + design decisions

Research round 2026-09: how established LLM proxies route requests, which
strategies matter for yardmaster's single-user / coding-plan fleet, and what we
adopted. Field reference for what shipped lives in the
[Routing](#yardmaster-implementation) section at the end.

## Industry survey

### LiteLLM (most complete taxonomy)

Global default `routing_strategy` plus per-model **routing groups** with
per-group strategy override — the same "global + per-group" split this feature
implements.

| Strategy | How it picks | When it's useful |
|---|---|---|
| `simple-shuffle` (**default, recommended**) | Weighted random by declared rpm/tpm; plain random if no limits | Production default — near-zero overhead, no shared state |
| `usage-based-routing-v2` | Deployment with most remaining TPM/RPM headroom | Known per-deployment quotas; spread to avoid 429s |
| `latency-based-routing` | Lowest observed latency (EMA over a TTL window) | Latency-sensitive serving |
| `least-busy` | Fewest in-flight requests | Long, variable-duration requests |
| `cost-based-routing` | Cheapest deployment | Cost optimization across equivalent models |

Reliability layer, orthogonal to strategy: `num_retries` per deployment →
cooldown after `allowed_fails` failures/min for `cooldown_time`; **429 triggers
immediate cooldown**; then ordered model-group fallbacks, with special classes
(`context_window_fallbacks`, `content_policy_fallbacks`). Session affinity
pins a conversation to one deployment (TTL-bound), primarily to maximize
provider-side prompt-cache hits.

Source: <https://docs.litellm.ai/docs/routing>,
<https://docs.litellm.ai/docs/proxy/reliability>

### OpenRouter (hosted aggregator)

Default = **price-weighted load balancing** (weight ∝ 1/price²) across
providers that haven't seen outages in the last 30s; remaining providers
become fallbacks. Key insight: **specifying `order` disables load balancing** —
explicit priority and load balancing are two mutually exclusive modes.
Per-request knobs: `sort: price|throughput|latency`, `order`, `only`/`ignore`,
`allow_fallbacks`, `require_parameters` (capability filter), `max_price`, and
percentile throughput/latency thresholds (p50–p99 over a 5-min window) that
deprioritize rather than exclude.

Source: <https://openrouter.ai/docs/features/provider-routing>

### Portkey

Three composable modes: `single`, `loadbalance` (per-target `weight`),
`fallback` (with `on_status_codes` limiting which errors trigger failover —
a 400/403 never fails over when only `[429,500,502,503,504]` is listed).
**Nesting is the standout**: a loadbalance whose targets are themselves
fallback chains.

Source: <https://portkey.ai/docs/guides/use-cases/combining-routing-strategies>

### Bifrost (Go gateway — yardmaster's closest architectural cousin)

Adaptive load balancer, weighted strategies, circuit breaker, and — most
relevant to coding-plan fleets — **per-provider key pooling**: multiple API
keys for one provider act as one pool; a 429 retries with a *different key
from the same provider* before falling back to another provider.

Source: <https://github.com/maximhq/bifrost>

## Strategy vs. use case

| Strategy | Use when | Verdict for yardmaster |
|---|---|---|
| Priority chain + fallback | Unequal economics: subscription first, pay-as-you-go second | ✅ Global default — `cmdcode → ocg → API` chains |
| Round-robin / weighted | Equal-cost mirrors (3 `ocg` wires, deepseek + openrouter) to spread quota | ✅ As an option within a route, not a global replacement |
| 429-triggered key rotation | Provider with N keys/accounts, per-key limits (coding plans!) | ✅ Biggest practical win |
| Cooldown memory (429 → skip for Retry-After; N-fails breaker) | Avoid hit-primary-fail-fallback on every request during an outage | ✅ Cheap, high value |
| Least-busy (in-flight) | Heterogeneous request durations, high concurrency | ❌ Marginal for personal fleets |
| Latency-based (EMA) | Serving user-facing chat | ❌ Agent workloads tolerate latency; adds state |
| Cost-based routing | Equivalent models at different prices | ❌ Subscription economics trump price here |
| Usage/rate-limit aware (TPM headroom) | Known per-account quotas | ❌ Needs per-key usage accounting; only worth it at volume |
| Sticky sessions | Providers with prompt caching (DeepSeek auto-cache, Z.ai cache_control) | ⏸️ Deferred — would raise cache-hit rate materially |
| Capability filters (`require_parameters`) | Aggregators serving many mirrors | ❌ Curated model lists already do this |

## Yardmaster implementation (what shipped)

The **priority chain is the global strategy** (matches subscription-first
economics; OpenRouter's `order` mode). Selection is a three-level inherit —
built-in default → global `routing:` section → per-route/per-provider
override — and all opt-in via config, so defaults reproduce the previous
behavior exactly:

1. **Global routing defaults** — `routing.strategy` (priority | weighted-rr)
   and `routing.rotation` (first | round_robin) are the fleet-wide defaults
   every route and provider inherits when it doesn't set its own.
2. **Per-route strategy override** — `strategy: weighted-rr` + `weights` on a
   route spreads requests across chain providers proportionally (stateless
   counter); the chosen provider heads the target list, the rest remain
   fallbacks in config order (OpenRouter's LB-then-fallback shape). Empty
   strategy = inherit global.
3. **Per-provider key rotation** — `rotation: round_robin` rotates the
   starting key/account per request (empty = inherit global, else `first`).
4. **Cooldown memory** (static keys; oauth keeps its own pool ladder) —
   429 cools that provider+key for `Retry-After` seconds (default 30s);
   3 × 5xx (500/502/503/504/529) within 60s cools for 30s; success resets.
   Cooling targets are skipped at dispatch. Thresholds are hardcoded
   deliberately — promote to config when someone actually needs a knob.

### Rejected / deferred

- **Sticky sessions** — deferred follow-up; session-id → provider pin with TTL
  for prompt-cache hit rate.
- Latency-based, least-busy, cost-based, usage-headroom routing — no payoff at
  this fleet size.
- Random key pick — round_robin spreads quota deterministically.
- 401/403 dead-key cooling — cheap follow-up if bad keys become a nuisance.
- Unifying oauth `OAuthPool` cooldowns with the generic cooldown map — two
  mechanisms coexist; merge only if confusion ever arises.
