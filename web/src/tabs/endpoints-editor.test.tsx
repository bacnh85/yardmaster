// @vitest-environment jsdom
// The key dialogs' model picker: all-models toggle default, search + checkbox
// selection, pattern escape hatch, and the allow payload round-trip. web_interact
// cannot drive these modals headlessly — jsdom + native events, per combos-editor.
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { EndpointsTab } from "./EndpointsTab";
import type { KeyRow } from "../api";

const routableModels = [
  { id: "glm-5.3", family: "chat" },
  { id: "glm-5.3-flash", family: "chat" },
  { id: "kimi-k3", family: "chat" },
  { id: "combo/jev", family: "decision" },
];

let posted: unknown, put_: unknown;
vi.mock("../hooks", async (orig) => ({
  ...(await orig<typeof import("../hooks")>()),
  useApi: (path: string) => {
    if (path === "keys") return {
      data: { keys: [{
        name: "agent", key_suffix: "ab12cd", allow: ["glm-*", "kimi-k3"], rpm: 0,
        id: "x", created_at: 1, last_used: 0,
      } as unknown as KeyRow, {
        name: "allkey", key_suffix: "ff00ff", allow: ["*"], rpm: 0,
        id: "y", created_at: 1, last_used: 0,
      } as unknown as KeyRow] }, error: null, loading: false, reload: () => {} };
    if (path === "config") return { data: { listen: ":8787" }, error: null, loading: false, reload: () => {} };
    if (path === "routable") return { data: { models: routableModels }, error: null, loading: false, reload: () => {} };
    return { data: null, error: null, loading: false, reload: () => {} };
  },
}));

vi.mock("../api", async (orig) => ({
  ...(await orig<typeof import("../api")>()),
  get: async () => ({}),
  post: async (_p: string, body: unknown) => { posted = body; return { ok: true, key: "ar-new" }; },
  put: async (_p: string, body: unknown) => { put_ = body; return {}; },
  del: async () => ({}),
}));

(globalThis as Record<string, unknown>).IS_REACT_ACT_ENVIRONMENT = true;

let host: HTMLDivElement | undefined;
let root: Root | undefined;
beforeEach(() => {
  posted = undefined; put_ = undefined;
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
});
afterEach(() => {
  root?.unmount();
  root = undefined;
  host?.remove();
});

const click = (el: Element) => act(() => { el.dispatchEvent(new MouseEvent("click", { bubbles: true })); });
/** jsdom/React-19: a native click on a controlled checkbox fires onChange with
 *  the flipped value (React tracks DOM state) — use for checkboxes. */
const toggleBox = (el: HTMLInputElement) => click(el);
const setValue = (el: HTMLInputElement, v: string) => {
  const set = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")!.set!;
  act(() => { set.call(el, v); el.dispatchEvent(new Event("input", { bubbles: true })); });
};
const submit = (form: HTMLFormElement) =>
  act(() => { form.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true })); });

const openAdd = () => {
  act(() => root!.render(<EndpointsTab />));
  click([...host!.querySelectorAll("button")].find((b) => b.textContent!.includes("Add API key"))!);
};
const openEdit = (name: string) => {
  act(() => root!.render(<EndpointsTab />));
  click([...host!.querySelectorAll("button")].find((b) => b.getAttribute("aria-label") === `edit key ${name}`)!);
};

