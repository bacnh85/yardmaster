# Classifier wire — System One decision models (TypeSafe Jev)

yardmaster's `classifier` wire proxies **decision models** — TypeSafe's System
One family (Jev). Unlike chat wires, these models return typed answers with
probabilities, never text. They are tagged `family: classifier` in the catalog,
excluded from `/v1/models` and the pi models.json feed, and appear in the
Models tab with a `clf` badge.

## What Jev is

Send a `state` (any JSON) plus typed questions; get one typed answer per
question. Three primitives:

| Primitive | Answers | Returns |
|---|---|---|
| `noul` | Is this true? | P(yes), 0–1 |
| `choice` | Which option? | option + per-option probabilities + confidence |
| `score` | Where on this scale? | probability-weighted position + confidence |

$0.042/Mtok input, **output tokens free**. 32k-token state context. English is
the strongest language (CJK weaker — test on your content). Questions run in
parallel inside one request; probabilities jitter ±0.08 between identical
calls — thresholds are policy, tune them on your own traffic.

## Endpoints (inbound, same handler)

| Path | Use |
|---|---|
| `POST /v1/systemone` | canonical — TypeSafe-SDK compatible (`TYPESAFE_BASE_URL=https://<router>/v1`) |
| `POST /v1/decisions` | OpenRouter Decisions-SDK style callers |
| `POST /v1/classifier` | generic alias |
| `GET /v1/systemone/models` | list decision models (prefixed ids) — discovery path for System One clients (pi-classifier); `/v1/models` stays chat-only |

Request: `{"model": "...", "state": {...}, "questions": {...}}` → response:
`{"model": "...", "answers": {...}, "usage": {"input_tokens": N, "output_tokens": 0}}`.
Non-streaming JSON only. Errors pass through verbatim (401 / 422 / 429+Retry-After / 529).

## Upstreams

One wire, one path — every upstream appends `/systemone` to `base_url`
(OpenRouter, TypeSafe direct, and Command Code's Provider API):

```yaml
# Billed to your OpenRouter credits (Jev is NOT in OpenRouter's /models
# catalog — the curated models list is required):
- name: openrouter-classifier
  base_url: https://openrouter.ai/api/v1
  wire: classifier
  prefix: or
  auth: { type: static, keys: ["sk-or-..."] }
  models: [typesafe/jev-1.13, "~typesafe/jev-latest"]

# TypeSafe direct (separate account/key, same shape):
- name: typesafe
  base_url: https://api.typesafe.ai/v1
  wire: classifier
  prefix: jev
  auth: { type: static, keys: ["ts-..."] }
  models: [jev-latest, jev-1.13.0]

# Command Code (GOAT+ plans, same credits as chat models, $0.04/M input):
# id is the bare "typesafe/jev"; NOT in cmdcode's /models catalog — the
# curated list is required. Dashboard preset: cmdcode-classifier.
- name: cmdcode-classifier
  base_url: https://api.commandcode.ai/provider/v1
  wire: classifier
  prefix: cmd-jev   # or share "cmd" with the chat entries
  auth: { type: static, keys: ["CMD_API_KEY"] }
  models: [typesafe/jev]
```

Config rules (validated): classifier providers must carry a curated `models`
list (no wildcard), and cannot appear in chat routes. Requests resolve by the
normal prefix rules — `jev/jev-latest` or (OpenRouter entry) the natural id
`or/typesafe/jev-1.13`.

## Decision combos (pool Jev across upstreams)

A combo with `type: decision` pools decision models across classifier
providers behind one `combo/<name>` id, with the usual priority/weighted-rr
dispatch and failover. It is validated to contain classifier members only,
advertises on `/v1/systemone/models` (never `/v1/models`), and is requested on
`/v1/systemone` like any other decision model:

```yaml
combos:
  - name: jev
    type: decision
    members:
      - provider: typesafe
        model: jev-latest
      - provider: openrouter-classifier
        model: typesafe/jev-1.13
      - provider: cmdcode-classifier
        model: typesafe/jev
```

```bash
curl -s http://localhost:8787/v1/systemone \
  -H "Authorization: Bearer $ROUTER_KEY" \
  -d '{"model":"combo/jev","state":{...},"questions":{...}}'
```

Mesh combos in the dashboard set `type` in the combo dialog: choosing
"decision models" filters the member pickers to classifier providers and their
decision ids, so illegal members cannot be picked in the first place.

Pin `jev-1.13` / `jev-1.13.0` when thresholds are tuned against one version;
aliases (`jev-latest`, `~typesafe/jev-latest`) move under you.

## Testing a call

```bash
curl -s http://localhost:8787/v1/systemone \
  -H "Authorization: Bearer $ROUTER_KEY" \
  -d '{"model":"jev/jev-latest",
       "state":{"command":"bun test","task":"add classifier tests"},
       "questions":{"reversible":{"type":"noul","instructions":"Is this command reversible?"}}}'
```

Usage lands in the normal requests table (tok_in / cost from the input price;
output is always 0). Cost defaults to Jev's $0.042/Mtok input via the built-in
cost table; override per model in `costs:` if your rate differs.

## Agent pickup

pi agents use the **pi-classifier** extension (classify tool + optional
permission auto-approve hook) pointed at this endpoint — see that extension's
README. Decision models are never advertised as chat models.

## Rejected / deferred

- Decisions-vs-SystemOne dual upstream support: identical shapes; one wire.
- TypeSafe `/v1/models` auto-discovery: curated-only for now.
- Cascade verification (cheap chat draft → Jev verify → escalate) as a router
  strategy: application-level orchestration; clients can build it on this
  endpoint today.
