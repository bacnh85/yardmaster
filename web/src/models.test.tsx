import { describe, expect, it } from "vitest";
import { toRow, sortPrice, filterRows, servedIds, priceSpread, displayName, DEFAULT_FILTERS, type CatalogRowUI } from "./tabs/ModelsTab";
import { fmtPrice } from "./api";
import { Playground, type PgTarget } from "./Playground";
import { renderToString } from "react-dom/server";

const row = (over: Partial<CatalogRowUI>): CatalogRowUI => ({
  id: "m", name: "", family: "chat",
  context: 0, max_output: 0,
  input: -1, output: -1, cache_read: 0,
  reasoning: false, tool_call: false, image: false, free: false, manual: false,
  providers: [{ name: "p1", prefix: "", exposed: true, served_as: "m", input: -1, output: -1, cache_read: -1 }],
  combos: [],
  ...over,
});

describe("sortPrice", () => {
  it("maps -1 unknown pricing to null (sorts last), keeps 0 = free", () => {
    expect(sortPrice(-1)).toBeNull();
    expect(sortPrice(0)).toBe(0);
    expect(sortPrice(3.5)).toBe(3.5);
  });
});

describe("fmtPrice", () => {
  it("rounds float noise to 3 decimals and trims trailing zeros", () => {
    expect(fmtPrice(0.09999999999999999)).toBe("$0.1");
    expect(fmtPrice(10.534600000000001)).toBe("$10.535");
    expect(fmtPrice(0.24900000000000003)).toBe("$0.249");
    expect(fmtPrice(0.035)).toBe("$0.035");
    expect(fmtPrice(10)).toBe("$10");
  });
  it("keeps the unknown/free conventions", () => {
    expect(fmtPrice(-1)).toBe("—");
    expect(fmtPrice(0)).toBe("free");
  });
});

