import { describe, expect, it } from "vitest";
import { TABS, tabFromHash } from "./route";

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
});
