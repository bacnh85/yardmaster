// @vitest-environment jsdom
// Requests tab infinite scroll: the sentinel below the table pages in strictly
// older rows via `before=<oldest id>` and appends them (the live head poll must
// not wipe them). jsdom has no IntersectionObserver — it is stubbed and fired
// by hand, per models-expand.test.tsx.
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { RequestsTab, PAGE } from "./RequestsTab";
import type { ReqRow } from "../api";

const row = (id: number): ReqRow => ({
  id, ts: 1_790_000_000_000 + id * 1000, key: "k", model: `m${id}`, provider: "p",
  status: 200, stream: true, ttft_ms: 100, dur_ms: 200, tok_in: 1, tok_out: 2,
  cache_read: 0, cache_write: 0, cost_usd: 0.1, err: "", attempts: 1, queue_ms: 0,
});
/** One page of rows, newest (highest id) first — what Recent returns. */
const page = (from: number, n = PAGE) => Array.from({ length: n }, (_, i) => row(from - i));

const get = vi.fn(async (path: string) => ({
  requests: page(Number(/before=(\d+)/.exec(path)?.[1]) - 1),
}));

vi.mock("../api", async (orig) => ({
  ...(await orig<typeof import("../api")>()),
  get: (p: string) => get(p),
}));
vi.mock("../hooks", async (orig) => ({
  ...(await orig<typeof import("../hooks")>()),
  useApi: () => ({ data: { requests: page(5000) }, error: "", loading: false, reload: () => {} }),
  usePoll: () => {},
}));

type Cb = (es: { isIntersecting: boolean }[]) => void;
const observers: { cb: Cb; el: Element | null }[] = [];
class IO {
  cb: Cb;
  el: Element | null = null;
  constructor(cb: Cb) { this.cb = cb; observers.push(this); }
  observe(el: Element) { this.el = el; }
  unobserve() {}
  disconnect() {}
}
(globalThis as Record<string, unknown>).IntersectionObserver = IO;
(globalThis as Record<string, unknown>).IS_REACT_ACT_ENVIRONMENT = true;

let host: HTMLDivElement | undefined;
let root: Root | undefined;
beforeEach(() => {
  observers.length = 0;
  get.mockClear();
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
});
afterEach(() => {
  act(() => root?.unmount());
  host?.remove();
});

const scroll = () => act(async () => {
  for (const o of observers) if (o.el?.isConnected) o.cb([{ isIntersecting: true }]);
});
const models = () => [...document.querySelectorAll("tbody tr")].map((r) => r.children[1].textContent);

describe("RequestsTab lazy paging", () => {
  it("pages in older rows on scroll, cursor = oldest loaded id, no duplicates", async () => {
    await act(async () => root?.render(<RequestsTab />));
    expect(models()).toHaveLength(PAGE);
    expect(models()[0]).toBe("m5000"); // newest first

    await scroll();
    expect(get).toHaveBeenCalledWith(`requests?limit=${PAGE}&before=4901`);
    const all = models();
    expect(all).toHaveLength(2 * PAGE);
    expect(all[all.length - 1]).toBe("m4801"); // oldest of page 2 appended after page 1
    expect(new Set(all).size).toBe(2 * PAGE); // strictly older, nothing repeated
  });

  it("stops at a short page and says so", async () => {
    get.mockImplementation(async () => ({ requests: page(4900, 5) }));
    await act(async () => root?.render(<RequestsTab />));
    await scroll();
    expect(models()).toHaveLength(PAGE + 5);
    expect(document.querySelector(".list-foot")?.textContent).toContain("end of history");
    const calls = get.mock.calls.length;
    await scroll(); // the observer must be gone
    expect(get).toHaveBeenCalledTimes(calls);
  });
});
