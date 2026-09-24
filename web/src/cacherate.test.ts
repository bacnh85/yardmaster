// CacheTab pulls in Chart.tsx → real uplot; jsdom lacks what uPlot needs at import
// @vitest-environment jsdom
import { describe, expect, it, vi } from "vitest";
vi.mock("uplot", () => ({ default: class { width = 0; destroy() {} setData() {} setSize() {} } }));
vi.mock("uplot/dist/uPlot.min.css", () => ({}));
import { cacheRate } from "./tabs/CacheTab";

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
