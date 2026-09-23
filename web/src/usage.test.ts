// UsageTab pure helpers: bucketing, share math, heatmap levels, top-5 pivot.
// @vitest-environment jsdom
import { describe, expect, it, vi } from "vitest";
// UsageTab pulls in Chart.tsx → real uplot; jsdom lacks what uPlot needs at import
vi.mock("uplot", () => ({ default: class { width = 0; destroy() {} setData() {} setSize() {} } }));
vi.mock("uplot/dist/uPlot.min.css", () => ({}));
import { bucketFor, sharePct, heatLevel, heatCells, weekdayTotals, busiestDay, pivotModels } from "./tabs/UsageTab";

describe("bucketFor", () => {
  it("minute at 1h, hour for 24h-30d, day at 90d", () => {
    expect(bucketFor(1)).toBe("minute");
    expect(bucketFor(24)).toBe("hour");
    expect(bucketFor(168)).toBe("hour");
    expect(bucketFor(720)).toBe("hour");
    expect(bucketFor(2160)).toBe("day");
  });
});

describe("sharePct", () => {
  it("rounds, and never divides by zero", () => {
    expect(sharePct(1, 3)).toBe(33);
    expect(sharePct(2, 4)).toBe(50);
    expect(sharePct(10, 0)).toBe(0);
  });
});

describe("heatLevel", () => {
  it("0 for empty, capped at 4, quartile-ish in between", () => {
    expect(heatLevel(0, 100)).toBe(0);
    expect(heatLevel(10, 0)).toBe(0); // degenerate max
    expect(heatLevel(5, 100)).toBe(1);
    expect(heatLevel(50, 100)).toBe(3);
    expect(heatLevel(100, 100)).toBe(4);
    expect(heatLevel(1000, 100)).toBe(4);
  });
});

const day = (y: number, m: number, d: number) => Date.UTC(y, m - 1, d); // UTC noon-less: buckets are UTC midnights

describe("heatCells + weekdayTotals + busiestDay", () => {
  const series = [
    { ts: day(2026, 9, 21), tok_in: 100, tok_out: 10 }, // Monday
    { ts: day(2026, 9, 26), tok_in: 40, tok_out: 10 }, // Saturday
    { ts: day(2026, 9, 27), tok_in: 80, tok_out: 20 }, // Sunday
  ];
  const cells = heatCells(series as never);
  it("keys by local date and sums in+out", () => {
    expect(cells.map((c) => c.tokens)).toEqual([110, 50, 100]);
    expect(cells[0].key).toBe("2026-09-21");
  });
  it("aggregates Monday-first", () => {
    const wk = weekdayTotals(cells);
    expect(wk[0]).toBe(110); // Mon
    expect(wk[5]).toBe(50); // Sat
    expect(wk[6]).toBe(100); // Sun
    expect(wk.reduce((a, b) => a + b, 0)).toBe(260);
  });
  it("picks the busiest day", () => {
    expect(busiestDay(cells)?.key).toBe("2026-09-21");
    expect(busiestDay([])).toBe(null);
  });
});

describe("pivotModels", () => {
  const p = (ts: number, model: string, tok_in: number, tok_out = 0) => ({ ts, model, tok_in, tok_out, cost: 0 });
  it("keeps top N models by total tokens, collapses the rest into other", () => {
    const { models, rows } = pivotModels([
      p(1000, "big", 500),
      p(1000, "mid", 100),
      p(1000, "tiny-a", 10),
      p(1000, "tiny-b", 5),
      p(2000, "big", 50),
      p(2000, "tiny-a", 30),
    ], 2);
    expect(models).toEqual(["big", "mid", "other"]);
    // rows only carry models with traffic at that bucket — TimeChart gaps them
    expect(rows).toEqual([
      { ts: 1000, big: 500, mid: 100, other: 15 },
      { ts: 2000, big: 50, other: 30 },
    ]);
  });
  it("omits other when top N covers everything", () => {
    const { models, rows } = pivotModels([p(1000, "a", 5), p(2000, "b", 3)], 5);
    expect(models).toEqual(["a", "b"]);
    expect(rows).toEqual([
      { ts: 1000, a: 5 },
      { ts: 2000, b: 3 },
    ]);
  });
  it("empty input → no rows", () => {
    expect(pivotModels([])).toEqual({ models: [], rows: [] });
  });
});