describe("filterRows", () => {
  const rows: CatalogRowUI[] = [
    row({ id: "glm-5.3", name: "GLM-5.3", input: 0.6, output: 2, reasoning: true, tool_call: true,
      providers: [{ name: "zai", prefix: "zai", exposed: true, served_as: "zai/glm-5.3", input: 0.6, output: 2, cache_read: 0.1 }],
      combos: ["combo/fast"] }),
    row({ id: "gpt-5.6", input: 1.25, output: 10, image: true,
      providers: [{ name: "or", prefix: "or", exposed: true, served_as: "or/X", input: 1.25, output: 10, cache_read: 0.2 }, { name: "cc", prefix: "", exposed: false, served_as: "cc-secret", input: -1, output: -1, cache_read: -1 }] }),
    row({ id: "tiny-free", free: true, input: 0, output: 0,
      providers: [{ name: "or", prefix: "or", exposed: true, served_as: "or/X", input: 1.25, output: 10, cache_read: 0.2 }] }),
  ];

  it("matches text against id and name, case-insensitive", () => {
    expect(filterRows(rows, { ...DEFAULT_FILTERS, text: "glm" }).map((r) => r.id)).toEqual(["glm-5.3"]);
    expect(filterRows(rows, { ...DEFAULT_FILTERS, text: "gpt" }).map((r) => r.id)).toEqual(["gpt-5.6"]);
    expect(filterRows(rows, { ...DEFAULT_FILTERS, text: "5.3" }).map((r) => r.id)).toEqual(["glm-5.3"]);
  });

  it("matches text against served ids (prefix/model, combo ids)", () => {
    expect(filterRows(rows, { ...DEFAULT_FILTERS, text: "zai/" }).map((r) => r.id)).toEqual(["glm-5.3"]);
    expect(filterRows(rows, { ...DEFAULT_FILTERS, text: "combo/fast" }).map((r) => r.id)).toEqual(["glm-5.3"]);
    // unexposed entries' served ids don't match at all — servedIds lists only
    // routable (exposed) ids, whatever exposedOnly is set to
    expect(filterRows(rows, { ...DEFAULT_FILTERS, text: "cc-secret" })).toEqual([]);
    expect(filterRows(rows, { ...DEFAULT_FILTERS, text: "cc-secret", exposedOnly: false })).toEqual([]);
  });

  it("servedIds unions exposed served_as + combos, deduped", () => {
    const m = row({ id: "k", providers: [
      { name: "a", prefix: "a", exposed: true, served_as: "a/k", input: 1, output: 2, cache_read: 0 },
      { name: "b", prefix: "", exposed: false, served_as: "k", input: -1, output: -1, cache_read: -1 },
    ], combos: ["combo/fast", "combo/fast"] });
    expect(servedIds(m)).toEqual(["a/k", "combo/fast"]);
  });

  it("displayName strips the vendor namespace for display only", () => {
    expect(displayName("deepseek/deepseek-v4-flash")).toBe("deepseek-v4-flash");
    expect(displayName("google/gemini-3.7-flash")).toBe("gemini-3.7-flash");
    expect(displayName("Qwen/Qwen3.8-Omni-Flash")).toBe("Qwen3.8-Omni-Flash");
    expect(displayName("glm-5.3")).toBe("glm-5.3"); // bare id untouched
  });

  it("priceSpread: null for single/price-identical providers, [min,max] across exposed only", () => {
    expect(priceSpread(row({ id: "solo" }))).toBeNull(); // one provider
    const same = row({ id: "same", providers: [
      { name: "a", prefix: "", exposed: true, served_as: "same", input: 1, output: 2, cache_read: 0.1 },
      { name: "b", prefix: "", exposed: true, served_as: "same", input: 1, output: 2, cache_read: 0.1 },
    ] });
    expect(priceSpread(same)).toBeNull(); // equal prices → nothing to expand
    const diff = row({ id: "diff", input: 0.13, providers: [
      { name: "cmdcode", prefix: "cmd", exposed: true, served_as: "cmd/q", input: 0.13, output: 0.43, cache_read: 0.016 },
      { name: "nvidia", prefix: "nv", exposed: true, served_as: "nv/q", input: 0, output: 0, cache_read: 0 },
    ] });
    expect(priceSpread(diff)).toEqual([0, 0.13]); // input class: free vs paid
    // unexposed provider's price never widens the spread
    const hiddenDiff = row({ id: "hd", providers: [
      { name: "a", prefix: "", exposed: true, served_as: "hd", input: 1, output: 2, cache_read: 0 },
      { name: "b", prefix: "", exposed: false, served_as: "hd", input: 99, output: 99, cache_read: 99 },
    ] });
    expect(priceSpread(hiddenDiff)).toBeNull();
  });

  it("filters by provider config name (unexposed entries only match with exposedOnly off)", () => {
    // gpt-5.6's cc entry is catalog-only (exposed: false) → no match by default
    expect(filterRows(rows, { ...DEFAULT_FILTERS, provider: "cc" })).toEqual([]);
    expect(filterRows(rows, { ...DEFAULT_FILTERS, provider: "or" }).map((r) => r.id)).toEqual(["gpt-5.6", "tiny-free"]);
    expect(filterRows(rows, { ...DEFAULT_FILTERS, provider: "cc", exposedOnly: false }).map((r) => r.id)).toEqual(["gpt-5.6"]);
  });

  it("price filter: free = free flag + 0 price, paid = any non-negative price", () => {
    expect(filterRows(rows, { ...DEFAULT_FILTERS, price: "free" }).map((r) => r.id)).toEqual(["tiny-free"]);
    expect(filterRows(rows, { ...DEFAULT_FILTERS, price: "paid" }).map((r) => r.id)).toEqual(["glm-5.3", "gpt-5.6"]);
  });

  it("capability chips AND together", () => {
    expect(filterRows(rows, { ...DEFAULT_FILTERS, reasoning: true }).map((r) => r.id)).toEqual(["glm-5.3"]);
    expect(filterRows(rows, { ...DEFAULT_FILTERS, reasoning: true, tool_call: false, image: true })).toEqual([]);
  });

  it("exposedOnly drops rows no provider exposes (default on)", () => {
    const hidden = row({ id: "ghost", providers: [{ name: "p", prefix: "", exposed: false, served_as: "ghost", input: -1, output: -1, cache_read: -1 }] });
    expect(filterRows([...rows, hidden], DEFAULT_FILTERS).some((r) => r.id === "ghost")).toBe(false);
    expect(filterRows([...rows, hidden], { ...DEFAULT_FILTERS, exposedOnly: false }).some((r) => r.id === "ghost")).toBe(true);
  });

  it("keeps unknown-price rows only under price=all (— rows never count as free or paid)", () => {
    const unknown = row({ id: "unknown" });
    expect(filterRows([unknown], { ...DEFAULT_FILTERS, price: "free" })).toEqual([]);
    expect(filterRows([unknown], { ...DEFAULT_FILTERS, price: "paid" })).toEqual([]);
  });

  it("exposedOnly + provider filter: a model exposed elsewhere must not list under a provider where it's catalog-only", () => {
    // kimi-k3 lives in opencode-go's models list but is ONLY in ollama's catalog
    const kimi = row({ id: "kimi-k3", providers: [
      { name: "opencode-go", prefix: "ocg", exposed: true, served_as: "ocg/kimi-k3", input: 3, output: 15, cache_read: 0.3 },
      { name: "ollama", prefix: "ol", exposed: false, served_as: "ol/kimi-k3", input: 0.1, output: 3, cache_read: 0 },
    ] });
    const byOllama = filterRows([kimi], { ...DEFAULT_FILTERS, provider: "ollama" });
    expect(byOllama).toEqual([]); // not exposed there → must not appear
    // exposed entries survive the filter; the unexposed one is trimmed from the row
    const kept = filterRows([kimi], { ...DEFAULT_FILTERS, provider: "opencode-go" });
    expect(kept).toHaveLength(1);
    expect(kept[0].providers).toEqual([{ name: "opencode-go", prefix: "ocg", exposed: true, served_as: "ocg/kimi-k3", input: 3, output: 15, cache_read: 0.3 }]);
    // same trimming applies with provider=all (badges never show catalog-only entries)
    const trimmed = filterRows([kimi], DEFAULT_FILTERS)[0];
    expect(trimmed.providers).toEqual([{ name: "opencode-go", prefix: "ocg", exposed: true, served_as: "ocg/kimi-k3", input: 3, output: 15, cache_read: 0.3 }]);
    // exposedOnly off → the catalog-only entry is visible again (badge shows muted)
    const all = filterRows([kimi], { ...DEFAULT_FILTERS, provider: "ollama", exposedOnly: false });
    expect(all).toHaveLength(1);
    expect(all[0].providers).toContainEqual({ name: "ollama", prefix: "ol", exposed: false, served_as: "ol/kimi-k3", input: 0.1, output: 3, cache_read: 0 });
  });
});

