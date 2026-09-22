import { describe, expect, it } from "vitest";
import { cacheHitPct, totalInput } from "./components";

// tok_in is cache-EXCLUSIVE in the store (anthropic reports input_tokens as the
// uncached remainder; openai paths subtract cached — clamped, see usage_test.go
// TestUsageOpenAIMisreportedCachedClamped) — totalInput re-adds the cached parts.
describe("cacheHitPct", () => {
  it("uses total input as denominator — never exceeds 100%", () => {
    // the live 240% bug: 3,072,080 uncached + 7,373,312 cached
    expect(cacheHitPct({ tok_in: 3_072_080, cache_read: 7_373_312 })).toBe(71);
  });

  it.each([
    [{ tok_in: 0, cache_read: 0 }, 0], // no traffic → 0, not NaN
    [{ tok_in: 27, cache_read: 2688 }, 99], // zai live row: 27 of 2715 total
    [{ tok_in: 100, cache_read: 0 }, 0], // no cache → 0%
    [{ tok_in: 0, cache_read: 500 }, 100], // fully cached
  ])("%j → %i", (s, want) => expect(cacheHitPct(s)).toBe(want));

  it("counts cache_write in the denominator (anthropic excludes it from input_tokens)", () => {
    expect(cacheHitPct({ tok_in: 30, cache_read: 30, cache_write: 30 })).toBe(33); // 30/90
  });

  it("aggregate ratio is bounded by construction when rows respect the invariant", () => {
    // rows: anthropic-style 27/2688 and openai-style (post-subtraction) 60/40
    // summed → tok_in 87, cache_read 2728 → still a sane ≤100% ratio
    const s = { tok_in: 27 + 60, cache_read: 2688 + 40 };
    expect(cacheHitPct(s)).toBeLessThanOrEqual(100);
  });
});

describe("totalInput", () => {
  it.each([
    [{ tok_in: 30, cache_read: 30, cache_write: 30 }, 90],
    [{ tok_in: 100, cache_read: 0 }, 100],
    [{ tok_in: 0, cache_read: 0, cache_write: 0 }, 0],
  ])("%j → %i", (s, want) => expect(totalInput(s)).toBe(want));

  it("treats missing cache_write as 0 (BreakdownRow has no field)", () => {
    expect(totalInput({ tok_in: 3_072_080, cache_read: 7_373_312 })).toBe(10_445_392);
  });
});
