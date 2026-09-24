// CacheTab pulls in Chart.tsx → real uplot; jsdom lacks what uPlot needs at import
// @vitest-environment jsdom
import { describe, expect, it, vi } from "vitest";
vi.mock("uplot", () => ({ default: class { width = 0; destroy() {} setData() {} setSize() {} } }));
vi.mock("uplot/dist/uPlot.min.css", () => ({}));
import { cacheRate, cacheRowCells } from "./tabs/CacheTab";

// Cache Rate = requests with cache_read > 0 / total requests (OmniRoute
// reference: 26,658 / 42,321 = 63%). cached_requests comes from the server
// (SUM(CASE WHEN cache_read>0)) — undefined on pre-upgrade servers.
describe("cacheRate", () => {
  it.each([
    [26_658, 42_321, 63], // the reference screenshot numbers
    [0, 100, 0], // no hits
    [50, 50, 100], // fully cached
    [undefined, 100, null], // older server without the field → "–", not 0
  ])("cached=%s / requests=%s → %s", (cached, requests, want) => {
    expect(cacheRate(cached, requests)).toBe(want);
  });

  it("no traffic → null (ratio meaningless)", () => {
    expect(cacheRate(0, 0)).toBeNull();
  });
});

// CacheBreakTable row math, exactly as rendered. The rate/cached null-degradation
// keeps the table honest on pre-upgrade servers where cached_requests is absent
// (summary card shows "–" too) — no fabricated 0% / 0.
describe("cacheRowCells", () => {
  it("computes total/reuse/rate/cached for a normal row", () => {
    expect(cacheRowCells({ name: "p", requests: 100, errors: 0, tok_in: 300, tok_out: 0,
      cache_read: 600, cache_write: 100, cached_requests: 50, cost: 0, ttft_p50_ms: null }))
      .toEqual({ total: 1000, reuse: 60, rate: 50, cached: 50 });
  });

  it("no-traffic row: reuse 0, rate null (matches the card's no-data contract)", () => {
    expect(cacheRowCells({ name: "p", requests: 0, errors: 0, tok_in: 0, tok_out: 0,
      cache_read: 0, cost: 0, ttft_p50_ms: null }))
      .toEqual({ total: 0, reuse: 0, rate: null, cached: undefined });
  });

  it("pre-upgrade server (cached_requests absent): rate/cached degrade to null, reuse still computed", () => {
    const cells = cacheRowCells({ name: "p", requests: 10, errors: 0, tok_in: 70, tok_out: 0,
      cache_read: 30, cost: 0, ttft_p50_ms: null });
    expect(cells.rate).toBeNull();
    expect(cells.cached).toBeUndefined();
    expect(cells.reuse).toBe(30); // 30/100 — reuse only needs cache_read+tok_in
  });
});
