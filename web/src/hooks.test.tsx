import { describe, expect, it } from "vitest";
import { renderToString } from "react-dom/server";
import { cmpVals, useSorted } from "./hooks";

describe("cmpVals", () => {
  it("sorts numbers numerically, not lexically", () => {
    expect([1240, 320, 45].sort((a, b) => cmpVals(a, b, 1))).toEqual([45, 320, 1240]);
    expect([1240, 320, 45].sort((a, b) => cmpVals(a, b, -1))).toEqual([1240, 320, 45]);
  });
  it("sorts nulls last regardless of direction (ttft_p50_ms is number|null)", () => {
    const rows: (number | null)[] = [320, null, 1240];
    expect([...rows].sort((a, b) => cmpVals(a, b, 1))).toEqual([320, 1240, null]);
    expect([...rows].sort((a, b) => cmpVals(a, b, -1))).toEqual([1240, 320, null]);
  });
  it("compares strings lexically", () => {
    expect(["b", "a", "c"].sort((x, y) => cmpVals(x, y, 1))).toEqual(["a", "b", "c"]);
  });
});

describe("useSorted.th", () => {
  // hooks only run inside a render — SSR is enough to pin the header markup
  const renderTh = (num: boolean, cls: string) => {
    const Probe = () => {
      const { th } = useSorted([{ id: 1, n: 2 }], "n");
      return <table><thead><tr>{th("n", "n", num, cls)}</tr></thead></table>;
    };
    return renderToString(<Probe />);
  };

  it("composes n + sortable + the responsive column class in order", () => {
    expect(renderTh(true, "col-md")).toContain('class="n sortable col-md"');
  });

  it("keeps defaults for plain string columns (no stray classes)", () => {
    expect(renderTh(false, "")).toContain('class="sortable"');
  });

  it("marks the active sort with aria-sort", () => {
    expect(renderTh(false, "")).toContain('aria-sort="descending"');
  });
});
