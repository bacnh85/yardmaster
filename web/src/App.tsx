import { useCallback, useEffect, useRef, useState } from "react";
import {
  get, post, put, del, login, fmtDur, fmtMs, fmtN, fmtTime, fmtUSD,
  BreakdownRow, KeyCreated, KeyRow, LivePayload, ProviderRow, ReqRow, Summary,
} from "./api";
import { TimeChart } from "./Chart";

type Tab = "live" | "usage" | "latency" | "requests" | "endpoints" | "providers" | "settings";
const TABS: Tab[] = ["live", "usage", "latency", "requests", "endpoints", "providers", "settings"];
// read once at module init: the pw-login flow clears the hash before tabs mount
const BOOT = new URLSearchParams(location.hash.slice(1));
const NAV: { group: string; tabs: Tab[] }[] = [
  { group: "Proxy", tabs: ["live", "usage", "latency", "requests"] },
  { group: "Access", tabs: ["endpoints", "providers"] },
  { group: "System", tabs: ["settings"] },
];

export default function App() {
  const [authed, setAuthed] = useState<boolean | null>(null);
  const [tab, setTab] = useState<Tab>(() => {
    const t = BOOT.get("tab") as Tab | null;
    if ((t as string) === "keys") return "endpoints"; // legacy deep links
    return t && TABS.includes(t) ? t : "live";
  });

  // deep link: #pw=<password>&tab=<tab> (hash stays client-side; also enables
  // headless captures). pw is consumed and cleared from the hash after login.
  useEffect(() => {
    const h = BOOT;
    const pw = h.get("pw");
    if (pw) {
      history.replaceState(null, "", location.pathname);
      login(pw).then(() => setAuthed(true)).catch(() => setAuthed(false));
      return;
    }
    get("summary?hours=1").then(() => setAuthed(true)).catch(() => setAuthed(false));
  }, []);

  if (authed === null) return <div className="empty">loading…</div>;
  if (!authed) return <Login onOk={() => setAuthed(true)} />;

  return (
    <div className="shell">
      <nav className="side" aria-label="main navigation">
        <div className="brand">agent-router</div>
        {NAV.map((g) => (
          <div key={g.group} className="nav-group-wrap">
            <div className="nav-group">{g.group}</div>
            {g.tabs.map((t) => (
              <button key={t} role="tab" aria-selected={tab === t} className="nav-item" onClick={() => setTab(t)}>
                {t}
              </button>
            ))}
          </div>
        ))}
        <div className="ver faint">v0.1.0</div>
      </nav>
      <main>
        {tab === "live" && <LiveTab />}
        {tab === "usage" && <UsageTab />}
        {tab === "latency" && <LatencyTab />}
        {tab === "requests" && <RequestsTab />}
        {tab === "endpoints" && <EndpointsTab />}
        {tab === "providers" && <ProvidersTab />}
        {tab === "settings" && <SettingsTab />}
      </main>
    </div>
  );
}

function Login({ onOk }: { onOk: () => void }) {
  const [pw, setPw] = useState("");
  const [err, setErr] = useState("");
  return (
    <form className="login card" onSubmit={(e) => {
      e.preventDefault();
      login(pw).then(onOk).catch(() => setErr("wrong password"));
    }}>
      <h3>agent-router</h3>
      <input type="password" placeholder="admin password" value={pw} autoFocus
        onChange={(e) => setPw(e.target.value)} />
      {err && <div className="error">{err}</div>}
      <button className="btn primary" type="submit">Sign in</button>
    </form>
  );
}

/* ---- Live ---- */

