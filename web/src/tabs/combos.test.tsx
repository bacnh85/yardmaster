import { describe, expect, it } from "vitest";
import { draftOf, parseDraft, providersForType, memberModelsFor, modelOptionLabel, modelMetaLine, memberAccounts, providerModels, comboTotals } from "./CombosTab";
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
    // omitted fields come back omitted… except the legacy combo model, which
    // resolves INTO the member (edit migrates the combo to per-member ids)
    const { model: _legacyModel, ...rest } = row;
    const back = parseDraft(d);
    expect(back).toEqual({ ...rest, members: [row.members[0], { provider: "cmdcode", model: "deepseek-v4.1-flash" }] });
  });

  it("inherit strategy and empty member fields stay absent", () => {
    const back = parseDraft({ name: "x", type: "chat", strategy: "", members: [{ provider: "p", model: "", keys: [], weight: "" }] });
    expect(back.strategy).toBeUndefined();
    expect(back.members[0]).toEqual({ provider: "p" });
  });

  it("bad weight and blank provider survive parsing (server validates)", () => {
    const back = parseDraft({ name: "x", type: "chat", strategy: "", members: [{ provider: "p", model: "", keys: [], weight: "abc" }] });
    expect(back.members[0].weight).toBeUndefined();
  });
});

describe("per-member models (combo model optional)", () => {
  it("legacy combo model resolves into members on edit", () => {
    expect(draftOf(row).members.map((m) => m.model)).toEqual(["deepseek-v4-flash", "deepseek-v4.1-flash"]);
  });
  it("per-member ids round-trip without a combo model", () => {
    const noModel: ComboRow = { name: "x", members: [{ provider: "a", model: "m-a" }, { provider: "b", model: "m-b" }] };
    expect(parseDraft(draftOf(noModel))).toEqual(noModel);
  });
  it("combo type: chat default, decision persisted, chat never emitted", () => {
    const decision: ComboRow = { name: "j", type: "decision", members: [{ provider: "t", model: "jev-latest" }] };
    expect(draftOf(decision).type).toBe("decision");
    expect(parseDraft(draftOf(decision))).toEqual(decision);
    // legacy combos (no type) edit as chat and save without a type field
    expect(draftOf(row).type).toBe("chat");
    expect(parseDraft(draftOf(row)).type).toBeUndefined();
  });
  it("providersForType: decision → classifier wires only, chat → the rest", () => {
    const provs = [
      { name: "t", wire: "classifier", disabled: false },
      { name: "a", wire: "openai", disabled: false },
      { name: "off", wire: "classifier", disabled: true },
    ] as unknown as ProviderRow[];
    expect(providersForType(provs, "decision").map((p) => p.name)).toEqual(["t"]);
    expect(providersForType(provs, "chat").map((p) => p.name)).toEqual(["a"]);
  });
  it("memberModelsFor filters to classifier models on decision combos", () => {
    const provs = [{ name: "t", wire: "classifier", models: ["jev-latest", "jev-1.13.0"] }] as unknown as ProviderRow[];
    const cat: CatalogModel[] = [
      { id: "jev-latest", family: "classifier", input: 0, output: 0, cache_read: 0, cache_write: 0 } as CatalogModel,
      { id: "jev-1.13.0", family: "chat", input: 0, output: 0, cache_read: 0, cache_write: 0 } as CatalogModel,
    ];
    expect(memberModelsFor(provs, "t", "decision", cat)).toEqual(["jev-latest"]);
    expect(memberModelsFor(provs, "t", "chat", cat)).toEqual(["jev-latest", "jev-1.13.0"]);
  });
  it("option label carries context + prices from the catalog", () => {
    const catalog: CatalogModel[] = [
      { id: "m1", context: 128000, input: 0.3, output: 1.2, cache_read: 0, cache_write: 0 } as CatalogModel,
      { id: "m2", context: 0, input: -1, output: -1, cache_read: 0, cache_write: 0 } as CatalogModel,
    ];
    expect(modelOptionLabel("m1", catalog)).toBe("m1 · 128K · $0.3/$1.2");
    expect(modelOptionLabel("m2", catalog)).toBe("m2 · — · —");
    expect(modelOptionLabel("m3", catalog)).toBe("m3"); // not in catalog
  });
  it("meta line describes ctx + prices, unknown parts dropped", () => {
    const catalog: CatalogModel[] = [
      { id: "m1", context: 1000000, input: 0.075, output: 0.25, cache_read: 0, cache_write: 0 } as CatalogModel,
      { id: "m2", input: -1, output: -1, cache_read: 0, cache_write: 0 } as CatalogModel,
    ];
    expect(modelMetaLine("m1", catalog)).toBe("1M ctx · $0.075 in / $0.25 out");
    expect(modelMetaLine("m2", catalog)).toBe("");
    expect(modelMetaLine("m3", catalog)).toBe("");
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
