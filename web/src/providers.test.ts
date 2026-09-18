import { describe, expect, it } from "vitest";
import { EMPTY_FORM, providerBody } from "./tabs/ProvidersTab";

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
});
