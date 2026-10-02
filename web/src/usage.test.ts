// UsageTab pure helpers: bucketing, share math, heatmap levels, top-5 pivot.
// @vitest-environment jsdom
import { describe, expect, it, vi } from "vitest";
// UsageTab pulls in Chart.tsx → real uplot; jsdom lacks what uPlot needs at import
vi.mock("uplot", () => ({ default: class { width = 0; destroy() {} setData() {} setSize() {} } }));
vi.mock("uplot/dist/uPlot.min.css", () => ({}));
import { bucketFor, sharePct, heatLevel, heatCells, monthLabels, weekdayTotals, busiestDay, pivotModels, PIVOT_KEY } from "./tabs/UsageTab";

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
  const now = day(2026, 9, 30) + 5 * 3_600_000; // some instant inside 2026-09-30 UTC
  const cells = heatCells(series as never, now);
  it("zero-fills the full 12-month window (sparse days must not collapse the grid)", () => {
    expect(cells).toHaveLength(365);
    expect(cells[0].key).toBe("2025-10-01");
    expect(cells[cells.length - 1].key).toBe("2026-09-30");
    expect(cells.filter((c) => c.tokens > 0).map((c) => [c.key, c.tokens]))
      .toEqual([["2026-09-21", 110], ["2026-09-26", 50], ["2026-09-27", 100]]);
  });
  it("keeps the server's leading partial day (window boundary)", () => {
    const boundary = heatCells([{ ts: day(2025, 9, 30), tok_in: 7, tok_out: 0 }] as never, now);
    expect(boundary).toHaveLength(366);
    expect(boundary[0]).toMatchObject({ key: "2025-09-30", tokens: 7 });
  });
  it("aggregates Monday-first", () => {
    const wk = weekdayTotals(cells);
    expect(wk[0]).toBe(110); // Mon
    expect(wk[5]).toBe(50); // Sat
    expect(wk[6]).toBe(100); // Sun
    expect(wk.reduce((a, b) => a + b, 0)).toBe(260);
  });
  it("picks the busiest day, and none when every day is empty", () => {
    expect(busiestDay(cells)?.key).toBe("2026-09-21");
    expect(busiestDay(heatCells([], now))).toBe(null); // zero-filled ≠ "busiest"
    expect(busiestDay([])).toBe(null);
  });
});

describe("monthLabels", () => {
  const cells = heatCells([], day(2026, 9, 30));
  const labels = monthLabels(cells, (new Date(cells[0].ts).getUTCDay() + 6) % 7);
  it("labels every month once, at its starting week column", () => {
    expect(labels.map((l) => l.label))
      .toEqual(["Oct", "Nov", "Dec", "Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep"]);
    expect(labels[0]).toEqual({ label: "Oct", col: 0 }); // window starts 2025-10-01
    expect(labels.map((l) => l.col)).toEqual([...labels.map((l) => l.col)].sort((a, b) => a - b));
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
    // rows key models under PIVOT_KEY ("ts"/"other" names can't clobber the row) —
    // only models with traffic at that bucket appear — TimeChart gaps them
    expect(rows).toEqual([
      { ts: 1000, [PIVOT_KEY + "big"]: 500, [PIVOT_KEY + "mid"]: 100, [PIVOT_KEY + "other"]: 15 },
      { ts: 2000, [PIVOT_KEY + "big"]: 50, [PIVOT_KEY + "other"]: 30 },
    ]);
  });
  it("row.ts survives a model literally named 'ts'; 'other' the model merges into the aggregate", () => {
    const { models, rows } = pivotModels([
      p(1000, "ts", 5),
      p(1000, "other", 2),
      p(1000, "tiny", 1),
    ], 2); // top 2 = ts + other; tiny collapses into "other"
    expect(rows[0].ts).toBe(1000); // not clobbered by the "ts" model
    expect(models).toContain("other");
    expect(rows[0][PIVOT_KEY + "ts"]).toBe(5);
    expect(rows[0][PIVOT_KEY + "other"]).toBe(2 + 1); // model "other" + tiny collapse together
  });
  it("omits other when top N covers everything", () => {
    const { models, rows } = pivotModels([p(1000, "a", 5), p(2000, "b", 3)], 5);
    expect(models).toEqual(["a", "b"]);
    expect(rows).toEqual([
      { ts: 1000, [PIVOT_KEY + "a"]: 5 },
      { ts: 2000, [PIVOT_KEY + "b"]: 3 },
    ]);
  });
  it("empty input → no rows", () => {
    expect(pivotModels([])).toEqual({ models: [], rows: [] });
  });
});
