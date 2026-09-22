// @vitest-environment jsdom
// BreakTable "tok in" column must sort by the DISPLAYED value (total input =
// tok_in + cache_read), not the raw cache-exclusive field — otherwise a
// heavily-cached model shows a big number but sorts by its small remainder.
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { BreakTable } from "./tabs/UsageTab";
import { totalInput } from "./components";
import type { BreakdownRow } from "./api";

(globalThis as Record<string, unknown>).IS_REACT_ACT_ENVIRONMENT = true;
// UsageTab pulls in Chart.tsx → real uplot; jsdom lacks what uPlot needs at import
vi.mock("uplot", () => ({ default: class { width = 0; destroy() {} setData() {} setSize() {} } }));
vi.mock("uplot/dist/uPlot.min.css", () => ({}));

const rows = (dir: number): BreakdownRow[] => {
  const r = (name: string, tok_in: number, cache_read: number, cache_write = 0): BreakdownRow => ({
    name, requests: 1, errors: 0, tok_in, tok_out: 0, cache_read, cache_write, cost: 0, ttft_p50_ms: null,
  });
  // discriminating pair: RAW order (a<b) is the reverse of TOTAL order (a>b)
  // — requests are equal so only the tok-in key can move anything
  const base = [
    r("cached-model", 10, 1000), // total 1010 — biggest display, smallest raw
    r("uncached-model", 500, 0), // total 500 — biggest raw, smaller display
  ];
  return dir === 1 ? [...base].reverse() : base;
};

// card-vs-table reconciliation fixture: includes an anthropic cache-write row
// (cache_write is in the Summary shape but must also flow through BreakdownRow)
const cardRows: BreakdownRow[] = [
  { name: "w1", requests: 1, errors: 0, tok_in: 30, tok_out: 1, cache_read: 30, cache_write: 30, cost: 0, ttft_p50_ms: null },
  { name: "w2", requests: 1, errors: 0, tok_in: 100, tok_out: 1, cache_read: 0, cost: 0, ttft_p50_ms: null },
];

let host: HTMLDivElement | undefined;
let root: Root | undefined;
beforeEach(() => {
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
});
afterEach(() => {
  root?.unmount();
  root = undefined;
  host?.remove();
});

const clickTokIn = () => {
  const th = [...host!.querySelectorAll("th")].find((h) => h.textContent.startsWith("tok in"))!;
  act(() => { th.dispatchEvent(new MouseEvent("click", { bubbles: true })); });
};
const tokInCells = () => [...host!.querySelectorAll("tbody tr td:nth-child(4)")].map((td) => td.textContent);
const names = () => [...host!.querySelectorAll("tbody tr td:first-child")].map((td) => td.textContent);

describe("BreakTable tok-in sort key", () => {
  it.each([1, -1])("feed order %i: sorts by displayed total, not raw tok_in", (dir) => {
    act(() => root!.render(<BreakTable rows={rows(dir)} />));
    clickTokIn(); // first click: descending by the sort key
    expect(names()).toEqual(["cached-model", "uncached-model"]); // 1010 > 500
    clickTokIn(); // second click: ascending
    expect(names()).toEqual(["uncached-model", "cached-model"]); // 500 < 1010
  });

  it("displays totals, never the raw uncached field", () => {
    act(() => root!.render(<BreakTable rows={rows(1)} />));
    expect([...tokInCells()].sort((a, b) => +a.replace(/,/g, "") - +b.replace(/,/g, ""))).toEqual(["500", "1,010"]);
  });

  it("sort indicator tracks the derived column", () => {
    act(() => root!.render(<BreakTable rows={rows(1)} />));
    clickTokIn();
    const th = [...host!.querySelectorAll("th")].find((h) => h.textContent.startsWith("tok in"))!;
    expect(th.getAttribute("aria-sort")).toBe("descending");
  });

  it("reconciles with the Tokens-in card for cache-write traffic (sum of tok-in cells = card total)", () => {
    act(() => root!.render(<BreakTable rows={cardRows} />));
    const cellSum = tokInCells().reduce((acc, c) => acc + Number(c.replace(/,/g, "")), 0);
    // the card computes totalInput over the Summary aggregate of the same rows
    const cardTotal = totalInput({
      tok_in: cardRows.reduce((a, r) => a + r.tok_in, 0),
      cache_read: cardRows.reduce((a, r) => a + r.cache_read, 0),
      cache_write: cardRows.reduce((a, r) => a + (r.cache_write ?? 0), 0),
    });
    expect(cellSum).toBe(cardTotal); // 30+30+30 + 100 = 190, cache_write included twice over
  });
});
