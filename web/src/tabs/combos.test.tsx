import { describe, expect, it } from "vitest";
import { draftOf, parseDraft, modelIds, memberAccounts, providerModels, comboTotals } from "./CombosTab";
import type { ComboRow, ComboUsageRow, ProviderRow, CatalogModel } from "../api";

const row: ComboRow = {
  name: "ds-flash", model: "deepseek-v4.1-flash", strategy: "weighted-rr",
  members: [
    { provider: "opencode-go", model: "deepseek-v4-flash", keys: ["main"], weight: 3 },
    { provider: "cmdcode" },
  ],
};

describe("draftOf/parseDraft round-trip", () => {
  it("keeps strategy, member model, pinned keys and weights", () => {
    const d = draftOf(row);
    expect(d.strategy).toBe("weighted-rr");
    expect(d.members[0]).toMatchObject({ provider: "opencode-go", model: "deepseek-v4-flash", keys: ["main"], weight: "3" });
    // omitted fields come back omitted
    const back = parseDraft(d);
    expect(back).toEqual(row);
  });

  it("inherit strategy and empty member fields stay absent", () => {
    const back = parseDraft({ name: "x", model: "m", strategy: "", members: [{ provider: "p", model: "", keys: [], weight: "" }] });
    expect(back.strategy).toBeUndefined();
    expect(back.members[0]).toEqual({ provider: "p" });
  });

  it("bad weight and blank provider survive parsing (server validates)", () => {
    const back = parseDraft({ name: "x", model: "m", strategy: "", members: [{ provider: "p", model: "", keys: [], weight: "abc" }] });
    expect(back.members[0].weight).toBeUndefined();
  });
});

describe("modelIds", () => {
  it("unions provider curated lists with catalog ids, sorted", () => {
    const providers = [{ name: "a", models: ["m2", "m1"] }, { name: "b", models: ["m3"] }] as unknown as ProviderRow[];
    const catalog = [{ id: "m0" }, { id: "m2" }] as unknown as CatalogModel[];
    expect(modelIds(providers, catalog)).toEqual(["m0", "m1", "m2", "m3"]);
  });
});

describe("memberAccounts", () => {
  it("static connection labels first, then oauth account names", () => {
    const providers = [{
      name: "a",
      connections: [{ label: "Key 1" }, { label: "work" }],
      accounts: [{ name: "acct1" }],
    }] as unknown as ProviderRow[];
    expect(memberAccounts(providers, "a")).toEqual(["Key 1", "work", "acct1"]);
    expect(memberAccounts(providers, "nope")).toEqual([]);
    expect(memberAccounts(null, "a")).toEqual([]);
  });
});

describe("providerModels", () => {
  it("returns the provider's curated model list", () => {
    const providers = [
      { name: "a", models: ["m1", "m2"] },
      { name: "b", models: [] },
    ] as unknown as ProviderRow[];
    expect(providerModels(providers, "a")).toEqual(["m1", "m2"]);
    expect(providerModels(providers, "b")).toEqual([]);
    expect(providerModels(providers, "nope")).toEqual([]);
  });
});

describe("comboTotals", () => {
  it("sums requests, errors, failovers and cost", () => {
    const rows: ComboUsageRow[] = [
      { combo: "combo/x", provider: "a", requests: 3, errors: 0, failovers: 0, tok_in: 0, tok_out: 0, cache_read: 0, cost: 0.5, avg_attempts: 1 },
      { combo: "combo/x", provider: "b", requests: 1, errors: 1, failovers: 1, tok_in: 0, tok_out: 0, cache_read: 0, cost: 0.25, avg_attempts: 2 },
    ];
    expect(comboTotals(rows)).toEqual({ requests: 4, errors: 1, failovers: 1, cost: 0.75 });
    expect(comboTotals([])).toEqual({ requests: 0, errors: 0, failovers: 0, cost: 0 });
  });
});
