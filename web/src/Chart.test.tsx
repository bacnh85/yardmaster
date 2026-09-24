// @vitest-environment jsdom
// TimeChart lifecycle: ONE uPlot instance per config, in-place setData on poll
// ticks (the whole point — full rebuilds per tick used to shift the page while
// scrolled). uPlot is mocked; jsdom provides effects/observers without canvas.
// react-dom's act() warns unless this is set before modules load:
(globalThis as Record<string, unknown>).IS_REACT_ACT_ENVIRONMENT = true;
import React, { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { TimeChart } from "./Chart";
import type { Summary } from "./api";

const plots: MockPlot[] = [];

class MockPlot {
  destroyed = 0;
  setDataCalls: unknown[] = [];
  setSizeCalls: { width: number; height: number }[] = [];
  width = 600;
  constructor(public opts: unknown, public data: unknown) {}
  destroy() { this.destroyed++; }
  setData(d: unknown) { this.setDataCalls.push(d); }
  setSize(o: { width: number; height: number }) { this.setSizeCalls.push(o); this.width = o.width; }
}

vi.mock("uplot", () => ({
  default: class {
    constructor(opts: unknown, data: unknown) {
      const p = new MockPlot(opts, data);
      plots.push(p);
      return p;
    }
    // real uPlot carries the path-builder factories (spline etc.); the mock
    // mirrors the ones TimeChart may call at opts-build time
    static paths = { spline: () => (u: unknown) => null };
  },
}));
vi.mock("uplot/dist/uPlot.min.css", () => ({}));

// jsdom lacks the observers TimeChart wires up
beforeEach(() => {
  class RO { observe() {} disconnect() {} }
  class MO { observe() {} disconnect() {} }
  vi.stubGlobal("ResizeObserver", RO);
  vi.stubGlobal("MutationObserver", MO);
});
afterEach(() => {
  root?.unmount();
  root = undefined;
  plots.length = 0;
  vi.unstubAllGlobals();
  document.documentElement.removeAttribute("data-theme");
});

const pts = (n: number, ts = 1_700_000_000_000): Summary["series"] =>
  Array.from({ length: n }, (_, i) => ({ ts: ts + i * 3600_000, requests: i, errors: 0, tok_in: 1, tok_out: 2, cost: 0.5 }));
const SERIES = [{ key: "requests" as const, label: "requests" }];

let host: HTMLDivElement | undefined;
let root: Root | undefined;
function mount(ui: React.ReactNode) {
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
  act(() => root!.render(ui));
  return host;
}
/** effects run after paint; flush what React defers (passive effects on same tick) */
const rerender = (ui: React.ReactNode) => act(() => root!.render(ui));

describe("TimeChart lifecycle", () => {
  it("smooth sets spline paths, legend.live is always false (tooltip replaces it)", () => {
    mount(<TimeChart data={pts(24)} series={SERIES} />);
    let opts = plots[0].opts as uPlot.Options;
    expect((opts.legend as uPlot.Legend).live).toBe(false);
    expect((opts.series as uPlot.Series[])[1].paths).toBeUndefined(); // default stays linear

    plots.length = 0;
    mount(<TimeChart data={pts(24)} series={SERIES} smooth />);
    opts = plots[0].opts as uPlot.Options;
    expect(typeof (opts.series as uPlot.Series[])[1].paths).toBe("function"); // spline builder
    expect((opts.legend as uPlot.Legend).live).toBe(false);
    expect((opts.plugins as unknown[]).length).toBe(1); // tooltip plugin present
  });

  it("hover markers: points always on with a cursor-index filter (density-independent)", () => {
    mount(<TimeChart data={pts(40)} series={SERIES} />);
    const ser = (plots[0].opts as uPlot.Options).series as uPlot.Series[];
    expect(ser[1].points?.show).toBe(true); // not the old data.length<30 flip
    const filter = ser[1].points?.filter as (
      u: uPlot, seriesIdx: number, show: boolean, gaps?: number[][]
    ) => number[];
    const u = { cursor: { idxs: [null, 7] } } as unknown as uPlot;
    expect(filter(u, 1, true)).toEqual([7]); // hovered index → marker
    expect(filter(u, 1, true, undefined)).toEqual([7]);
    const none = { cursor: { idxs: [null, null] } } as unknown as uPlot;
    expect(filter(none, 1, true)).toEqual([]); // no hover → no markers
  });

  it("builds once and setData's in place when only data changes (poll tick)", () => {
    mount(<TimeChart data={pts(24)} series={SERIES} />);
    rerender(<TimeChart data={pts(24, 2_000_000_000_000)} series={SERIES} />);
    rerender(<TimeChart data={pts(24, 3_000_000_000_000)} series={SERIES} />);
    expect(plots).toHaveLength(1); // one instance across 3 data identities
    expect(plots[0].destroyed).toBe(0);
    // mount draws once (build effect creates + data effect ticks) + 2 poll ticks
    expect(plots[0].setDataCalls).toHaveLength(3);
    // full payload: [xs, one column per series] — values AND shape
    const SRC = pts(24, 3_000_000_000_000);
    expect(plots[0].setDataCalls[2]).toEqual([SRC.map((p) => p.ts / 1000), SRC.map((p) => p.requests)]);
  });

  it("rebuilds when the series labels change (legend must follow)", () => {
    mount(<TimeChart data={pts(24)} series={SERIES} />);
    rerender(<TimeChart data={pts(24)} series={[{ key: "requests", label: "renamed" }]} />);
    expect(plots).toHaveLength(2);
    expect(plots[0].destroyed).toBe(1);
  });

  it("empty→data and data→empty mount/unmount the chart exactly once per flip", () => {
    const { rerender: rr } = { rerender };
    mount(<TimeChart data={[]} series={SERIES} />);
    expect(plots).toHaveLength(0);
    rr(<TimeChart data={pts(24)} series={SERIES} />);
    expect(plots).toHaveLength(1);
    rr(<TimeChart data={pts(24, 9e12)} series={SERIES} />);
    expect(plots).toHaveLength(1); // still in-place
    rr(<TimeChart data={[]} series={SERIES} />);
    expect(plots[0].destroyed).toBe(1);
    expect(host!.querySelector(".empty")?.textContent).toBe("no data in range");
  });

  it("crosses the 30-point threshold → rebuild; same side of it → setData", () => {
    mount(<TimeChart data={pts(24)} series={SERIES} />);
    rerender(<TimeChart data={pts(25)} series={SERIES} />);
    expect(plots).toHaveLength(1); // in place (marker density is no longer a build dep)
    rerender(<TimeChart data={pts(31)} series={SERIES} />);
    expect(plots).toHaveLength(1); // in place — points.show is always true now
    rerender(<TimeChart data={pts(31, 9e12)} series={SERIES} />);
    const SRC = pts(31, 9e12);
    expect(plots[0].setDataCalls.at(-1)).toEqual([SRC.map((p) => p.ts / 1000), SRC.map((p) => p.requests)]);
    expect(plots[0].destroyed).toBe(0);
  });

  it("rebuilds on data-theme change, redrawing the latest committed data", () => {
    const mos: { cb: MutationRecord[] extends never ? never : MutationCallback }[] = [];
    class MO {
      cb: MutationCallback;
      constructor(cb: MutationCallback) { this.cb = cb; mos.push(this as never); }
      observe() {}
      disconnect() {}
    }
    vi.stubGlobal("MutationObserver", MO);
    mount(<TimeChart data={pts(24)} series={SERIES} />);
    rerender(<TimeChart data={pts(24, 5e12)} series={SERIES} />); // committed tick
    expect(mos.length).toBeGreaterThan(0);
    act(() => {
      document.documentElement.setAttribute("data-theme", "dark");
      mos[mos.length - 1].cb([], {} as MutationObserver);
    });
    expect(plots).toHaveLength(2); // theme rebuild
    // full payload from the LATEST committed tick (ts offset 5e12), not the initial one
    const SRC = pts(24, 5e12);
    expect(plots[1].data).toEqual([SRC.map((p) => p.ts / 1000), SRC.map((p) => p.requests)]);
  });

  it("resize calls setSize on the existing plot and never destroys it", () => {
    const ros: { clientWidth: number; cb: ResizeObserverCallback }[] = [];
    class RO {
      cb: ResizeObserverCallback;
      clientWidth: number;
      constructor(cb: ResizeObserverCallback) { this.cb = cb; this.clientWidth = 0; ros.push(this); }
      observe(target: HTMLElement) { this.clientWidth = target.clientWidth; }
      disconnect() {}
    }
    vi.stubGlobal("ResizeObserver", RO);
    vi.stubGlobal("ResizeObserverEntry", class {});
    mount(<TimeChart data={pts(24)} series={SERIES} />);
    // jsdom reports clientWidth 0 — fake the observed element width via the entry
    const entry = { contentRect: { width: 500 }, target: host!.querySelector(".chart") } as unknown as ResizeObserverEntry;
    Object.defineProperty(host!.querySelector(".chart")!, "clientWidth", { value: 500, configurable: true });
    plots[0].width = 600; // old width differs by >2px → must resize, not rebuild
    act(() => {
      ros[ros.length - 1].cb([entry], {} as ResizeObserver);
    });
    expect(plots).toHaveLength(1);
    expect(plots[0].destroyed).toBe(0);
    expect(plots[0].setSizeCalls).toEqual([{ width: 500, height: 220 }]);
  });
});
