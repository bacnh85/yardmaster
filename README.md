# agent-router

Self-hosted LLM proxy for CLI coding agents. One API URL + key for all your
agents; one config entry per upstream provider. Go, single static binary,
embedded dashboard.

Built to replace OmniRoute / CLIProxyAPI / 9router with something minimal:
no MITM, no token compression, no OAuth monolith — just fast streaming
routing, honest stats, and isolated auth adapters.

## Features

- **Streaming-first**: byte-exact SSE passthrough when wires match; flush per
  chunk; client disconnect cancels the upstream call. Bench: **TTFT overhead
  p95 ≤ 1 ms** at 200–500 concurrent streams × 300 tok/s, zero stalls
  (`agent-router bench`).
- **Wire surface**: `POST /v1/chat/completions` (OpenAI) and `POST /v1/messages`
  (Anthropic) in; OpenAI or Anthropic wire per upstream. Full translation
  (tool calls, thinking/reasoning, usage) between the two.
- **Ordered failover**: model → provider chain; any upstream >=400 falls
  through to the next provider/key; the last real upstream error is surfaced.
- **Z.ai GLM Coding Plan support** (`session`-style providers, see config):
  Anthropic wire, `cache_control` injection (cache reads ≈ 0.1x input — the
  subscription multiplier), fast mode, adaptive thinking, and a per-key
  dispatch throttle that dodges the 429/1302 request-rate limit.
- **OpenCode Go support**: `session: opencode` providers send the required
  `x-opencode-session`/`x-opencode-client` headers (stable per key; a client
  header is forwarded when present).
- **Stats**: SQLite request log (async batched), per-model/provider/key
  breakdowns, TTFT p50/p95, cache hit rate, cost accounting (config-editable
  per-model prices; built-in estimates), live SSE feed, embedded dashboard.

## Quick start

```bash
cp config.example.yaml config.yaml   # fill in provider keys
go build -o agent-router ./cmd/agent-router
./agent-router run -c config.yaml    # http://127.0.0.1:8787
./agent-router key add my-laptop     # prints a key snippet for config.yaml
```

Dashboard: `http://127.0.0.1:8787/` (admin password from config).
Hot reload: edit `config.yaml` → `SIGHUP` or `POST /admin/api/reload`.

## Provider config

```yaml
providers:
  - name: zai
    base_url: https://api.z.ai/api/anthropic
    wire: anthropic
    session: opencode            # opencode-style session headers (opencode-go)
    auth: { type: static, keys: ["..."] }
    models: [glm-5.3, glm-5.3-flash]
    dispatch_interval_ms: 1000   # per-key spacing (Z.ai 429/1302)
    adaptive_thinking: true      # thinking adaptive + output_config.effort
    inject_cache_control: true   # ephemeral markers on system/tools/last msg
    extra_headers: { anthropic-beta: "fast-mode-2026-02-01" }
    body_overrides: { speed: fast }

routes:
  - match: "glm-*"
    chain: [zai]
  - match: "deepseek-v4-flash"
    chain: [opencode-go, deepseek]   # subscription first, platform fallback

keys:
  - { key: "ar-...", name: pi-laptop, allow: ["*"], rpm: 0 }
```

## Per-CLI setup (all against `http://127.0.0.1:8787`, key `ar-...`)

| CLI | Config |
|---|---|
| **pi** | OpenAI-compatible provider: `base_url: http://127.0.0.1:8787/v1` |
| **Claude Code** | `ANTHROPIC_BASE_URL=http://127.0.0.1:8787 ANTHROPIC_AUTH_TOKEN=ar-...` |
| **Codex** | `~/.codex/config.toml` → `model_providers.ar` with `base_url = "http://127.0.0.1:8787/v1"`, `wire_api = "chat"` |
| **opencode** | provider with OpenAI-compatible base URL `http://127.0.0.1:8787/v1` |
| **Cline / Cursor** | OpenAI-compatible base URL + key |

## Benchmarks

`agent-router bench -conns 500` — synthetic SSE upstream, N concurrent
streams, measures proxied-vs-direct TTFT:

```
streams=500  wall=2.684s  total-chunks=151500
TTFT direct   p50=3.0ms p95=5.0ms max=7.0ms
TTFT proxied  p50=3.0ms p95=5.0ms max=8.0ms
overhead      p50=0.0ms p95=0.0ms
stalls        0
phase-0 gate (overhead p95 < 5ms, 0 stalls): PASS
```

Real-provider spot checks through the router (2026-09-16): glm-5.3 via Z.ai
Anthropic passthrough ≈143 tok/s; deepseek-v4-flash via OpenCode Go ≈56–80
tok/s × 3 concurrent streams; glm-5.3-flash ≈36–41 tok/s (thinking included).

## Status

- Phases 0–3 done (core, translation, stats, dashboard). Phase 4 (OAuth
  account pools — Claude Code / Codex / opencode OAuth) designed, not built.
- Deferred: `/v1/responses` serving, Gemini-native wire.

## Layout

```
cmd/agent-router/    run | key add | bench
internal/config/     YAML config + validation + cost table
internal/translate/  openai<->anthropic requests/SSE
internal/provider/   registry + model routing
internal/auth/       inbound keys (RPM) + per-key dispatch limiter
internal/proxy/      streaming pipeline, failover, usage tee
internal/store/      SQLite request log + aggregates
internal/server/     endpoints, admin API, dashboard
web/                 React + uPlot dashboard (embedded via go:embed)
bench/               perf harness (also `agent-router bench`)
```
