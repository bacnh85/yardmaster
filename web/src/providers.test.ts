import { describe, expect, it } from "vitest";
import { EMPTY_FORM, providerBody, rowToForm, groupFor, connectedCount, connRows, labelWithCode, familyFor, serveTargetModels } from "./tabs/ProvidersTab";
import { REGISTRY, registryFor, entryFor, wireFamily, presetToForm } from "./presets";
import type { ProviderRow } from "./api";

describe("providerBody", () => {
  it("coerces dispatch spacing to a number (type=number inputs yield strings)", () => {
    const form = { ...EMPTY_FORM, dispatch_interval_ms: "250" as unknown as number };
    const body = providerBody(form);
    expect(body.dispatch_interval_ms).toBe(250);
  });
  it("treats blank dispatch as 0", () => {
    const body = providerBody({ ...EMPTY_FORM, dispatch_interval_ms: "" as unknown as number });
    expect(body.dispatch_interval_ms).toBe(0);
  });
  it("omits blank keys so the server keeps existing ones on edit", () => {
    const body = providerBody({ ...EMPTY_FORM, keys: "  \n " });
    expect(body.keys).toBeUndefined();
  });
  it("splits models on commas and drops blanks", () => {
    expect(providerBody({ ...EMPTY_FORM, models: " a , b,, " }).models).toEqual(["a", "b"]);
  });
  it("parses one key per line", () => {
    expect(providerBody({ ...EMPTY_FORM, keys: "k1\nk2\n\n k3 " }).keys).toEqual(["k1", "k2", "k3"]);
  });
  it("passes the session field through", () => {
    expect(providerBody({ ...EMPTY_FORM, session: "opencode" }).session).toBe("opencode");
    expect(providerBody(EMPTY_FORM).session).toBe("");
  });
  it("round-trips session on edit (Go PUT: omitted = keep, \"\" = clear, so the form must send the prefilled value)", () => {
    const editForm = { ...EMPTY_FORM, name: "opencode-go", session: "opencode" };
    expect(providerBody(editForm).session).toBe("opencode");
    expect(providerBody({ ...editForm, session: "" }).session).toBe("");
  });
  it("lowercases and trims the routing prefix", () => {
    expect(providerBody({ ...EMPTY_FORM, prefix: "  OCG " }).prefix).toBe("ocg");
  });
  it("round-trips prefix on edit; servers without prefix degrade to empty (PUT keeps stored)", () => {
    const p: Partial<ProviderRow> = { prefix: "ocg" };
    expect(providerBody(rowToForm({ ...baseRow, ...p })).prefix).toBe("ocg");
    expect(providerBody(rowToForm(baseRow)).prefix).toBe("");
  });
});

const baseRow: ProviderRow = {
  name: "x", wire: "openai", base_url: "https://x", models: [], session: "",
  preset: "", disabled: false, dispatch_interval_ms: 0, auth_type: "static",
  adaptive_thinking: false, inject_cache_control: false, connections: [], accounts: [],
};

describe("prefix registry", () => {
  it("every registry provider carries a short routing prefix", () => {
    expect(REGISTRY.map((r) => [r.id, r.prefix])).toEqual([
      ["opencode-go", "ocg"], ["deepseek", "ds"], ["zai", "zai"], ["cmdcode", "cmd"],
    ]);
    expect(REGISTRY.map((r) => [r.id, r.code])).toEqual([
      ["opencode-go", "OCG"], ["deepseek", "DS"], ["zai", "ZAI"], ["cmdcode", "CC"],
    ]);
  });
});

describe("connection labels", () => {
  it("prepends the provider code to user labels", () => {
    expect(labelWithCode("OCG", " mail@bacnh.com ")).toBe("OCG mail@bacnh.com");
  });
  it("is idempotent for already-prefixed labels", () => {
    expect(labelWithCode("OCG", "OCG mail@bacnh.com")).toBe("OCG mail@bacnh.com");
  });
  it("passes blank labels through", () => {
    expect(labelWithCode("OCG", "  ")).toBe("");
  });
});

