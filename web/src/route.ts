export type Tab = "live" | "usage" | "quota" | "latency" | "requests" | "endpoints" | "providers" | "settings";
export const TABS: Tab[] = ["live", "usage", "quota", "latency", "requests", "endpoints", "providers", "settings"];

// hash -> tab. Supported: "#/usage" (canonical), "#tab=usage", "#pw=x&tab=usage"
// (deep-link login; pw is consumed elsewhere). "keys" maps to "endpoints" in
// both syntaxes for pre-rename links.
export function tabFromHash(hash: string): Tab {
  const h = hash.replace(/^#\/?/, "");
  // first path segment only: "#/providers/<preset>" deep links to a detail page
  const t0 = h.split("?")[0].split("/")[0];
  const norm = t0 === "keys" ? "endpoints" : t0; // legacy name, canonical syntax
  const raw = (TABS as string[]).includes(norm) ? norm : new URLSearchParams(h).get("tab") || "";
  const t = raw === "keys" ? "endpoints" : raw; // legacy name, query syntax
  return (TABS as string[]).includes(t) ? (t as Tab) : "live";
}

// "#/providers/<preset>" → detail id ("" when not in a provider detail page)
export function providerDetailFromHash(hash: string): string {
	const parts = hash.replace(/^#\/?/, "").split("?")[0].split("/");
	if (parts[0] === "providers" && parts[1]) {
		try {
			return decodeURIComponent(parts[1]);
		} catch {
			return ""; // malformed percent escape → providers list, never crash the app
		}
	}
	// query syntax: #pw=...&tab=providers&provider=<preset>
	const q = new URLSearchParams(hash.replace(/^#\/?/, "")).get("provider");
	return q || "";
}
