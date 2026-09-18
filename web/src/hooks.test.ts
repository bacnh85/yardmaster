import { describe, expect, it } from "vitest";
import { cmpVals } from "./hooks";

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
