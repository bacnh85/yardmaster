import { useCallback, useEffect, useRef, useState } from "react";
import {
  get, login, fmtDur, fmtMs, fmtN, fmtTime, fmtUSD,
  BreakdownRow, KeyRow, LivePayload, ProviderRow, ReqRow, Summary,
} from "./api";
import { TimeChart } from "./Chart";

type Tab = "live" | "usage" | "latency" | "requests" | "keys" | "providers" | "settings";
const TABS: Tab[] = ["live", "usage", "latency", "requests", "keys", "providers", "settings"];

export default function App() {
  const [authed, setAuthed] = useState<boolean | null>(null);
  const [tab, setTab] = useState<Tab>(() => {
    const t = new URLSearchParams(location.hash.slice(1)).get("tab") as Tab | null;
    return t && TABS.includes(t) ? t : "live";
  });

  // deep link: #pw=<password>&tab=<tab> (hash stays client-side; also enables
  // headless captures). pw is consumed and cleared from the hash after login.
  useEffect(() => {
    const h = new URLSearchParams(location.hash.slice(1));
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
    <>
      <header className="hdr">
        <h1>agent-router</h1>
        <div className="spacer" />
        <span className="faint">v0.1.0</span>
      </header>
      <nav className="tabs" role="tablist">
        {TABS.map((t) => (
          <button key={t} role="tab" aria-selected={tab === t} className="tab" onClick={() => setTab(t)}>
            {t}
          </button>
        ))}
      </nav>
      <main>
        {tab === "live" && <LiveTab />}
        {tab === "usage" && <UsageTab />}
        {tab === "latency" && <LatencyTab />}
        {tab === "requests" && <RequestsTab />}
        {tab === "keys" && <KeysTab />}
        {tab === "providers" && <ProvidersTab />}
        {tab === "settings" && <SettingsTab />}
      </main>
    </>
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
    if (new URLSearchParams(location.hash.slice(1)).get("shot")) {
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

/* ---- Keys ---- */

function KeysTab() {
  const [keys, setKeys] = useState<KeyRow[]>([]);
  useEffect(() => { get("keys").then((r: { keys: KeyRow[] }) => setKeys(r.keys)); }, []);
  return (
    <div className="card" style={{ padding: 0 }}>
      <table>
        <thead><tr><th>name</th><th>key</th><th>allowed models</th><th className="n">rpm limit</th></tr></thead>
        <tbody>
          {keys.map((k) => (
            <tr key={k.name}>
              <td>{k.name}</td>
              <td className="mono">{k.key_suffix}</td>
              <td className="mono">{k.allow.join(", ")}</td>
              <td className="n">{k.rpm > 0 ? fmtN(k.rpm) : "–"}</td>
            </tr>
          ))}
        </tbody>
      </table>
      {keys.length === 0 && <div className="empty">no keys configured</div>}
    </div>
  );
}

/* ---- Providers ---- */

function ProvidersTab() {
  const [provs, setProvs] = useState<ProviderRow[]>([]);
  useEffect(() => { get("providers").then((r: { providers: ProviderRow[] }) => setProvs(r.providers)); }, []);
  return (
    <div style={{ display: "grid", gap: 12 }}>
      {provs.map((p) => (
        <div className="card" key={p.name}>
          <h3>{p.name} <span className="badge muted" style={{ marginLeft: 8 }}>{p.wire}</span>
            {p.auth_type === "oauth" && <span className="badge ok" style={{ marginLeft: 4 }}>oauth</span>}</h3>
          <div className="mono faint" style={{ marginBottom: 8 }}>{p.base_url}</div>
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
      {provs.length === 0 && <div className="empty">no providers configured</div>}
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
        <div className="faint">keys are redacted. edit config.yaml on disk, then Reload. SIGHUP also reloads.</div>
      </div>
    </div>
  );
}
