// EndpointsTab keys-table budget cell: $spent/$cap rendering, the uncapped
// dash, and the rounded month_limit_pct tooltip.
import { describe, expect, it } from "vitest";
import { budgetCell } from "./tabs/EndpointsTab";
import type { KeyRow } from "./api";

const key = (over: Partial<KeyRow> = {}): KeyRow => ({
  name: "k", key_suffix: "ab12cd", allow: ["*"], rpm: 0, ...over,
});

describe("budgetCell", () => {
  it("renders $spent / $cap at 2 decimals with a rounded pct title", () => {
    const b = budgetCell(key({ monthly_usd: 10, month_spent: 4.2, month_limit_pct: 42.4 }));
    expect(b.text).toBe("$4.20 / $10.00");
    expect(b.title).toBe("42% of cap");
  });

  it("shows a dash with no title for uncapped keys (monthly_usd 0/absent)", () => {
    for (const k of [key(), key({ monthly_usd: 0 }), key({ monthly_usd: 0, month_spent: 5, month_limit_pct: 0 })]) {
      expect(budgetCell(k)).toEqual({ text: "–" });
    }
  });

  it("shows over-cap percents above 100 and tolerates a missing pct (older servers)", () => {
    expect(budgetCell(key({ monthly_usd: 2, month_spent: 3, month_limit_pct: 150.2 })).title).toBe("150% of cap");
    const b = budgetCell(key({ monthly_usd: 4, month_spent: 0 }));
    expect(b.text).toBe("$0.00 / $4.00");
    expect(b.title).toBeUndefined(); // month_limit_pct omitted when unlimited server-side
  });
});
