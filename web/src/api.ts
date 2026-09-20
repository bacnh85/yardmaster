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
  cache_read: number; cache_write: number; cost_usd: number;
  ttft_p50_ms: number | null; ttft_p95_ms: number | null; avg_dur_ms: number;
  series: { ts: number; requests: number; errors: number; tok_in: number; tok_out: number; cost: number }[];
}
export interface ReqRow {
  id: number; ts: number; key: string; model: string; provider: string;
  status: number; stream: boolean; ttft_ms: number; dur_ms: number;
  tok_in: number; tok_out: number; cache_read: number; cache_write: number;
  cost_usd: number; err: string; attempts: number;
}
export interface BreakdownRow {
  name: string; requests: number; errors: number; tok_in: number; tok_out: number;
  cache_read: number; cost: number; ttft_p50_ms: number | null;
}
export interface KeyRow { name: string; key_suffix: string; allow: string[]; rpm: number }
export interface ProviderRow {
  name: string; wire: string; base_url: string; models: string[];
  prefix?: string; // absent from older servers → prefill degrades to ""
  session?: string; // absent from older servers → prefill degrades to ""
  preset: string; disabled: boolean;
  dispatch_interval_ms: number; auth_type: string;
  adaptive_thinking: boolean; inject_cache_control: boolean;
  connections: { label: string; suffix: string }[];
  accounts: { name: string; kind: string; disabled: boolean; expires_at: number; state?: string; last_error?: string }[];
}
export interface CatalogModel {
  id: string; name?: string; family: string; context?: number; max_output?: number;
  input: number; output: number; cache_read: number; cache_write: number;
  reasoning?: boolean; tool_call?: boolean; image?: boolean; free?: boolean;
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
export const fmtUSD = (n: number) =>
  n === 0 ? "$0" : n < 0.01 ? "<$0.01" : "$" + n.toFixed(2);
export const fmtMs = (n: number | null | undefined) =>
  n == null ? "–" : n >= 1000 ? `${(n / 1000).toFixed(1)}s` : `${Math.round(n)}ms`; // seconds ≥1s: mixed units on adjacent cards read worse
export const fmtTime = (ts: number) => new Date(ts).toLocaleTimeString();
export const fmtDur = (ms: number) =>
  ms < 1000 ? `${Math.round(ms)}ms` : `${(ms / 1000).toFixed(1)}s`;