describe("toRow", () => {
  it("defaults absent metadata (older servers) to the unknown conventions", () => {
    const r = toRow({ id: "x", family: "chat", input: -1, output: -1, cache_read: 0, cache_write: 0 });
    expect(r.context).toBe(0);
    expect(r.providers).toEqual([]);
  });
  it("carries provider entries with prefix + served_as defaults", () => {
    const r = toRow({ id: "x", family: "chat", input: 0, output: 0, cache_read: 0, cache_write: 0,
      providers: [{ name: "zai", wire: "openai", exposed: true, served_as: "zai/x", input: 2, output: 8, cache_read: 0.2 }] });
    expect(r.providers).toEqual([{ name: "zai", prefix: "", exposed: true, served_as: "zai/x", input: 2, output: 8, cache_read: 0.2 }]);
  });

  it("defaults absent combos to empty", () => {
    const r = toRow({ id: "x", family: "chat", input: -1, output: -1, cache_read: 0, cache_write: 0 });
    expect(r.combos).toEqual([]);
    const c = toRow({ id: "x", family: "chat", input: -1, output: -1, cache_read: 0, cache_write: 0, combos: ["combo/fast"] });
    expect(c.combos).toEqual(["combo/fast"]);
  });
});

describe("Playground thread mapping", () => {
  const target = (models: string[]): PgTarget => ({ provider: "prov", models, connections: [{ label: "k1", suffix: "ab12cd" }] });

  it("renders the empty-state hint before the first send", () => {
    const html = renderToString(<Playground targets={[target(["m1"])]} model="m1" onModel={() => {}} />);
    expect(html).toContain("Send a message to start the conversation");
  });

  it("renders pickers for model + key with the target's models", () => {
    const html = renderToString(<Playground targets={[target(["m1", "m2"])]} model="m2" onModel={() => {}} />);
    expect(html).toContain('id="pg-model"');
    expect(html).toContain('id="pg-key"');
    expect(html).toContain(">m2<");
  });

  it("renders the no-targets empty state when the provider exposes nothing", () => {
    const html = renderToString(<Playground targets={[]} model="" onModel={() => {}} />);
    expect(html).toContain("no exposed models");
  });
});
