import { describe, expect, it } from "vitest";
import { renderToString } from "react-dom/server";
import { fmtResetIn, QuotaWindow, QuotaAccount } from "./api";
import { pct, monthlyWindow, isQuotaProvider, QuotaTable } from "./tabs/QuotaTab";

const mins = (n: number) => Date.now() + n * 60000;

describe("fmtResetIn", () => {
  it.each([
    [undefined, "—"],
    [0, "—"],
    [Date.now() - 5000, "—"], // past
    [mins(30), "30m"],
    [mins(59), "59m"],
    [mins(60), "1h"],
    [mins(237), "3h 57m"], // the screenshot's 5-hour reset label
    [mins(24 * 4 * 60), "4d"],
    [mins((24 * 4 + 2) * 60), "4d 2h"],
  ])("%s -> %s", (at, want) => {
    expect(fmtResetIn(at as number | undefined)).toBe(want);
  });
});

describe("pct", () => {
  const w = (used: number, cap: number): QuotaWindow => ({ used, cap });
  it.each([
    [undefined, 0],
    [w(0, 14), 0],
    [w(7, 35), 20],
    [w(0.5, 14), 4],
    [w(14, 14), 100],
    [w(20, 14), 100], // over cap clamps
    [w(1, 0), 0], // uncapped
  ])("%j -> %i", (win, want) => {
    expect(pct(win as QuotaWindow | undefined)).toBe(want);
  });
});

describe("monthlyWindow", () => {
  it("derives used/cap from plan total and remaining credits", () => {
    const a = { label: "k", suffix: "s", monthly_credits: 0.01, monthly_total: 70 } as QuotaAccount;
    expect(monthlyWindow(a)).toEqual({ used: 69.99, cap: 70 });
    expect(pct(monthlyWindow(a))).toBe(100); // clamp holds for the monthly bar too
  });
  it("is undefined without a known plan total", () => {
    expect(monthlyWindow({ label: "k", suffix: "s", monthly_credits: 12 } as QuotaAccount)).toBeUndefined();
    expect(monthlyWindow({ label: "k", suffix: "s", monthly_total: 70 } as QuotaAccount)).toBeUndefined();
  });
});

describe("isQuotaProvider", () => {
  it.each([
    ["https://api.commandcode.ai/provider/v1", true],
    ["https://API.CommandCode.AI/provider/v1", true],
    ["https://api.commandcode.ai:8443/v1", false], // port present: server quotaSource also rejects (u.Host compare)
    ["https://api.deepseek.com", true],
    ["https://api.deepseek.com/anthropic", true],
    ["https://opencode.ai/zen/go/v1", true],
    ["https://opencode.ai/zen/v1", false], // Zen credits: no usage API
    ["https://opencode.ai", false],
    ["https://api.example.com/v1", false],
    ["", false],
    ["not a url", false],
  ])("%s -> %s", (url, want) => {
    expect(isQuotaProvider(url)).toBe(want);
  });
});

describe("QuotaTable key display", () => {
  const acct = (label: string, suffix: string): QuotaAccount => ({ label, suffix });

  it("exposes the suffix in every account cell (title + sr-only) so AT/touch keep the disambiguator", () => {
    const html = renderToString(<QuotaTable accounts={[acct("Key 1", "ab12cd")]} />);
    expect(html).toContain('title="key …ab12cd"');
    expect(html).toContain('class="sr-only"');
  });

  it("shows the suffix VISIBLY only when another account in the group shares the label", () => {
    const dup = renderToString(<QuotaTable accounts={[acct("Key 1", "ab12cd"), acct("Key 1", "ef34gh")]} />);
    expect(dup).toContain('title="key …ab12cd"');
    expect(dup).toContain('title="key …ef34gh"');
    expect(dup.match(/mono faint/g)?.length).toBe(2); // both duplicate rows get the visible suffix

    const unique = renderToString(<QuotaTable accounts={[acct("Key 1", "ab12cd")]} />);
    expect(unique).not.toContain("mono faint"); // unique label stays label-only
  });

  it("prefers an explicit monthly percent window (OpenCode Go) over the derived allowance", () => {
    const a: QuotaAccount = {
      label: "go", suffix: "xy99", monthly: { used: 80, cap: 100, unit: "pct", reset_at: mins(240) },
      monthly_credits: 3, monthly_total: 70, // would compute a different bar — explicit window wins
    };
    const html = renderToString(<QuotaTable accounts={[a]} />);
    expect(html).toContain("80%");
    expect(html).toContain("resets in");
    expect(html).toContain("4h");
    expect(html).not.toContain("$67");
  });
});
