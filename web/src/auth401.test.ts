import { describe, expect, it, vi } from "vitest";
import { get, put } from "./api";

// 401s from any API call announce on window so App can drop back to <Login/>
// (issue #1: expired sessions used to strand the user on error banners)
describe("api 401 event", () => {
  it("dispatches auth:unauthorized when a response is 401", async () => {
    const on401 = vi.fn();
    window.addEventListener("auth:unauthorized", on401);
    try {
      vi.stubGlobal("fetch", vi.fn(async () => new Response("unauthorized", { status: 401 })));
      await expect(get("summary?hours=1")).rejects.toThrow("unauthorized");
      await expect(put("retention", { days: 1 })).rejects.toThrow("unauthorized");
      expect(on401).toHaveBeenCalledTimes(2);
    } finally {
      window.removeEventListener("auth:unauthorized", on401);
      vi.unstubAllGlobals();
    }
  });

  it("does not dispatch on other statuses", async () => {
    const on401 = vi.fn();
    window.addEventListener("auth:unauthorized", on401);
    try {
      vi.stubGlobal("fetch", vi.fn(async () => new Response("boom", { status: 500 })));
      await expect(get("summary?hours=1")).rejects.toThrow("boom");
      expect(on401).not.toHaveBeenCalled();
    } finally {
      window.removeEventListener("auth:unauthorized", on401);
      vi.unstubAllGlobals();
    }
  });
});
