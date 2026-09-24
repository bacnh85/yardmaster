import { describe, expect, it } from "vitest";
import { EMPTY_FORM, providerBody, providerUpdateBody, rowToForm, groupFor, connectedCount, connRows, labelWithCode, familyFor, serveTargetModels, bulkNextModels, effectiveCap, capIds, withCurated, toggleModels, isCuratedModel, canonModelId, connSeedPlan, planTargetsToWrite } from "./tabs/ProvidersTab";
import { REGISTRY, registryFor, entryFor, wireFamily, presetToForm, cmdPlan, olPlan, planLadder, normPlan } from "./presets";
import type { CatalogModel, ProviderRow } from "./api";
import type { ConnRow } from "./tabs/ProvidersTab";

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
  it("round-trips zcode signing + header/body overrides on edit", () => {
    const row: ProviderRow = { ...baseRow, wire: "anthropic", zcode_signing: true,
      extra_headers: { "anthropic-beta": "fast-mode-2026-02-01" }, body_overrides: { speed: "fast" } };
    const f = rowToForm(row);
    expect(f.zcode_signing).toBe(true);
    const body = providerBody(f);
    expect(body.zcode_signing).toBe(true);
    expect(body.extra_headers).toEqual({ "anthropic-beta": "fast-mode-2026-02-01" });
    expect(body.body_overrides).toEqual({ speed: "fast" });
    // blank JSON fields are omitted → server keeps stored
    const blank = providerBody(rowToForm(baseRow));
    expect(blank.zcode_signing).toBe(false);
    expect(blank.extra_headers).toBeUndefined();
    expect(blank.body_overrides).toBeUndefined();
  });
  it("round-trips subscription on add/edit (Go PUT: omitted = keep, \"\" = clear)", () => {
    expect(providerBody({ ...EMPTY_FORM, subscription: "goat" }).subscription).toBe("goat");
    expect(providerBody({ ...EMPTY_FORM, subscription: "" }).subscription).toBe("");
    const prow = rowToForm({ ...baseRow, subscription: "pro" });
    expect(prow.subscription).toBe("pro");
    expect(providerBody(prow).subscription).toBe("pro");
    // old servers without the field degrade to "" (PUT clears the tier, harmless)
    expect(providerBody(rowToForm(baseRow)).subscription).toBe("");
  });
  it("providerUpdateBody never drops the stored subscription (row actions)", () => {
    const body = providerUpdateBody({ ...baseRow, subscription: "max" }, ["m"]);
    expect(body.subscription).toBe("max");
    expect(providerUpdateBody(baseRow, ["m"]).subscription).toBe("");
  });
  it("rejects non-object JSON in override fields (save() surfaces the error)", () => {
    expect(() => providerBody({ ...EMPTY_FORM, extra_headers: "[1,2]" })).toThrow();
    expect(() => providerBody({ ...EMPTY_FORM, body_overrides: "not json" })).toThrow();
  });
});

describe("providerUpdateBody (row actions)", () => {
  it("round-trips every advanced field incl. zcode_signing — row actions must never drop the tricks", () => {
    const row: ProviderRow = { ...baseRow, wire: "anthropic", zcode_signing: true,
      adaptive_thinking: true, inject_cache_control: true, dispatch_interval_ms: 1000,
      extra_headers: { "anthropic-beta": "fast-mode-2026-02-01" }, body_overrides: { speed: "fast" } };
    const body = providerUpdateBody(row, ["glm-5.3"]);
    expect(body.zcode_signing).toBe(true);
    expect(body.adaptive_thinking).toBe(true);
    expect(body.inject_cache_control).toBe(true);
    expect(body.dispatch_interval_ms).toBe(1000);
    expect(body.extra_headers).toEqual({ "anthropic-beta": "fast-mode-2026-02-01" });
    expect(body.body_overrides).toEqual({ speed: "fast" });
    expect(body.models).toEqual(["glm-5.3"]);
  });
});

