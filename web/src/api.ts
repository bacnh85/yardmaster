const j = async (r: Response) => {
  if (r.status === 401) throw new Error("unauthorized");
  if (!r.ok) throw new Error((await r.text()) || `http ${r.status}`);
  return r.json();
};

const send = (method: string) => (path: string, body?: unknown) =>
  fetch(`/admin/api/${path}`, {
    method,
    headers: body === undefined ? undefined : { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  }).then(j);

export const post = send("POST");
export const put = send("PUT");
export const del = send("DELETE");

export async function login(password: string) {
  const r = await fetch("/admin/api/login", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ password }),
  });
  return j(r);
}

export const get = (path: string) => fetch(`/admin/api/${path}`).then(j);

export interface Summary {
  requests: number; errors: number; tok_in: number; tok_out: number;
  cache_read: number; cache_write: number; cached_requests?: number;
  cache_saved_usd?: number; cost_usd: number;
  ttft_p50_ms: number | null; ttft_p95_ms: number | null; avg_dur_ms: number;
  series: { ts: number; requests: number; errors: number; tok_in: number; tok_out: number; cache_read?: number; cost: number }[];
}
export interface ReqRow {
  id: number; ts: number; key: string; model: string; provider: string;
  status: number; stream: boolean; ttft_ms: number; dur_ms: number;
  tok_in: number; tok_out: number; cache_read: number; cache_write: number;
  cost_usd: number; err: string; attempts: number;
}
export interface BreakdownRow {
  name: string; requests: number; errors: number; tok_in: number; tok_out: number;
  cache_read: number; cache_write?: number; cached_requests?: number; cost: number; ttft_p50_ms: number | null;
}
export interface ModelSeriesPoint { ts: number; model: string; tok_in: number; tok_out: number; cost: number }
export interface KeyRow {
  name: string; key_suffix: string; allow: string[]; rpm: number; usage?: boolean | null;
  id?: string; // 32-hex derived reference handle (absent from older servers)
  created_at?: number; // unix ms; 0/undefined = legacy key, unknown
  last_used?: number; // unix ms; 0/undefined = never used
}
export interface ProviderRow {
  name: string; wire: string; base_url: string; models: string[];
  prefix?: string; // absent from older servers → prefill degrades to ""
  session?: string; // absent from older servers → prefill degrades to ""
  rotation?: string; // absent from older servers → "first"
  subscription?: string; // absent from older servers → "" (no plan tier)
  preset: string; disabled: boolean;
  dispatch_interval_ms: number; auth_type: string;
  adaptive_thinking: boolean; inject_cache_control: boolean;
  zcode_signing?: boolean; // absent from older servers → prefill degrades to false
  extra_headers?: Record<string, string> | null;
  body_overrides?: Record<string, unknown> | null;
  connections: { label: string; suffix: string; disabled?: boolean }[];
  accounts: { name: string; kind: string; disabled: boolean; expires_at: number; state?: string; last_error?: string }[];
}
export interface RouteRow {
  match: string; chain: string[];
  strategy?: string; // "priority" (default) | "weighted-rr"
  weights?: number[]; // weighted-rr only, index-aligned with chain
}
export interface ComboMemberRow {
  provider: string;
  model?: string; // upstream id at that provider; absent = combo's model
  keys?: string[]; // key labels / account names; absent = all connections
  weight?: number; // weighted-rr only
}
export interface ComboRow {
  name: string; model: string;
  strategy?: string; // "priority" (default) | "weighted-rr"
  members: ComboMemberRow[];
}
export interface ComboUsageRow {
  combo: string; provider: string;
  requests: number; errors: number;
  tok_in: number; tok_out: number; cache_read: number; cost: number;
  failovers: number; avg_attempts: number; ttft_p50_ms?: number | null;
}
export interface CooldownRow { provider: string; key: string; until: string }
export interface CatalogModel {
  id: string; name?: string; family: string; context?: number; max_output?: number;
  input: number; output: number; cache_read: number; cache_write: number;
  reasoning?: boolean; tool_call?: boolean; image?: boolean; free?: boolean;
  manual?: boolean; // curated by hand: not in the provider's catalog (no metadata)
}
export interface ProbeResult {
  text: string; tok_in: number; tok_out: number;
  cost_usd: number; ttft_ms: number; dur_ms: number;
}
export interface LivePayload {
  active: { id: string; model: string; provider: string; key: string; stream: boolean; start: number; ttft_ms: number }[];
  inflight: number; total: number;
}

export interface KeyCreated { ok: boolean; key: string }

export const fmtN = (n: number | null | undefined) =>
  n == null ? "–" : n.toLocaleString("en-US", { maximumFractionDigits: 0 });
export const fmtUSD = (n: number, currency?: string) => {
  const sym = currency === "CNY" ? "¥" : "$";
  return n === 0 ? sym + "0" : n < 0.01 ? "<" + sym + "0.01" : sym + n.toFixed(2);
};
export const fmtMs = (n: number | null | undefined) =>
  n == null ? "–" : n >= 1000 ? `${(n / 1000).toFixed(1)}s` : `${Math.round(n)}ms`; // seconds ≥1s: mixed units on adjacent cards read worse
export const fmtTime = (ts: number) => new Date(ts).toLocaleTimeString();
export const fmtDur = (ms: number) =>
  ms < 1000 ? `${Math.round(ms)}ms` : `${(ms / 1000).toFixed(1)}s`;

/** Countdown label for an epoch-ms reset timestamp: "3h 57m", "4d", "—". */
export const fmtResetIn = (resetAtMs: number | null | undefined) => {
  if (!resetAtMs) return "—";
  const m = Math.round((resetAtMs - Date.now()) / 60000);
  if (m <= 0) return "—";
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60), rm = m % 60;
  if (h < 24) return rm ? `${h}h ${rm}m` : `${h}h`;
  const d = Math.floor(h / 24), rh = h % 24;
  return rh ? `${d}d ${rh}h` : `${d}d`;
};

export interface QuotaWindow { used: number; cap: number; unit?: string; exceeded?: boolean; reset_at?: number }
export interface QuotaAccount {
  label: string; suffix: string;
  five_hour?: QuotaWindow; weekly?: QuotaWindow;
  monthly?: QuotaWindow;
  monthly_credits?: number; monthly_total?: number; limited?: boolean; err?: string; currency?: string;
}
export interface QuotaGroup { source: string; providers: string[]; accounts: QuotaAccount[]; fetched_at: number }
