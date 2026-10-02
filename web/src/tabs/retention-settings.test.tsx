// @vitest-environment jsdom
// Request-history retention card: renders the current config value, dirty
// gating, and the PUT payload. jsdom + native events, per endpoints-editor.
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { SettingsTab } from "./SettingsTab";

let cfgResponse: Record<string, unknown> = {};
let put_: unknown;
vi.mock("../api", async (orig) => ({
  ...(await orig<typeof import("../api")>()),
  get: async () => cfgResponse,
  put: async (_p: string, body: unknown) => { put_ = body; return {}; },
}));

(globalThis as Record<string, unknown>).IS_REACT_ACT_ENVIRONMENT = true;

let host: HTMLDivElement | undefined;
let root: Root | undefined;
beforeEach(() => {
  cfgResponse = { listen: ":8787", db_path: "y.db", retention_days: 0, providers: [], routes: [] };
  put_ = undefined;
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
});
afterEach(() => {
  act(() => root?.unmount());
  host?.remove();
});

const $ = (sel: string): HTMLSelectElement | HTMLButtonElement =>
  document.querySelector(sel) as HTMLSelectElement | HTMLButtonElement;

describe("SettingsTab request history", () => {
  it("renders the configured retention and gates save until dirty", async () => {
    await act(async () => root?.render(<SettingsTab />));
    const sel = $("#retention-days") as HTMLSelectElement;
    expect(sel.value).toBe("0");
    const save = [...document.querySelectorAll("button")].find((b) => (b.textContent ?? "").includes("request history")) as HTMLButtonElement;
    expect(save).toBeTruthy();
    expect(save.disabled).toBe(true); // not dirty yet
    await act(async () => { sel.value = "7"; sel.dispatchEvent(new Event("change", { bubbles: true })); });
    expect(save.disabled).toBe(false);
    await act(async () => save.dispatchEvent(new MouseEvent("click", { bubbles: true })));
    expect(put_).toEqual({ days: 7 });
  });
});
