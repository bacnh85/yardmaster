import { describe, expect, it } from "vitest";
import { TABS, tabFromHash, providerDetailFromHash } from "./route";

describe("tabFromHash", () => {
  it.each([
    ["#/usage", "usage"],
    ["#tab=usage", "usage"],
    ["#pw=secret&tab=latency", "latency"], // deep-link login format
    ["#/endpoints?shot=1", "endpoints"], // path + query
    ["#/keys", "endpoints"], // legacy name, canonical syntax
    ["#tab=keys", "endpoints"], // legacy name, legacy syntax
    ["#/bogus", "live"],
    ["#pw=secret", "live"], // pw only, no tab
    ["#", "live"],
    ["", "live"],
  ])("%s -> %s", (hash, want) => {
    expect(tabFromHash(hash)).toBe(want);
  });

  it("covers every tab", () => {
    for (const t of TABS) expect(tabFromHash(`#/${t}`)).toBe(t);
  });

  it("detail deep links keep the providers tab", () => {
    expect(tabFromHash("#/providers/opencode-go")).toBe("providers");
  });
});

describe("providerDetailFromHash", () => {
  it.each([
    ["#/providers/opencode-go", "opencode-go"],
    ["#/providers/deep-seek%2Bx", "deep-seek+x"], // encoded id
    ["#/providers", ""],
    ["#/usage", ""],
    ["#pw=x&tab=providers&provider=opencode-go", "opencode-go"], // query syntax
    ["#pw=x&tab=providers", ""],
    ["#/providers/%zz", ""], // malformed escape must not throw
    ["#/providers/%", ""],
  ])("%s -> %s", (hash, want) => {
    expect(providerDetailFromHash(hash)).toBe(want);
  });
});