describe("key dialog model picker", () => {
  it("key rows keep table-cell layout for the edit/revoke actions", () => {
    // regression: <td className="row"> made the cell display:flex, which drops
    // the td out of table layout (anonymous wrapper) and misaligned the row
    act(() => root!.render(<EndpointsTab />));
    const cell = [...host!.querySelectorAll("td")].find((td) => td.textContent === "editrevoke")!;
    expect(cell.className).not.toContain("row");
    expect(cell.querySelector("div.row")).not.toBeNull();
  });

  it("defaults to all models; search + exact pick + pattern land in the POST body", () => {
    openAdd();
    const all = document.querySelector("#mp-all") as HTMLInputElement;
    expect(all.checked).toBe(true);
    expect(document.querySelector(".mp-list")).toBeNull(); // list hidden while all-models

    toggleBox(all);
    // unchecking leaves all-mode for real: checkbox off + list visible + hint
    // (computed-only checkbox was idempotent — [] and ["*"] both render "all")
    expect((document.querySelector("#mp-all") as HTMLInputElement).checked).toBe(false);
    expect(document.querySelector(".mp-list")).not.toBeNull();
    // unselected hint states the all-models coercion
    expect(document.body.textContent).toContain("no models selected — saving will share all models (*)");

    const search = document.querySelector('input[aria-label="search models"]') as HTMLInputElement;
    setValue(search, "glm");
    const rows = [...document.querySelectorAll(".mp-row")].map((r) => r.textContent);
    expect(rows.some((t) => t!.includes("glm-5.3"))).toBe(true);
    expect(rows.every((t) => !t!.includes("kimi-k3"))).toBe(true);

    const box = document.querySelector('input[id^="mp-glm-5_3"]') as HTMLInputElement;
    toggleBox(box);
    // summary line states the snapshot semantics
    expect(document.body.textContent).toContain("1 model — exact picks cover current ids only");

    const pat = document.querySelector('input[aria-label="add glob pattern"]') as HTMLInputElement;
    setValue(pat, "ds*");
    click([...document.querySelectorAll(".mp-pattern button")].find((b) => b.textContent === "Add")!);
    expect(document.body.textContent).toContain("1 pattern");

    const form = document.querySelector(".modal form") as HTMLFormElement;
    setValue(document.querySelector("#ak-name") as HTMLInputElement, "t");
    submit(form);
    expect(posted).toMatchObject({ name: "t", allow: ["glm-5.3", "ds*"] });
  });

  it("edit round-trips stored patterns and exact ids without dropping either", () => {
    openEdit("agent");
    // glm-* is not routable → pattern chip preserved; kimi-k3 checked in the list
    const chips = [...document.querySelectorAll(".mp-chip .mono")].map((c) => c.textContent);
    expect(chips).toEqual(["glm-*", "kimi-k3"]);
    expect([...document.querySelectorAll(".mp-chip .badge")].length).toBe(1); // one pattern badge
    const kimi = document.querySelector('input[id^="mp-kimi-k3"]') as HTMLInputElement;
    expect(kimi.checked).toBe(true);

    const form = document.querySelector(".modal form") as HTMLFormElement;
    submit(form);
    expect(put_).toMatchObject({ allow: ["glm-*", "kimi-k3"] });
  });

  it("all-models key edits to [] and PUT coerces explicitly to *", () => {
    // key with allow ["*"] renders the toggle on; saving sends ["*"]
    openEdit("allkey");
    expect((document.querySelector("#mp-all") as HTMLInputElement).checked).toBe(true);
    submit(document.querySelector(".modal form") as HTMLFormElement);
    expect(put_).toMatchObject({ allow: ["*"] });
  });

  it("unchecking an explicit-* key leaves all-mode; re-checking restores all", () => {
    openEdit("allkey"); // draft allow starts [] (["*"] normalized)
    // uncheck: leaves all-mode (list visible). allow stays unsubmitted-as-*:
    // empty draft still PUTs ["*"] — the * is never silently dropped.
    toggleBox(document.querySelector("#mp-all") as HTMLInputElement);
    expect((document.querySelector("#mp-all") as HTMLInputElement).checked).toBe(false);
    expect(document.querySelector(".mp-list")).not.toBeNull();
    submit(document.querySelector(".modal form") as HTMLFormElement);
    expect(put_).toMatchObject({ allow: ["*"] }); // empty selection → explicit all (server coercion preserved)
  });

  it("re-checking after unchecking restores all-mode", () => {
    openEdit("allkey");
    toggleBox(document.querySelector("#mp-all") as HTMLInputElement); // off
    toggleBox(document.querySelector("#mp-all") as HTMLInputElement); // on again
    expect((document.querySelector("#mp-all") as HTMLInputElement).checked).toBe(true);
    expect(document.querySelector(".mp-list")).toBeNull();
  });
});
