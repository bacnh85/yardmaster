#!/usr/bin/env python3
"""Real-provider perf probe through agent-router (or direct upstream).
Measures TTFT, tok/s, and usage per request. stdlib only."""
import argparse, json, sys, time, urllib.request

def stream_once(base, key, wire, model, prompt, max_tokens=256, timeout=120):
    if wire == "openai":
        url, body, hdrs = base.rstrip("/") + "/chat/completions", {
            "model": model, "stream": True, "max_tokens": max_tokens,
            "messages": [{"role": "user", "content": prompt}]}, {"Authorization": "Bearer " + key}
    else:
        url, body, hdrs = base.rstrip("/") + "/v1/messages", {
            "model": model, "stream": True, "max_tokens": max_tokens,
            "messages": [{"role": "user", "content": prompt}]}, {"x-api-key": key, "anthropic-version": "2023-06-01"}
    hdrs["Content-Type"] = "application/json"
    data = json.dumps(body).encode()
    req = urllib.request.Request(url, data=data, headers=hdrs)
    t0 = time.perf_counter()
    resp = urllib.request.urlopen(req, timeout=timeout)
    ttft = None
    text_len = 0
    usage = {}
    nchunks = 0
    for raw in resp:
        line = raw.decode("utf-8", "replace").strip()
        if not line.startswith("data:"):
            continue
        payload = line[5:].strip()
        if ttft is None:
            ttft = time.perf_counter() - t0
        if payload == "[DONE]":
            break
        try:
            ev = json.loads(payload)
        except json.JSONDecodeError:
            continue
        nchunks += 1
        if wire == "openai":
            if ev.get("usage"):
                usage = ev["usage"]
            for ch in ev.get("choices", []):
                d = ch.get("delta") or {}
                text_len += len(d.get("content") or "")
        else:
            if ev.get("type") == "message_start":
                u = (ev.get("message") or {}).get("usage") or {}
                usage["prompt_tokens"] = u.get("input_tokens")
                usage["cache_read_tokens"] = u.get("cache_read_input_tokens")
            if ev.get("type") == "message_delta":
                u = ev.get("usage") or {}
                usage["completion_tokens"] = u.get("output_tokens")
            d = ev.get("delta") or {}
            if d.get("type") == "text_delta":
                text_len += len(d.get("text") or "")
    dur = time.perf_counter() - t0
    out_toks = usage.get("completion_tokens") or (text_len // 4)
    gen_s = max(dur - (ttft or 0), 1e-6)
    in_toks = usage.get("prompt_tokens") or 0
    cache_r = usage.get("cache_read_tokens") or 0
    return {"ttft_s": round(ttft or 0, 3), "dur_s": round(dur, 3),
            "tok_out": out_toks, "tok_s": round(out_toks / gen_s, 1) if gen_s else 0,
            "tok_in": in_toks, "cache_read": cache_r, "chunks": nchunks,
            "status": resp.status}

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--url", default="http://127.0.0.1:8790/v1")
    ap.add_argument("--key", default="ar-test0001")
    ap.add_argument("--wire", default="openai", choices=["openai", "anthropic"])
    ap.add_argument("--model", required=True)
    ap.add_argument("--n", type=int, default=3)
    ap.add_argument("--max-tokens", type=int, default=256)
    ap.add_argument("--prompt", default="Write a detailed explanation of how database B-trees work, with examples.")
    args = ap.parse_args()
    rows = []
    for i in range(args.n):
        try:
            r = stream_once(args.url, args.key, args.wire, args.model, args.prompt, args.max_tokens)
        except Exception as e:
            print(f"  [{i+1}] ERROR: {e}", file=sys.stderr)
            continue
        rows.append(r)
        print(f"  [{i+1}] ttft={r['ttft_s']}s dur={r['dur_s']}s out={r['tok_out']}tok speed={r['tok_s']}tok/s in={r['tok_in']} cache_r={r['cache_read']}")
    if rows:
        avg_speed = sum(r["tok_s"] for r in rows) / len(rows)
        avg_ttft = sum(r["ttft_s"] for r in rows) / len(rows)
        print(f"SUMMARY model={args.model} wire={args.wire} n={len(rows)} "
              f"avg_ttft={avg_ttft:.3f}s avg_speed={avg_speed:.1f}tok/s")

if __name__ == "__main__":
    main()