describe("zai preset defaults", () => {
  it("the zai entry ships the coding-plan quota levers as create defaults", () => {
    const zai = REGISTRY.find((r) => r.id === "zai");
    expect(zai?.entries).toHaveLength(1);
    const d = zai?.entries[0].defaults as Record<string, unknown>;
    expect(d.models).toEqual(["glm-5.3", "glm-5.3-flash"]);
    expect(d.dispatch_interval_ms).toBe(1000);
    expect(d.adaptive_thinking).toBe(true);
    expect(d.inject_cache_control).toBe(true);
    expect(d.zcode_signing).toBe(true);
    expect(d.extra_headers).toEqual({ "anthropic-beta": "fast-mode-2026-02-01" });
    expect(d.body_overrides).toEqual({ speed: "fast" });
  });
  it("only zai carries defaults — other presets create bare providers", () => {
    for (const r of REGISTRY.filter((r) => r.id !== "zai")) {
      for (const e of r.entries) {
        expect(e.defaults).toBeUndefined();
      }
    }
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
      ["opencode-go", "ocg"], ["deepseek", "ds"], ["zai", "zai"], ["cmdcode", "cmd"], ["openrouter", "or"], ["ollama", "ol"],
    ]);
    expect(REGISTRY.map((r) => [r.id, r.code])).toEqual([
      ["opencode-go", "OCG"], ["deepseek", "DS"], ["zai", "ZAI"], ["cmdcode", "CC"], ["openrouter", "OR"], ["ollama", "OL"],
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
    expect(REGISTRY.map((r) => r.id).sort()).toEqual(["cmdcode", "deepseek", "ollama", "opencode-go", "openrouter", "zai"]);
    const zen = REGISTRY.find((r) => r.id === "opencode-go")!;
    expect(zen.entries).toHaveLength(3); // one config provider per wire family
    for (const e of zen.entries) {
      expect(e.base_url).toBe("https://opencode.ai/zen/go/v1");
      expect(e.session).toBe("opencode");
    }
    expect(zen.entries.map((e) => e.family).sort()).toEqual(["anthropic", "chat", "responses"]);
    // CommandCode: Claude models serve /messages only → dedicated anthropic entry
    const cc = REGISTRY.find((r) => r.id === "cmdcode")!;
    expect(cc.plans).toBe(true);
    expect(cc.entries).toHaveLength(2);
    expect(new Set(cc.entries.map((e) => e.base_url)).size).toBe(1);
    expect(cc.entries.map((e) => e.family).sort()).toEqual(["anthropic", "chat"]);
    // DeepSeek serves all three wires from one API key (docs 2026-09-21):
    // /chat/completions, /anthropic/v1/messages, /responses — one entry per family
    const ds = REGISTRY.find((r) => r.id === "deepseek")!;
    expect(ds.entries).toHaveLength(3);
    expect(ds.entries.map((e) => e.family).sort()).toEqual(["anthropic", "chat", "responses"]);
    expect(new Set(ds.entries.map((e) => e.base_url)).size).toBe(2); // /anthropic base differs
    expect(ds.entries.find((e) => e.wire === "anthropic")!.base_url).toBe("https://api.deepseek.com/anthropic");
    // Ollama Cloud: single openai-wire entry (chat hub translates for Claude/Codex
    // clients); /v1/models is public, catalog metadata comes from models.dev ollama-cloud
    const ol = REGISTRY.find((r) => r.id === "ollama")!;
    expect(ol.plans).toBe(true);
    expect(ol.planOf?.("gpt-oss:20b")).toBe("free"); // Ollama's own lowest tier
    expect(ol.planOf?.("glm-5.3")).toBe("pro");
    expect(ol.entries).toHaveLength(1);
    expect(ol.entries[0]).toMatchObject({ name: "ollama", wire: "openai", base_url: "https://ollama.com/v1", family: "chat" });
  });

  it("classifies CommandCode models per plan (live catalog ids)", () => {
    // vendor-namespaced + mixed-case ids must match case-insensitively
    expect(cmdPlan("MiniMaxAI/MiniMax-M3")).toBe("goat");
    expect(cmdPlan("zai-org/GLM-5.3")).toBe("goat");
    expect(cmdPlan("gpt-5.6-luna")).toBe("goat");
    expect(cmdPlan("inclusionai/ling-3.0-flash-sante:free")).toBe("goat");
    expect(cmdPlan("claude-sonnet-5")).toBe("pro");
    expect(cmdPlan("google/gemini-3.6-flash")).toBe("pro");
    expect(cmdPlan("meta/muse-spark-1.1")).toBe("pro");
    expect(cmdPlan("claude-opus-5")).toBe("max");
    expect(cmdPlan("sakana/fugu-ultra")).toBe("max");
    expect(cmdPlan("brand-new-model")).toBe(""); // unclassified future id
  });

  it("classifies Ollama Cloud models per plan (live free-tier list)", () => {
    // the 6 free models → "free" (Ollama's own ladder: free < pro < max); everything else → "pro"
    expect(olPlan("gemma4:31b")).toBe("free");
    expect(olPlan("gpt-oss:20b")).toBe("free");
    expect(olPlan("gpt-oss:120b")).toBe("free");
    expect(olPlan("nemotron-3-nano:30b")).toBe("free");
    expect(olPlan("nemotron-3-super")).toBe("free");
    expect(olPlan("nemotron-3-ultra")).toBe("free");
    expect(olPlan("glm-5.3")).toBe("pro");
    expect(olPlan("deepseek-v4-pro:0813")).toBe("pro");
    expect(olPlan("kimi-k3")).toBe("pro");
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

  describe("bulkNextModels (bulk visibility toggle)", () => {
    it("show on a curated entry unions the shown ids", () => {
      expect(bulkNextModels(["a"], ["b", "c"], ["a", "b", "c", "d"], true)).toEqual(["a", "b", "c"]);
    });
    it("show on a wildcard stays wildcard (no-op)", () => {
      expect(bulkNextModels([], ["b", "c"], ["a", "b", "c"], true)).toEqual([]);
    });
    it("show never curates ids outside the entry's family/tier universe (cross-wire guard)", () => {
      // shownIds may span wire families on screen; only ids in familyIds land
      expect(bulkNextModels(["a"], ["b", "r1"], ["a", "b"], true)).toEqual(["a", "b"]);
    });
    it("hide on a curated entry removes only the shown ids", () => {
      expect(bulkNextModels(["a", "b", "c"], ["b"], ["a", "b", "c"], false)).toEqual(["a", "c"]);
    });
    it("hide on a wildcard materializes the family complement of the shown ids", () => {
      expect(bulkNextModels([], ["b"], ["a", "b", "c"], false)).toEqual(["a", "c"]);
    });
    it("hide on a wildcard materializes the UNcapped family complement (per-row semantics: hiding only narrows)", () => {
      // "hide these" on a wildcard = serve the family rest — incl. ids the cap
      // would forbid ADDING; a narrowing of the wildcard can't expose anything new
      expect(bulkNextModels([], ["a"], ["a", "b", "c"], false)).toEqual(["b", "c"]);
    });
    it("refuses (null) when hiding the LAST model of a curated entry (per-row single uncheck too)", () => {
      expect(bulkNextModels(["a"], ["a"], ["a"], false)).toBeNull();
    });
    it("refuses (null) when hiding would empty a CURATED entry — [] means wildcard=serve-everything", () => {
      expect(bulkNextModels(["a", "b"], ["a", "b"], ["a", "b"], false)).toBeNull();
    });
    it("refuses (null) when hiding would empty a WILDCARD entry", () => {
      expect(bulkNextModels([], ["a", "b"], ["a", "b"], false)).toBeNull();
    });
  });

  describe("effectiveCap + capIds (subscription guardrail)", () => {
    it("connSeedPlan/planTargetsToWrite: a label-only save never rewrites tiers (reviewer regression)", () => {
      const conn = (label: string, suffix: string) => ({ label, suffix });
      const heterogeneous: ConnRow = {
        suffix: "…abc123", label: "OL one",
        targets: [
          { p: { ...baseRow, name: "ollama", preset: "ollama", connections: [conn("OL one", "…abc123")] }, idx: 0 },
          { p: { ...baseRow, name: "ollama-claude", preset: "ollama", subscription: "pro", connections: [conn("OL one", "…abc123")] }, idx: 0 },
        ],
      };
      // display shows the group's tier (first non-empty), not targets[0]'s absence
      expect(connSeedPlan(heterogeneous)).toBe("pro");
      // label-only save (plan unchanged from seed) → no subscription writes at all
      expect(planTargetsToWrite(heterogeneous, "pro", "pro")).toEqual([]);
      // picking "none" in the select is an explicit clear of every tiered target
      expect(planTargetsToWrite(heterogeneous, "", "pro").map((p) => p.name)).toEqual(["ollama-claude"]);
      // changing the tier fans out to every target not already on it
      expect(planTargetsToWrite(heterogeneous, "max", "pro").map((p) => p.name)).toEqual(["ollama", "ollama-claude"]);
      // a target already on the chosen tier is skipped (no redundant PUT)
      expect(planTargetsToWrite(heterogeneous, "pro", "").map((p) => p.name)).toEqual(["ollama"]);
    });
    it("normPlan maps legacy cross-ladder subscriptions onto the preset ladder", () => {
      expect(normPlan("ollama", "goat")).toBe("free"); // legacy goat = tier-0 free
      expect(normPlan("ollama", "free")).toBe("free");
      expect(normPlan("ollama", "pro")).toBe("pro");
      expect(normPlan("ollama", "max")).toBe("max");
      expect(normPlan("ollama", "")).toBe("");
      expect(normPlan("ollama", "bogus")).toBe("");
      expect(normPlan("cmdcode", "free")).toBe("goat"); // reverse mapping
      expect(planLadder("cmdcode")).toEqual(["goat", "pro", "max"]);
      expect(planLadder("ollama")).toEqual(["free", "pro", "max"]);
      expect(planLadder("deepseek")).toEqual(["goat", "pro", "max"]); // default ladder
    });
    it("sweeps with a legacy stored goat sub cap to the ollama free tier (not zero models)", () => {
      const ids = ["gpt-oss:20b", "glm-5.3", "kimi-k3"];
      const cap = effectiveCap("all", normPlan("ollama", "goat"));
      expect(cap).toBe("free");
      expect(capIds(ids, cap, olPlan)).toEqual(["gpt-oss:20b"]);
    });
    it("stored subscription bounds the sweep — user filter can only narrow within it", () => {
      expect(effectiveCap("pro", "goat")).toBe("goat"); // filter above stored tier → capped down
      expect(effectiveCap("max", "pro")).toBe("pro");
      expect(effectiveCap("all", "goat")).toBe("goat"); // "all plans" still sweeps only the stored tier
      expect(effectiveCap("goat", "max")).toBe("goat"); // narrower choice allowed
      expect(effectiveCap("all", "")).toBe(""); // no subscription = uncapped
      expect(effectiveCap("pro", "")).toBe("pro");
    });
    it("capIds keeps only the capped tier's models (case-insensitive cmdPlan)", () => {
      const ids = ["claude-opus-5", "zai-org/GLM-5.3", "claude-sonnet-5"];
      expect(capIds(ids, "")).toEqual(ids);
      expect(capIds(ids, "goat")).toEqual(["zai-org/GLM-5.3"]);
      expect(capIds(ids, "max")).toEqual(["claude-opus-5"]);
    });
    it("capIds honors a preset-specific planOf (ollama free-tier guardrail)", () => {
      const ids = ["gpt-oss:20b", "glm-5.3", "kimi-k3"];
      expect(capIds(ids, "free", olPlan)).toEqual(["gpt-oss:20b"]); // free models only
      expect(capIds(ids, "pro", olPlan)).toEqual(["glm-5.3", "kimi-k3"]);
    });
  });

  it("maps wires to catalog families", () => {
    expect(wireFamily("openai")).toBe("chat");
    expect(wireFamily("anthropic")).toBe("anthropic");
    expect(wireFamily("responses")).toBe("responses");
    expect(presetToForm(REGISTRY[0].entries[0]).session).toBe("opencode");
  });
});

describe("withCurated (manual models in the catalog table)", () => {
  const cat = (id: string, family = "chat"): CatalogModel => ({ id, family, input: 1, output: 2, cache_read: 0, cache_write: 0 });
  const prow = (over: Partial<ProviderRow>): ProviderRow => ({ ...baseRow, ...over });

  it("appends curated ids the catalog doesn't list as synthetic rows (family from wire, unknown pricing)", () => {
    const rows = withCurated([cat("a"), cat("b")], [prow({ name: "p1", models: ["a", "test-manual-1"] })]);
    expect(rows.map((m) => m.id)).toEqual(["a", "b", "test-manual-1"]); // catalog first, extras after
    expect(rows[2]).toEqual({ id: "test-manual-1", family: "chat", input: -1, output: -1, cache_read: 0, cache_write: 0, manual: true });
    // input/-1 renders "—" via fmtPrice — the existing unknown-pricing convention
  });

  it("never duplicates ids the catalog already lists", () => {
    expect(withCurated([cat("a")], [prow({ name: "p1", models: ["a"] })])).toHaveLength(1);
  });

  it("derives each synthetic row's family from the curating entry's wire", () => {
    const rows = withCurated([], [
      prow({ name: "p1", wire: "anthropic", models: ["m1"] }),
      prow({ name: "p2", wire: "responses", models: ["m2"] }),
    ]);
    expect(rows.map((m) => [m.id, m.family])).toEqual([["m1", "anthropic"], ["m2", "responses"]]);
  });

  it("leaves the catalog untouched for an empty group (unconfigured provider)", () => {
    const catalog = [cat("a")];
    expect(withCurated(catalog, [])).toEqual(catalog);
  });

  it("emits one row for an id curated on TWO group entries (partially failed move)", () => {
    // doServe adds to the target then removes from the source; if the second
    // PUT fails the id sits on both wires — duplicate rows would collide keys
    const rows = withCurated([], [
      prow({ name: "a", models: ["x"] }),
      prow({ name: "b", wire: "anthropic", models: ["x"] }),
    ]);
    expect(rows).toHaveLength(1);
    expect(rows[0].id).toBe("x");
  });

  it("dedupes canonically: a curated 'gpt-5.5' is not re-synthesized against catalog 'gpt-5-5'", () => {
    // the server suppresses the manual extra via canonModelID; the client must
    // agree or the same model renders twice with two spellings
    expect(withCurated([cat("gpt-5-5")], [prow({ name: "p", models: ["gpt-5.5"] })])).toHaveLength(1);
    expect(withCurated([cat("GPT-5-5")], [prow({ name: "p", models: ["gpt-5.5"] })])).toHaveLength(1);
  });

  it("canonModelId mirrors the server's canonModelID (case + dot/dash)", () => {
    expect(canonModelId("Claude-Haiku-4.5")).toBe("claude-haiku-4-5");
    expect(canonModelId("gpt-5-5")).toBe(canonModelId("GPT-5.5"));
  });
});

describe("isCuratedModel (wire picker scope)", () => {
  const prow = (over: Partial<ProviderRow>): ProviderRow => ({ ...baseRow, ...over });

  it("is true for models served by any entry — the family cell shows the move picker", () => {
    const group = [prow({ name: "zai", models: ["glm-5.3-flashx"] })];
    expect(isCuratedModel(group, "glm-5.3-flashx")).toBe(true);
  });

  it("is false for catalog-only rows — they keep the read-only badge / serve-on picker", () => {
    expect(isCuratedModel([prow({ name: "zai", models: [] })], "glm-5.3-flash")).toBe(false);
  });

  it("covers curated ids on ANY entry of the group", () => {
    const group = [prow({ name: "ocg", models: [] }), prow({ name: "ocg-claude", wire: "anthropic", models: ["omen-alpha"] })];
    expect(isCuratedModel(group, "omen-alpha")).toBe(true);
  });
});

describe("toggleModels (row checkbox incl. manual models)", () => {
  it("hide of a SINGLE-model curated entry refuses (null) — never [] = wildcard serve-everything", () => {
    expect(toggleModels({ models: ["only"] }, "only", ["only"], false)).toBeNull();
  });

  it("hide of a curated id filters it out of the entry's list", () => {
    expect(toggleModels({ models: ["a", "manual-1"] }, "manual-1", [], false)).toEqual(["a"]);
  });

  it("hide works for curated ids even with an empty catalog universe (manual models offline)", () => {
    expect(toggleModels({ models: ["a", "b"] }, "a", [], false)).toEqual(["b"]);
  });

  it("hide on a wildcard still materializes the family complement", () => {
    expect(toggleModels({ models: [] }, "a", ["a", "b", "c"], false)).toEqual(["b", "c"]);
  });

  it("show re-curates a manually added id the catalog never lists (no silent delete)", () => {
    // the catalog universe can't contain a manual id; without unioning the id
    // itself, re-checking a hidden manual model would PUT an unchanged list
    // while toasting "visible" — an irreversible vanish from the table
    expect(toggleModels({ models: ["a"] }, "manual-1", ["a", "b"], true)).toEqual(["a", "manual-1"]);
    // a wildcard entry (empty list = serves everything) stays wildcard on show:
    // there is nothing to re-curate, the model is already served
    expect(toggleModels({ models: [] }, "manual-1", [], true)).toEqual([]);
  });

  it("show unions the toggled id into the universe — the per-row guard is the ROW's wire, not the catalog list", () => {
    // The row's entry is resolved before the call (modelsOf(familyFor(...))), so
    // the id belongs on that wire by construction; a manual id is absent from
    // the catalog list only because the upstream never listed it. The
    // cross-wire guard that DOES matter lives in bulkNextModels and is tested
    // there for multi-id sweeps.
    expect(toggleModels({ models: ["a"] }, "other-wire", ["a", "b"], true)).toEqual(["a", "other-wire"]);
  });

  it("show delegates to bulkNextModels (unions, no duplicates)", () => {
    expect(toggleModels({ models: ["a"] }, "b", ["a", "b", "c"], true)).toEqual(["a", "b"]);
    expect(toggleModels({ models: ["a", "b"] }, "b", ["a", "b"], true)).toEqual(["a", "b"]); // idempotent
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
