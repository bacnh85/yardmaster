// @vitest-environment jsdom
// Models page per-provider pricing expansion: the +N toggle appears only on
// price-spread rows, clicking it reveals one sub-row per exposed provider
// (served-as id + own prices), − collapses. jsdom + native events, per
// endpoints-editor. IntersectionObserver is stubbed (lazy-scroll sentinel).
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ModelsTab } from "./ModelsTab";
import type { CatalogModel, ProviderRow } from "../api";

const catalog: CatalogModel[] = [
  {
    id: "Qwen/Qwen3.8-Omni-Flash", name: "Qwen3.8 Omni Flash", family: "chat",
    context: 1000000, input: 0.13, output: 0.43, cache_read: 0.016, cache_write: 0,
    reasoning: true, tool_call: true,
    providers: [
      { name: "cmdcode", wire: "openai", prefix: "cmd", exposed: true, served_as: "cmd/Qwen/Qwen3.8-Omni-Flash", input: 0.13, output: 0.43, cache_read: 0.016 },
      { name: "nvidia", wire: "openai", prefix: "nv", exposed: true, served_as: "nv/Qwen/Qwen3.8-Omni-Flash", input: 0, output: 0, cache_read: 0 },
    ],
  },
  {
    id: "glm-5.3", family: "chat", input: 1.4, output: 4.4, cache_read: 0.26, cache_write: 0,
    providers: [{ name: "zai", wire: "anthropic", exposed: true, served_as: "glm-5.3", input: 1.4, output: 4.4, cache_read: 0.26 }],
  },
];

vi.mock("../hooks", async (orig) => ({
  ...(await orig<typeof import("../hooks")>()),
  useApi: (path: string) => {
    if (path === "catalog") return { data: { models: catalog }, error: null, loading: false, reload: () => {} };
    if (path === "providers") return { data: { providers: [{ name: "cmdcode", models: [], connections: [] } as unknown as ProviderRow] }, error: null, loading: false, reload: () => {} };
    return { data: null, error: null, loading: false, reload: () => {} };
  },
}));
vi.mock("../clipboard", () => ({ copyText: async () => {} }));
vi.mock("../components", async (orig) => ({
  ...(await orig<typeof import("../components")>()),
  toast: () => {},
}));

class IO {
  observe() {}
  disconnect() {}
  unobserve() {}
}
(globalThis as Record<string, unknown>).IntersectionObserver = IO;
(globalThis as Record<string, unknown>).IS_REACT_ACT_ENVIRONMENT = true;

let host: HTMLDivElement | undefined;
let root: Root | undefined;
beforeEach(() => {
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
});
afterEach(() => {
  act(() => root?.unmount());
  host?.remove();
});

const btn = (label: string) =>
  [...document.querySelectorAll("button")].find((b) => b.getAttribute("aria-label") === label);

describe("ModelsTab per-provider pricing expansion", () => {
  it("shows +N only on spread rows; expand reveals per-provider price sub-rows", async () => {
    await act(async () => root?.render(<ModelsTab />));
    // glm-5.3: single provider, no toggle
    expect(btn("show per-provider pricing for glm-5.3")).toBeUndefined();
    const toggle = btn("show per-provider pricing for Qwen/Qwen3.8-Omni-Flash") as HTMLButtonElement;
    expect(toggle).toBeTruthy();
    expect(toggle.textContent).toBe("+2");
    expect(document.body.textContent).not.toContain("via nvidia"); // collapsed
    await act(async () => toggle.dispatchEvent(new MouseEvent("click", { bubbles: true })));
    expect(document.body.textContent).toContain("cmdcode");
    expect(document.body.textContent).toContain("nvidia");
    // sub-row shape: tree glyph + provider badge + served-as id (no "via" text,
    // prices under the row's price columns)
    const glyphs = [...document.querySelectorAll(".tree")].map((t) => t.textContent);
    expect(glyphs).toEqual(["├──", "└──"]);
    const sub = [...document.querySelectorAll("tr.prov-row")].find((r) => r.textContent?.includes("nvidia"));
    expect(sub?.textContent).not.toContain("via");
    expect(sub?.textContent).toContain("nv/Qwen/Qwen3.8-Omni-Flash");
    expect(sub?.querySelector(".badge")?.textContent).toBe("nvidia");
    expect(sub?.textContent).toContain("free"); // nvidia's own $0 price renders as free
    // collapse: − label, sub-rows gone
    const collapse = btn("hide per-provider pricing for Qwen/Qwen3.8-Omni-Flash") as HTMLButtonElement;
    expect(collapse.textContent).toBe("−");
    await act(async () => collapse.dispatchEvent(new MouseEvent("click", { bubbles: true })));
    expect(document.querySelectorAll("tr.prov-row")).toHaveLength(0);
  });
});

