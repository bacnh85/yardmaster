import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { copyText } from "./clipboard";

// copyText touches document + navigator globals — stub them with plain objects
// (no jsdom needed: it only uses createElement/appendChild/select/remove/execCommand).
type ExecFn = () => boolean;

function installDom(opts: { clipboard?: { writeText: ReturnType<typeof vi.fn> }; execCommand: ExecFn }) {
  const el = {
    value: "", style: {} as Record<string, string>,
    select: vi.fn(), remove: vi.fn(),
  };
  (globalThis as Record<string, unknown>).document = {
    createElement: vi.fn(() => el),
    body: { appendChild: vi.fn() },
    execCommand: opts.execCommand,
  };
  (globalThis as Record<string, unknown>).navigator = {
    ...(opts.clipboard ? { clipboard: opts.clipboard } : {}),
  };
  return el;
}

beforeEach(() => {
  vi.stubGlobal("navigator", {});
});
afterEach(() => {
  vi.unstubAllGlobals();
  delete (globalThis as Record<string, unknown>).document;
  delete (globalThis as Record<string, unknown>).navigator;
});

describe("copyText", () => {
  it("uses navigator.clipboard when available", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    const el = installDom({ clipboard: { writeText }, execCommand: () => true });
    await copyText("ar-1");
    expect(writeText).toHaveBeenCalledWith("ar-1");
    expect(el.select).not.toHaveBeenCalled(); // fallback never ran
  });

  it("falls back to execCommand when clipboard API is absent (insecure origin)", async () => {
    const el = installDom({ execCommand: () => true });
    await expect(copyText("ar-2")).resolves.toBeUndefined();
    expect(el.select).toHaveBeenCalled();
    expect(el.value).toBe("ar-2");
    expect(el.remove).toHaveBeenCalled();
  });

  it("falls back when clipboard writeText rejects (permission denied)", async () => {
    const writeText = vi.fn().mockRejectedValue(new Error("denied"));
    installDom({ clipboard: { writeText }, execCommand: () => true });
    await expect(copyText("ar-3")).resolves.toBeUndefined();
  });

  it("rejects when execCommand returns false — no silent success", async () => {
    installDom({ execCommand: () => false });
    await expect(copyText("ar-4")).rejects.toThrow("copy failed");
  });

  it("rejects when both clipboard API and execCommand fail", async () => {
    const writeText = vi.fn().mockRejectedValue(new Error("denied"));
    installDom({ clipboard: { writeText }, execCommand: () => false });
    await expect(copyText("ar-5")).rejects.toThrow("copy failed");
  });
});
