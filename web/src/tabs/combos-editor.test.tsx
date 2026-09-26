// @vitest-environment jsdom
// The combo editor must render per-member model selects annotated with catalog
// metadata (context/pricing), with NO top-level combo-model field — the user
// picks each member's model, the annotations drive the consideration.
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { CombosTab } from "./CombosTab";

vi.mock("../hooks", async (orig) => ({
  ...(await orig<typeof import("../hooks")>()),
  useApi: (path: string) => {
    if (path === "combos") return { data: { combos: [] }, error: null, loading: false, reload: () => {} };
    if (path === "providers") return {
      data: { providers: [
        { name: "typesafe", wire: "classifier", models: ["jev-latest"], connections: [], accounts: [], disabled: false },
        { name: "ds-a", wire: "openai", models: ["deepseek-v4-flash"], connections: [], accounts: [], disabled: false },
      ] }, error: null, loading: false, reload: () => {} };
    if (path.startsWith("catalog")) return {
      data: { models: [
        { id: "jev-latest", family: "classifier", context: 32000, input: -1, output: -1, cache_read: 0, cache_write: 0 },
        { id: "deepseek-v4-flash", family: "chat", context: 128000, input: 0.3, output: 1.2, cache_read: 0.006, cache_write: 0 },
      ] }, error: null, loading: false, reload: () => {} };
    if (path.startsWith("combos/usage")) return { data: { usage: [] }, error: null, loading: false, reload: () => {} };
    return { data: null, error: null, loading: false, reload: () => {} };
  },
}));

vi.mock("../api", async (orig) => ({
  ...(await orig<typeof import("../api")>()),
  get: async () => ({}),
  put: async () => ({}),
}));

(globalThis as Record<string, unknown>).IS_REACT_ACT_ENVIRONMENT = true;

let host: HTMLDivElement | undefined;
let root: Root | undefined;
beforeEach(() => {
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
});
afterEach(() => {
  root?.unmount();
  root = undefined;
  host?.remove();
});

const openEditor = () => {
  act(() => root!.render(<CombosTab />));
  const btn = [...host!.querySelectorAll("button")].find((b) => b.textContent.includes("New combo"))!;
  act(() => { btn.dispatchEvent(new MouseEvent("click", { bubbles: true })); });
  return host!.querySelector(".modal")!;
};

describe("combo editor modal (per-member model selection)", () => {
  it("has no top-level combo model field; member model select carries ctx+price annotations", () => {
    const modal = openEditor();
    expect(document.querySelector("#cb-model")).toBeNull(); // combo model input removed
    expect(document.querySelector("#cb-name")).not.toBeNull();
    // combo type selector: chat default, decision option present
    const typeSel = modal.querySelector("#cb-type") as HTMLSelectElement;
    expect(typeSel).not.toBeNull();
    expect(typeSel.value).toBe("chat");
    expect([...typeSel.options].map((o) => o.value)).toEqual(["chat", "decision"]);

    // pick the provider on member 1 → model select appears
    const provSel = modal.querySelector('select[aria-label="provider 1"]') as HTMLSelectElement;
    const setV = Object.getOwnPropertyDescriptor(HTMLSelectElement.prototype, "value")!.set!;
    act(() => { setV.call(provSel, "ds-a"); provSel.dispatchEvent(new Event("change", { bubbles: true })); });

    const modelSel = modal.querySelector('select[aria-label="model at provider 1"]') as HTMLSelectElement;
    expect(modelSel).not.toBeNull();
    const options = [...modelSel.options].map((o) => o.textContent);
    expect(options.some((t) => t!.includes("deepseek-v4-flash · 128K · $0.3/$1.2"))).toBe(true);
    expect(options.some((t) => t!.includes("—"))).toBe(true); // blank placeholder option
  });

  it("decision type lists only classifier providers, then only their classifier models", () => {
    act(() => root!.render(<CombosTab />));
    const btn = [...host!.querySelectorAll("button")].find((b) => b.textContent.includes("New combo"))!;
    act(() => { btn.dispatchEvent(new MouseEvent("click", { bubbles: true })); });
    const typeSel = host!.querySelector("#cb-type") as HTMLSelectElement;
    const setV = Object.getOwnPropertyDescriptor(HTMLSelectElement.prototype, "value")!.set!;
    act(() => { setV.call(typeSel, "decision"); typeSel.dispatchEvent(new Event("change", { bubbles: true })); });

    const provSel = host!.querySelector('select[aria-label="provider 1"]') as HTMLSelectElement;
    const provOpts = [...provSel.options].map((o) => o.value);
    expect(provOpts).toEqual(["", "typesafe"]); // ds-a (openai) pruned, typesafe kept
    expect(provOpts).not.toContain("ds-a");

    act(() => { setV.call(provSel, "typesafe"); provSel.dispatchEvent(new Event("change", { bubbles: true })); });
    const modelSel = host!.querySelector('select[aria-label="model at provider 1"]') as HTMLSelectElement;
    const opts = [...modelSel.options].map((o) => o.textContent!);
    expect(opts.some((t) => t.includes("jev-latest"))).toBe(true);
    expect(opts.every((t) => !t.includes("deepseek"))).toBe(true); // chat model hidden in decision mode
  });
});
