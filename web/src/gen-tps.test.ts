import { describe, expect, it } from "vitest";
import { genTps } from "./api";

// ZCode desktop convention: output tokens ÷ (duration − TTFT). Wall-clock
// rates fold the multi-second server-side first-token wait into the divisor
// and read several times lower on short generations (e.g. Z.ai's glm-5.3-flash
// holds every request ~3–8s before the first byte, then decodes at 60–230
// tok/s — measured 2026-10-04, proxy adds ~0).
describe("genTps", () => {
  it("divides by the post-first-token window", () => {
    // 205 tokens over 6224ms total, 5524ms to first token → 205/0.7s ≈ 292.9
    expect(genTps({ tok_out: 205, dur_ms: 6224, ttft_ms: 5524 })).toBeCloseTo(292.857, 2);
  });

  it.each([
    [{ tok_out: 0, dur_ms: 5000, ttft_ms: 1000 }, null], // no output
    [{ tok_out: 100, dur_ms: 900, ttft_ms: 900 }, null], // degenerate: dur == ttft
    [{ tok_out: 100, dur_ms: 500, ttft_ms: 900 }, null], // dur < ttft (clock skew / early abort)
  ])("%j → %s", (r, want) => expect(genTps(r)).toBe(want));
});
