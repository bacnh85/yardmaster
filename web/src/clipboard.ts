// Copy text to the clipboard, resolving only when the text actually landed.
// navigator.clipboard is undefined on insecure origins (LAN http) and may
// reject on permission denials — fall back to the deprecated-but-universal
// execCommand path in both cases, and reject if that fails too.
export function copyText(text: string): Promise<void> {
  const fallback = (): Promise<void> => {
    const ta = document.createElement("textarea");
    ta.value = text;
    ta.style.position = "fixed";
    ta.style.opacity = "0";
    document.body.appendChild(ta);
    ta.select();
    const ok = document.execCommand("copy");
    ta.remove();
    return ok ? Promise.resolve() : Promise.reject(new Error("copy failed"));
  };
  if (navigator.clipboard?.writeText) {
    return navigator.clipboard.writeText(text).catch(fallback);
  }
  return fallback();
}