describe("registry", () => {
  const row = (over: Partial<ProviderRow>): ProviderRow => ({
    name: "x", wire: "openai", base_url: "https://x", models: [], session: "",
    preset: "", disabled: false, dispatch_interval_ms: 0, auth_type: "static",
    adaptive_thinking: false, inject_cache_control: false, connections: [], accounts: [],
    ...over,
  });

  it("has the providers the product must support", () => {
    expect(REGISTRY.map((r) => r.id).sort()).toEqual(["cmdcode", "deepseek", "opencode-go", "zai"]);
    const zen = REGISTRY.find((r) => r.id === "opencode-go")!;
    expect(zen.entries).toHaveLength(3); // one config provider per wire family
    for (const e of zen.entries) {
      expect(e.base_url).toBe("https://opencode.ai/zen/go/v1");
      expect(e.session).toBe("opencode");
    }
    expect(zen.entries.map((e) => e.family).sort()).toEqual(["anthropic", "chat", "responses"]);
  });

  it("groups config providers by preset, falling back to legacy names", () => {
    const zen = REGISTRY.find((r) => r.id === "opencode-go")!;
    const provs = [
      row({ name: "opencode-go", preset: "opencode-go", connections: [{ label: "OCG", suffix: "…aaa" }] }),
      row({ name: "opencode-go-claude", preset: "opencode-go", wire: "anthropic", connections: [{ label: "OCG", suffix: "…aaa" }] }),
      row({ name: "deepseek", preset: "" }), // legacy: matched by name
      row({ name: "other", preset: "" }),
    ];
    const zenGroup = groupFor(zen, provs);
    expect(zenGroup.map((p) => p.name)).toEqual(["opencode-go", "opencode-go-claude"]);
    expect(connectedCount(zenGroup)).toBe(2);
    expect(groupFor(REGISTRY.find((r) => r.id === "deepseek")!, provs).map((p) => p.name)).toEqual(["deepseek"]);
    expect(registryFor(row({ name: "other", preset: "" }))).toBeUndefined();
    // unknown preset id falls back to name matching (legacy/renamed configs)
    expect(registryFor(row({ name: "zai", preset: "removed-id" }))?.id).toBe("zai");
  });

  it("resolves a config provider's registry entry (wire disambiguates groups)", () => {
    const zen = REGISTRY.find((r) => r.id === "opencode-go")!;
    expect(entryFor(zen, { preset: "opencode-go", name: "opencode-go-claude", wire: "anthropic" })?.family).toBe("anthropic");
    expect(entryFor(zen, { preset: "", name: "opencode-go-gpt", wire: "responses" })?.family).toBe("responses");
  });

  it("folds one key across wire entries into a single connection row", () => {
    const zen = REGISTRY.find((r) => r.id === "opencode-go")!;
    const provs = [
      row({ name: "opencode-go", preset: "opencode-go", connections: [{ label: "OCG a@b.c", suffix: "…aaa" }] }),
      row({ name: "opencode-go-claude", preset: "opencode-go", wire: "anthropic", connections: [{ label: "OCG a@b.c", suffix: "…aaa" }] }),
      row({ name: "opencode-go-gpt", preset: "opencode-go", wire: "responses", connections: [{ label: "", suffix: "…aaa" }] }),
    ];
    const rows = connRows(groupFor(zen, provs));
    expect(rows).toHaveLength(1);
    expect(rows[0].suffix).toBe("…aaa");
    expect(rows[0].label).toBe("OCG a@b.c"); // first non-empty label wins
    expect(rows[0].targets).toHaveLength(3);
  });

  it("keeps distinct keys in separate connection rows", () => {
    const zen = REGISTRY.find((r) => r.id === "opencode-go")!;
    const provs = [
      row({ name: "opencode-go", preset: "opencode-go", connections: [{ label: "A", suffix: "…1" }, { label: "B", suffix: "…2" }] }),
      row({ name: "opencode-go-claude", preset: "opencode-go", wire: "anthropic", connections: [] }),
    ];
    expect(connRows(groupFor(zen, provs)).map((r) => r.suffix)).toEqual(["…1", "…2"]);
  });

  it("never merges same-provider duplicate suffixes (distinct keys, index-addressed delete)", () => {
    const zen = REGISTRY.find((r) => r.id === "opencode-go")!;
    const provs = [
      row({ name: "opencode-go", preset: "opencode-go", connections: [{ label: "A", suffix: "…aaa" }, { label: "B", suffix: "…aaa" }] }),
      row({ name: "opencode-go-claude", preset: "opencode-go", wire: "anthropic", connections: [{ label: "C", suffix: "…aaa" }] }),
    ];
    const rows = connRows(groupFor(zen, provs));
    expect(rows).toHaveLength(2);
    // rendered React keys must stay unique across rows sharing a suffix
    const keys = rows.map((r) => r.targets.map((t) => `${t.p.name}:${t.idx}`).join("|"));
    expect(new Set(keys).size).toBe(keys.length);
    expect(rows[0].targets.map((t) => t.p.name)).toEqual(["opencode-go", "opencode-go-claude"]);
    expect(rows[1].targets.map((t) => t.p.name)).toEqual(["opencode-go"]);
  });

  it("derives a curated model's family from its wire, not from missing metadata", () => {    const zen = REGISTRY.find((r) => r.id === "opencode-go")!;
    const provs = [
      row({ name: "opencode-go", preset: "opencode-go", models: ["deepseek-v4.1-flash"] }),
      row({ name: "opencode-go-gpt", preset: "opencode-go", wire: "responses", models: ["muse-spark-1.3-contributor"] }),
    ];
    const g = groupFor(zen, provs);
    expect(familyFor(g, { id: "muse-spark-1.3-contributor", family: "" })).toBe("responses"); // models.dev knows nothing; curation wins
    expect(familyFor(g, { id: "deepseek-flash", family: "" })).toBe(""); // uncurated: metadata only
    expect(familyFor(g, { id: "deepseek-v4.1-flash", family: "chat" })).toBe("chat");
  });

  it("serveTargetModels: appending to a wildcard (empty-models) entry requires confirmation", () => {
    // the guard: without it, serving one unknown model on a serve-all entry
    // silently un-serves every other model on that wire
    expect(serveTargetModels({ models: [] }, "omen-alpha")).toEqual({ models: ["omen-alpha"], confirm: true });
    expect(serveTargetModels({ models: ["a", "b"] }, "c")).toEqual({ models: ["a", "b", "c"], confirm: false });
  });

  it("maps wires to catalog families", () => {
    expect(wireFamily("openai")).toBe("chat");
    expect(wireFamily("anthropic")).toBe("anthropic");
    expect(wireFamily("responses")).toBe("responses");
    expect(presetToForm(REGISTRY[0].entries[0]).session).toBe("opencode");
  });
});

describe("rowToForm (edit prefill)", () => {
  it("prefills the edit form from a provider row — session round-trips", () => {
    // a row with session:opencode edited + saved must PUT "opencode" back,
    // not "" (which the Go server treats as an intentional clear)
    const prow = rowToForm({
      name: "opencode-go", wire: "openai", base_url: "https://opencode.ai/zen/v1",
      models: ["big-pickle", "glm-5.3"], session: "opencode",
      preset: "opencode-go", disabled: false, key_suffixes: [],
      dispatch_interval_ms: 0, auth_type: "static",
      adaptive_thinking: false, inject_cache_control: false,
      accounts: [], connections: [{ label: "OCG", suffix: "…aaaa" }],
    } as unknown as ProviderRow);
    expect(prow.session).toBe("opencode");
    expect(providerBody(prow).session).toBe("opencode");
    expect(prow.models).toBe("big-pickle, glm-5.3");
    expect(prow.keys).toBe(""); // blank keys = server keeps existing on edit
  });
});
