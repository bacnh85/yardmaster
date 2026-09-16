export type Tab = "live" | "usage" | "latency" | "requests" | "endpoints" | "providers" | "settings";
export const TABS: Tab[] = ["live", "usage", "latency", "requests", "endpoints", "providers", "settings"];

// hash -> tab. Supported: "#/usage" (canonical), "#tab=usage", "#pw=x&tab=usage"
// (deep-link login; pw is consumed elsewhere). "keys" maps to "endpoints" in
// both syntaxes for pre-rename links.
export function tabFromHash(hash: string): Tab {
  const h = hash.replace(/^#\/?/, "");
  const t0 = h.split("?")[0];
  const norm = t0 === "keys" ? "endpoints" : t0; // legacy name, canonical syntax
  const raw = (TABS as string[]).includes(norm) ? norm : new URLSearchParams(h).get("tab") || "";
  const t = raw === "keys" ? "endpoints" : raw; // legacy name, query syntax
  return (TABS as string[]).includes(t) ? (t as Tab) : "live";
}