function LiveTab() {
  const [live, setLive] = useState<LivePayload | null>(null);
  const [connected, setConnected] = useState(false);
  useEffect(() => {
    // shot=1: single snapshot via fetch (for headless captures) instead of SSE
    if (BOOT.get("shot")) {
      get("summary?hours=1").then((s: { inflight: number; total: number }) =>
        setLive({ active: [], inflight: s.inflight, total: s.total })).then(() => setConnected(true));
      return;
    }
    const es = new EventSource("/admin/api/live");
    es.onopen = () => setConnected(true);
    es.onmessage = (m) => setLive(JSON.parse(m.data));
    es.onerror = () => setConnected(false);
    return () => es.close();
  }, []);
  if (!connected) return <div className="reconnecting">reconnecting…</div>;
  if (!live) return null;
  return (
    <div className="grid stats">
      <div className="card stat">
        <div className="label">Active streams</div>
        <div className="value">
          <span className="livedot on" style={{ display: "inline-block", marginRight: 8 }} />
          {fmtN(live.inflight)}
        </div>
        <div className="sub">{fmtN(live.total)} requests since start</div>
      </div>
      <div className="card" style={{ gridColumn: "1 / -1" }}>
        <h3>In flight</h3>
        {live.active.length === 0 ? (
          <div className="empty">no active requests</div>
        ) : (
          <table>
            <thead><tr>
              <th>model</th><th>provider</th><th>key</th><th className="n">ttft</th><th className="n">elapsed</th>
            </tr></thead>
            <tbody>
              {live.active.map((a) => (
                <tr key={a.id}>
                  <td>{a.model}</td>
                  <td>{a.provider}</td>
                  <td>{a.key}</td>
                  <td className="n">{a.ttft_ms > 0 ? fmtMs(a.ttft_ms) : "…"}</td>
                  <td className="n">{fmtDur(Date.now() - a.start)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </div>
  );
}

/* ---- Usage ---- */

function useSummary(hours: number) {
  const [sum, setSum] = useState<Summary | null>(null);
  const load = useCallback(() => {
    get(`summary?hours=${hours}&bucket=hour`).then((s: { summary: Summary }) => setSum(s.summary));
  }, [hours]);
  useEffect(() => { load(); const t = setInterval(load, 10000); return () => clearInterval(t); }, [load]);
  return sum;
}

function StatCard({ label, value, sub }: { label: string; value: string; sub?: string }) {
  return (
    <div className="card stat">
      <div className="label">{label}</div>
      <div className="value">{value}</div>
      {sub && <div className="sub">{sub}</div>}
    </div>
  );
}

function UsageTab() {
  const [hours, setHours] = useState(24);
  const sum = useSummary(hours);
  const [byModel, setByModel] = useState<BreakdownRow[]>([]);
  const [byProvider, setByProvider] = useState<BreakdownRow[]>([]);
  useEffect(() => {
    get(`breakdown?by=model&hours=${hours}`).then((r: { breakdown: BreakdownRow[] }) => setByModel(r.breakdown));
    get(`breakdown?by=provider&hours=${hours}`).then((r: { breakdown: BreakdownRow[] }) => setByProvider(r.breakdown));
  }, [hours]);

  const range = (
    <select value={hours} onChange={(e) => setHours(+e.target.value)} aria-label="time range"
      style={{ padding: "6px 10px", border: "1px solid var(--line)", borderRadius: "var(--radius-sm)" }}>
      {[1, 24, 168, 720].map((h) => <option key={h} value={h}>{h === 1 ? "last hour" : h === 24 ? "last 24h" : h === 168 ? "last 7d" : "last 30d"}</option>)}
    </select>
  );

  if (!sum) return null;
  const cacheHit = sum.tok_in > 0 ? Math.round((sum.cache_read / sum.tok_in) * 100) : 0;
  return (
    <div style={{ display: "grid", gap: 12 }}>
      <div style={{ display: "flex", alignItems: "center", gap: 12 }}>
        <h3 style={{ margin: 0, flex: 1 }}>Usage</h3>{range}
      </div>
      <div className="grid stats">
        <StatCard label="Requests" value={fmtN(sum.requests)} sub={`${fmtN(sum.errors)} errors`} />
        <StatCard label="Cost" value={fmtUSD(sum.cost_usd)} />
        <StatCard label="Tokens in" value={fmtN(sum.tok_in)} sub={`${cacheHit}% cache read`} />
        <StatCard label="Tokens out" value={fmtN(sum.tok_out)} />
      </div>
      <div className="card">
        <h3>Requests &amp; tokens</h3>
        <TimeChart data={sum.series} height={220}
          series={[{ key: "requests", label: "requests" }, { key: "errors", label: "errors" }]} />
      </div>
      <div className="card">
        <h3>Tokens</h3>
        <TimeChart data={sum.series} height={200}
          series={[{ key: "tok_in", label: "in" }, { key: "tok_out", label: "out" }]} />
      </div>
      <div className="card">
        <h3>Cost</h3>
        <TimeChart data={sum.series} height={200} series={[{ key: "cost", label: "cost (USD)" }]} />
      </div>
      <div className="grid half">
        <div className="card"><h3>By model</h3><BreakTable rows={byModel} /></div>
        <div className="card"><h3>By provider</h3><BreakTable rows={byProvider} /></div>
      </div>
    </div>
  );
}

function BreakTable({ rows }: { rows: BreakdownRow[] }) {
  if (rows.length === 0) return <div className="empty">no data in range</div>;
  return (
    <table>
      <thead><tr>
        <th>name</th><th className="n">reqs</th><th className="n">errors</th>
        <th className="n">tok in</th><th className="n">tok out</th><th className="n">cache rd</th>
        <th className="n">cost</th><th className="n">ttft p50</th>
      </tr></thead>
      <tbody>
        {rows.map((r) => (
          <tr key={r.name}>
            <td>{r.name}</td>
            <td className="n">{fmtN(r.requests)}</td>
            <td className="n">{r.errors > 0 ? <span className="err">{fmtN(r.errors)}</span> : 0}</td>
            <td className="n">{fmtN(r.tok_in)}</td>
            <td className="n">{fmtN(r.tok_out)}</td>
            <td className="n">{fmtN(r.cache_read)}</td>
            <td className="n">{fmtUSD(r.cost)}</td>
            <td className="n">{fmtMs(r.ttft_p50_ms)}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

/* ---- Latency ---- */

function LatencyTab() {
  const sum = useSummary(24);
  if (!sum) return null;
  return (
    <div style={{ display: "grid", gap: 12 }}>
      <div className="grid stats">
        <StatCard label="TTFT p50" value={fmtMs(sum.ttft_p50_ms)} />
        <StatCard label="TTFT p95" value={fmtMs(sum.ttft_p95_ms)} />
        <StatCard label="Avg duration" value={fmtDur(sum.avg_dur_ms)} />
        <StatCard label="Error rate"
          value={sum.requests > 0 ? `${Math.round((sum.errors / sum.requests) * 100)}%` : "–"}
          sub={`${fmtN(sum.errors)} of ${fmtN(sum.requests)}`} />
      </div>
      <div className="card">
        <h3>Errors per hour (24h)</h3>
        <TimeChart data={sum.series} height={200} series={[{ key: "errors", label: "errors" }]} />
      </div>
    </div>
  );
}

/* ---- Requests ---- */

function RequestsTab() {
  const [rows, setRows] = useState<ReqRow[]>([]);
  const load = useCallback(() => { get("requests?limit=100").then((r: { requests: ReqRow[] }) => setRows(r.requests)); }, []);
  useEffect(() => { load(); const t = setInterval(load, 5000); return () => clearInterval(t); }, [load]);
  if (rows.length === 0) return <div className="empty">no requests yet</div>;
  return (
    <div className="card" style={{ padding: 0, overflowX: "auto" }}>
      <table>
        <thead><tr>
          <th>time</th><th>model</th><th>provider</th><th>key</th><th>status</th>
          <th className="n">ttft</th><th className="n">dur</th>
          <th className="n">in</th><th className="n">out</th><th className="n">cache</th>
          <th className="n">cost</th><th className="n">tries</th><th></th>
        </tr></thead>
        <tbody>
          {rows.map((r) => (
            <tr key={r.id}>
              <td className="mono">{fmtTime(r.ts)}</td>
              <td>{r.model}</td>
              <td>{r.provider}</td>
              <td>{r.key}</td>
              <td><StatusBadge status={r.status} /></td>
              <td className="n">{fmtMs(r.ttft_ms)}</td>
              <td className="n">{fmtDur(r.dur_ms)}</td>
              <td className="n">{fmtN(r.tok_in)}</td>
              <td className="n">{fmtN(r.tok_out)}</td>
              <td className="n">{fmtN(r.cache_read)}</td>
              <td className="n">{fmtUSD(r.cost_usd)}</td>
              <td className="n">{r.attempts}</td>
              <td>{r.err && <span className="err mono">{r.err.slice(0, 60)}</span>}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function StatusBadge({ status }: { status: number }) {
  const cls = status < 300 ? "ok" : status < 500 ? "warn" : "danger";
  return <span className={`badge ${cls}`}>{status}</span>;
}

/* ---- Endpoints (inbound API + keys) ---- */

const ENDPOINTS: [string, string, string][] = [
  ["POST", "/v1/chat/completions", "OpenAI wire (streaming supported)"],
  ["POST", "/v1/messages", "Anthropic wire (streaming supported)"],
  ["POST", "/v1/messages/count_tokens", "Anthropic token count"],
  ["GET", "/v1/models", "models routable by your key"],
  ["GET", "/healthz", "liveness (no auth)"],
];

function EndpointsTab() {
  const [keys, setKeys] = useState<KeyRow[]>([]);
  const [name, setName] = useState("");
  const [allow, setAllow] = useState("*");
  const [created, setCreated] = useState<KeyCreated | null>(null);
  const [err, setErr] = useState("");
  const [copied, setCopied] = useState("");
  const load = useCallback(() => { get("keys").then((r: { keys: KeyRow[] }) => setKeys(r.keys)); }, []);
  useEffect(load, [load]);

  const base = location.origin;
  const copy = (text: string, what: string) => {
    navigator.clipboard.writeText(text).then(() => {
      setCopied(what);
      setTimeout(() => setCopied(""), 1500);
    });
  };
  const addKey = async (e: React.FormEvent) => {
    e.preventDefault();
    setErr("");
    try {
      const allowList = allow.split(",").map((s) => s.trim()).filter(Boolean);
      const r = (await post("keys", { name: name.trim(), allow: allowList })) as KeyCreated;
      setCreated(r);
      setName("");
      load();
    } catch (e2) {
      setErr(String(e2 instanceof Error ? e2.message : e2));
    }
  };
  const revoke = async (n: string) => {
    if (!confirm(`Revoke key "${n}"? Clients using it stop working immediately.`)) return;
    try { await del(`keys/${n}`); load(); } catch (e2) { alert(String(e2)); }
  };

  return (
    <div style={{ display: "grid", gap: 12 }}>
      <div className="card">
        <h3>Inbound endpoints</h3>
        <div className="key-reveal" style={{ marginBottom: 12 }}>
          <span className="mono">{base}/v1</span>
          <button className="btn sm" onClick={() => copy(base + "/v1", "base")}>{copied === "base" ? "copied" : "copy"}</button>
        </div>
        <table>
          <thead><tr><th>method</th><th>path</th><th>notes</th></tr></thead>
          <tbody>
            {ENDPOINTS.map(([m, p, note]) => (
              <tr key={p}><td className="mono">{m}</td><td className="mono">{p}</td><td className="muted">{note}</td></tr>
            ))}
          </tbody>
        </table>
        <div className="faint" style={{ marginTop: 8 }}>
          authenticate with <span className="mono">Authorization: Bearer &lt;key&gt;</span> or <span className="mono">x-api-key: &lt;key&gt;</span>
        </div>
      </div>

      <div className="card">
        <h3>API keys</h3>
        {created && (
          <div className="key-reveal" style={{ marginBottom: 12 }}>
            <span>
              new key (shown once): <span className="mono">{created.key}</span>
            </span>
            <button className="btn sm" onClick={() => copy(created.key, "key")}>{copied === "key" ? "copied" : "copy"}</button>
          </div>
        )}
        <form className="add-key" onSubmit={addKey}>
          <input placeholder="key name" value={name} required onChange={(e) => setName(e.target.value)} aria-label="key name" />
          <input placeholder="allowed models, e.g. glm-* or *" value={allow} onChange={(e) => setAllow(e.target.value)} aria-label="allowed models" />
          <button className="btn primary" type="submit" disabled={!name.trim()}>Add key</button>
        </form>
        {err && <div className="error" style={{ color: "var(--danger)", fontSize: 13, marginTop: 8 }}>{err}</div>}
        {keys.length === 0 ? <div className="empty">no keys configured</div> : (
          <table style={{ marginTop: 12 }}>
            <thead><tr><th>name</th><th>key</th><th>allowed models</th><th className="n">rpm limit</th><th></th></tr></thead>
            <tbody>
              {keys.map((k) => (
                <tr key={k.name}>
                  <td>{k.name}</td>
                  <td className="mono">{k.key_suffix}</td>
                  <td className="mono">{k.allow.join(", ")}</td>
                  <td className="n">{k.rpm > 0 ? fmtN(k.rpm) : "–"}</td>
                  <td><button className="btn sm danger" onClick={() => revoke(k.name)}>revoke</button></td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </div>
  );
}

/* ---- Providers ---- */

interface ProviderForm {
  name: string; wire: string; base_url: string; models: string;
  keys: string; dispatch_interval_ms: number;
  adaptive_thinking: boolean; inject_cache_control: boolean;
}

const EMPTY_FORM: ProviderForm = {
  name: "", wire: "openai", base_url: "", models: "", keys: "",
  dispatch_interval_ms: 0, adaptive_thinking: false, inject_cache_control: false,
};

function ProvidersTab() {
  const [provs, setProvs] = useState<ProviderRow[]>([]);
  const [form, setForm] = useState<ProviderForm | null>(null);
  const [editName, setEditName] = useState(""); // non-empty = editing this provider
  const [err, setErr] = useState("");
  const load = useCallback(() => { get("providers").then((r: { providers: ProviderRow[] }) => setProvs(r.providers)); }, []);
  useEffect(load, [load]);

  const openAdd = () => { setEditName(""); setForm({ ...EMPTY_FORM }); setErr(""); };
  const openEdit = (p: ProviderRow) => {
    setEditName(p.name);
    setForm({
      name: p.name, wire: p.wire, base_url: p.base_url, models: p.models.join(", "),
      keys: "", // blank = keep existing keys (server keeps them when omitted)
      dispatch_interval_ms: p.dispatch_interval_ms,
      adaptive_thinking: p.adaptive_thinking, inject_cache_control: p.inject_cache_control,
    });
    setErr("");
  };
  const save = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!form) return;
    setErr("");
    const body = {
      name: form.name.trim(), wire: form.wire, base_url: form.base_url.trim(),
      models: form.models.split(",").map((s) => s.trim()).filter(Boolean),
      ...(form.keys.trim() ? { keys: form.keys.split("\n").map((s) => s.trim()).filter(Boolean) } : {}),
      dispatch_interval_ms: form.dispatch_interval_ms || 0,
      adaptive_thinking: form.adaptive_thinking,
      inject_cache_control: form.inject_cache_control,
    };
    try {
      if (editName) await put(`providers/${editName}`, body);
      else await post("providers", body);
      setForm(null);
      load();
    } catch (e2) {
      setErr(String(e2 instanceof Error ? e2.message : e2));
    }
  };
  const remove = async (p: ProviderRow) => {
    if (!confirm(`Remove provider "${p.name}"? Routes pointing only at it are removed too.`)) return;
    try { await del(`providers/${p.name}`); load(); } catch (e2) { alert(String(e2)); }
  };
  const set = (k: keyof ProviderForm) => (e: React.ChangeEvent<HTMLInputElement | HTMLSelectElement | HTMLTextAreaElement>) =>
    setForm((f) => f && { ...f, [k]: e.target.type === "checkbox" ? (e.target as HTMLInputElement).checked : e.target.value });

  return (
    <div style={{ display: "grid", gap: 12 }}>
      <div style={{ display: "flex", alignItems: "center", gap: 12 }}>
        <h3 style={{ margin: 0, flex: 1 }}>Upstream providers</h3>
        <button className="btn primary" onClick={openAdd}>Add provider</button>
      </div>

      {form && (
        <form className="card" onSubmit={save} aria-label={editName ? "edit provider" : "add provider"}>
          <h3>{editName ? `Edit provider: ${editName}` : "Add provider"}</h3>
          <div className="form-grid">
            <div className="field">
              <label htmlFor="pf-name">name</label>
              <input id="pf-name" value={form.name} required disabled={!!editName} onChange={set("name")} placeholder="my-provider" />
            </div>
            <div className="field">
              <label htmlFor="pf-wire">wire</label>
              <select id="pf-wire" value={form.wire} onChange={set("wire")}>
                <option value="openai">openai</option>
                <option value="anthropic">anthropic</option>
              </select>
            </div>
            <div className="field full">
              <label htmlFor="pf-url">base URL</label>
              <input id="pf-url" value={form.base_url} required onChange={set("base_url")} placeholder="https://api.example.com/v1" />
            </div>
            <div className="field full">
              <label htmlFor="pf-models">models (comma-separated, empty = any)</label>
              <input id="pf-models" value={form.models} onChange={set("models")} placeholder="model-a, model-b" />
            </div>
            <div className="field full">
              <label htmlFor="pf-keys">API keys, one per line {editName && <span className="muted">(leave blank to keep existing)</span>}</label>
              <textarea id="pf-keys" rows={2} value={form.keys} onChange={set("keys")} placeholder="sk-…" />
            </div>
            <div className="field">
              <label htmlFor="pf-dispatch">dispatch spacing (ms, 0 = off)</label>
              <input id="pf-dispatch" type="number" min={0} value={form.dispatch_interval_ms} onChange={set("dispatch_interval_ms")} />
            </div>
            <div className="field">
              <label>options</label>
              <label className="checkbox"><input type="checkbox" checked={form.adaptive_thinking} onChange={set("adaptive_thinking")} /> adaptive thinking</label>
              <label className="checkbox"><input type="checkbox" checked={form.inject_cache_control} onChange={set("inject_cache_control")} /> inject cache control</label>
            </div>
          </div>
          {err && <div className="error" style={{ color: "var(--danger)", fontSize: 13, marginTop: 8 }}>{err}</div>}
          <div style={{ display: "flex", gap: 8, marginTop: 12 }}>
            <button className="btn primary" type="submit">{editName ? "Save changes" : "Add provider"}</button>
            <button className="btn" type="button" onClick={() => setForm(null)}>Cancel</button>
          </div>
        </form>
      )}

      {provs.map((p) => (
        <div className="card" key={p.name}>
          <div style={{ display: "flex", alignItems: "baseline", gap: 8 }}>
            <h3 style={{ flex: 1, marginBottom: 0 }}>
              {p.name} <span className="badge muted" style={{ marginLeft: 8 }}>{p.wire}</span>
              {p.auth_type === "oauth" && <span className="badge ok" style={{ marginLeft: 4 }}>oauth</span>}
            </h3>
            <button className="btn sm" onClick={() => openEdit(p)}>edit</button>
            <button className="btn sm danger" onClick={() => remove(p)}>remove</button>
          </div>
          <div className="mono faint" style={{ margin: "8px 0" }}>{p.base_url}</div>
          <div style={{ display: "flex", gap: 16, flexWrap: "wrap", fontSize: 13 }}>
            <span><span className="muted">models:</span> {p.models.length > 0 ? p.models.join(", ") : "any"}</span>
            {p.dispatch_interval_ms > 0 && (
              <span><span className="muted">dispatch spacing:</span> {p.dispatch_interval_ms}ms</span>
            )}
            {p.accounts.map((a) => (
              <span key={a.name}>
                <span className="muted">account:</span> {a.name}
                {a.disabled ? " (disabled)" : ""}
              </span>
            ))}
          </div>
        </div>
      ))}
      {provs.length === 0 && !form && <div className="empty">no providers configured — add one above</div>}
    </div>
  );
}

/* ---- Settings ---- */

function SettingsTab() {
  const [cfg, setCfg] = useState<any>(null);
  const [msg, setMsg] = useState("");
  useEffect(() => { get("config").then(setCfg); }, []);
  const reload = async () => {
    const r = await fetch("/admin/api/reload", { method: "POST" });
    setMsg(r.ok ? "config reloaded" : "reload failed");
    if (r.ok) get("config").then(setCfg);
    setTimeout(() => setMsg(""), 3000);
  };
  if (!cfg) return null;
  return (
    <div style={{ display: "grid", gap: 12 }}>
      <div className="card">
        <h3>Configuration</h3>
        <div style={{ display: "flex", gap: 12, alignItems: "center", marginBottom: 8 }}>
          <span><span className="muted">listen:</span> {cfg.listen}</span>
          <span><span className="muted">db:</span> {cfg.db_path}</span>
          <span><span className="muted">providers:</span> {cfg.providers?.length ?? 0}</span>
          <span className="spacer" style={{ flex: 1 }} />
          <button className="btn primary" onClick={reload}>Reload config</button>
        </div>
        {msg && <div className="badge ok">{msg}</div>}
        <pre className="mono" style={{ background: "var(--surface2)", padding: 12, borderRadius: "var(--radius-sm)", overflowX: "auto", fontSize: 12 }}>
{JSON.stringify(cfg, null, 2)}
        </pre>
        <div className="faint">keys are redacted. edit config.yaml on disk, then Reload. SIGHUP also reloads. dashboard edits (keys, providers) rewrite config.yaml — comments in it are not preserved.</div>
      </div>
    </div>
  );
}
