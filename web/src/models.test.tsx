import { describe, expect, it } from "vitest";
import { toRow, sortPrice, filterRows, DEFAULT_FILTERS, type CatalogRowUI } from "./tabs/ModelsTab";
import { fmtPrice } from "./api";
import { Playground, type PgTarget } from "./Playground";
import { renderToString } from "react-dom/server";

const row = (over: Partial<CatalogRowUI>): CatalogRowUI => ({
  id: "m", name: "", family: "chat",
  context: 0, max_output: 0,
  input: -1, output: -1, cache_read: 0,
  reasoning: false, tool_call: false, image: false, free: false, manual: false,
  providers: [{ name: "p1", prefix: "", exposed: true }],
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
      providers: [{ name: "zai", prefix: "zai", exposed: true }] }),
    row({ id: "gpt-5.6", input: 1.25, output: 10, image: true,
      providers: [{ name: "or", prefix: "or", exposed: true }, { name: "cc", prefix: "", exposed: false }] }),
    row({ id: "tiny-free", free: true, input: 0, output: 0,
      providers: [{ name: "or", prefix: "or", exposed: true }] }),
  ];

  it("matches text against id and name, case-insensitive", () => {
    expect(filterRows(rows, { ...DEFAULT_FILTERS, text: "glm" }).map((r) => r.id)).toEqual(["glm-5.3"]);
    expect(filterRows(rows, { ...DEFAULT_FILTERS, text: "gpt" }).map((r) => r.id)).toEqual(["gpt-5.6"]);
    expect(filterRows(rows, { ...DEFAULT_FILTERS, text: "5.3" }).map((r) => r.id)).toEqual(["glm-5.3"]);
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
    const hidden = row({ id: "ghost", providers: [{ name: "p", prefix: "", exposed: false }] });
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
      { name: "opencode-go", prefix: "ocg", exposed: true },
      { name: "ollama", prefix: "ol", exposed: false },
    ] });
    const byOllama = filterRows([kimi], { ...DEFAULT_FILTERS, provider: "ollama" });
    expect(byOllama).toEqual([]); // not exposed there → must not appear
    // exposed entries survive the filter; the unexposed one is trimmed from the row
    const kept = filterRows([kimi], { ...DEFAULT_FILTERS, provider: "opencode-go" });
    expect(kept).toHaveLength(1);
    expect(kept[0].providers).toEqual([{ name: "opencode-go", prefix: "ocg", exposed: true }]);
    // same trimming applies with provider=all (badges never show catalog-only entries)
    const trimmed = filterRows([kimi], DEFAULT_FILTERS)[0];
    expect(trimmed.providers).toEqual([{ name: "opencode-go", prefix: "ocg", exposed: true }]);
    // exposedOnly off → the catalog-only entry is visible again (badge shows muted)
    const all = filterRows([kimi], { ...DEFAULT_FILTERS, provider: "ollama", exposedOnly: false });
    expect(all).toHaveLength(1);
    expect(all[0].providers).toContainEqual({ name: "ollama", prefix: "ol", exposed: false });
  });
});

describe("toRow", () => {
  it("defaults absent metadata (older servers) to the unknown conventions", () => {
    const r = toRow({ id: "x", family: "chat", input: -1, output: -1, cache_read: 0, cache_write: 0 });
    expect(r.context).toBe(0);
    expect(r.providers).toEqual([]);
  });
  it("carries provider entries with prefix default", () => {
    const r = toRow({ id: "x", family: "chat", input: 0, output: 0, cache_read: 0, cache_write: 0,
      providers: [{ name: "zai", exposed: true }] });
    expect(r.providers).toEqual([{ name: "zai", prefix: "", exposed: true }]);
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
