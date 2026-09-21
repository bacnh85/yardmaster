import { useEffect, useState } from "react";
import { get, put, RouteRow } from "../api";
import { ErrorBanner, PageHead, Skeleton, toast } from "../components";
import { IconX } from "../icons";

interface Cfg {
  listen: string; db_path: string; providers?: unknown[]; routes?: RouteRow[];
  routing?: { strategy?: string; rotation?: string };
}

interface RouteDraft {
  match: string; chain: string; strategy: string; weights: string;
}

const draftOf = (r: RouteRow): RouteDraft => ({
  match: r.match,
  chain: r.chain.join(", "),
  strategy: r.strategy === "weighted-rr" ? "weighted-rr" : r.strategy === "priority" ? "priority" : "",
  weights: (r.weights ?? []).join(", "),
});

const parseDraft = (d: RouteDraft): RouteRow => {
  const weights = d.weights.split(",").map((s) => parseInt(s.trim(), 10)).filter((n) => Number.isFinite(n) && n > 0);
  return {
    match: d.match.trim(),
    chain: d.chain.split(",").map((s) => s.trim()).filter(Boolean),
    ...(d.strategy === "weighted-rr" ? { strategy: "weighted-rr" } : d.strategy === "priority" ? { strategy: "priority" } : {}),
    // weights survive an inherit strategy — legal when routing.strategy is weighted-rr
    ...(weights.length > 0 ? { weights } : {}),
  };
};

export function SettingsTab() {
  const [cfg, setCfg] = useState<Cfg | null>(null);
  const [routes, setRoutes] = useState<RouteDraft[] | null>(null);
  const [gStrategy, setGStrategy] = useState("");
  const [gRotation, setGRotation] = useState("");
  const [err, setErr] = useState("");
  const [msg, setMsg] = useState("");
  const [busy, setBusy] = useState(false);

  const load = () => get("config").then((c: Cfg) => {
    setCfg(c);
    setRoutes((c.routes ?? []).map(draftOf));
    setGStrategy(c.routing?.strategy === "weighted-rr" ? "weighted-rr" : "");
    setGRotation(c.routing?.rotation === "round_robin" ? "round_robin" : "");
    setErr("");
  }).catch((e: unknown) => setErr(String(e instanceof Error ? e.message : e)));
  useEffect(() => { load(); }, []);

  const reload = async () => {
    setBusy(true);
    setMsg("");
    try {
      const r = await fetch("/admin/api/reload", { method: "POST" });
      setMsg(r.ok ? "config reloaded" : "reload failed");
      if (r.ok) load();
    } finally {
      setBusy(false);
      setTimeout(() => setMsg(""), 3000);
    }
  };

  const setRoute = (i: number, patch: Partial<RouteDraft>) =>
    setRoutes((rs) => rs && rs.map((r, j) => (j === i ? { ...r, ...patch } : r)));

  const saveRouting = async () => {
    setBusy(true);
    try {
      await put("routing", { strategy: gStrategy, rotation: gRotation });
      toast("routing defaults saved");
      await load();
    } catch (e) {
      toast(String(e instanceof Error ? e.message : e), "err");
    } finally { setBusy(false); }
  };

  const saveRoutes = async () => {
    if (!routes) return;
    setBusy(true);
    try {
      await put("routes", routes.map(parseDraft));
      toast("routes saved");
      await load();
    } catch (e) {
      toast(String(e instanceof Error ? e.message : e), "err");
    } finally { setBusy(false); }
  };

  const routesDirty = () => {
    if (!cfg || !routes) return false;
    const orig = (cfg.routes ?? []).map(draftOf);
    return JSON.stringify(orig) !== JSON.stringify(routes);
  };
  const routingDirty = () =>
    (cfg?.routing?.strategy ?? "") !== gStrategy || (cfg?.routing?.rotation ?? "") !== gRotation;
  // a route's weights matter when it opts into weighted-rr itself or inherits it globally
  const effWeighted = (r: RouteDraft) => r.strategy === "weighted-rr" || (r.strategy === "" && gStrategy === "weighted-rr");

  return (
    <>
      <PageHead title="Settings" desc="Global routing defaults, routes, live configuration (redacted), and reload" />
      {err && <ErrorBanner msg={err} onRetry={load} />}
      {!cfg && !err ? (
        <div className="card"><Skeleton h={200} w="100%" /></div>
      ) : cfg && (
        <>
          <div className="card" style={{ marginBottom: 16 }}>
            <h3>Global routing</h3>
            <div className="faint" style={{ marginBottom: 8 }}>
              defaults every route and provider inherits; per-route and per-provider settings override these.
            </div>
            <div className="form-grid">
              <div className="field">
                <label htmlFor="gr-strategy">route strategy</label>
                <select id="gr-strategy" value={gStrategy} onChange={(e) => setGStrategy(e.target.value)}>
                  <option value="">priority (default)</option>
                  <option value="weighted-rr">weighted-rr</option>
                </select>
                <div className="field-hint">priority = try chain top-down · weighted-rr = spread by weights</div>
              </div>
              <div className="field">
                <label htmlFor="gr-rotation">key rotation</label>
                <select id="gr-rotation" value={gRotation} onChange={(e) => setGRotation(e.target.value)}>
                  <option value="">first (default)</option>
                  <option value="round_robin">round robin</option>
                </select>
                <div className="field-hint">which key/account starts each request on multi-key providers</div>
              </div>
            </div>
            <div className="row" style={{ marginTop: 10 }}>
              <button className="btn primary" onClick={saveRouting} disabled={busy || !routingDirty()}>
                {busy ? "saving…" : "Save routing"}
              </button>
            </div>
          </div>

          <div className="card" style={{ marginBottom: 16 }}>
            <h3>Routes</h3>
            <div className="faint" style={{ marginBottom: 8 }}>
              first match wins; the chain is tried in order. strategy <span className="mono">inherit</span> uses the
              global default above; <span className="mono">weighted-rr</span> spreads requests across chain providers
              by weights (the rest stay as fallbacks).
            </div>
            {routes && routes.length > 0 && (
              <table className="table">
                <thead>
                  <tr><th>match</th><th>chain</th><th>strategy</th><th>weights</th><th></th></tr>
                </thead>
                <tbody>
                  {routes.map((r, i) => (
                    <tr key={i}>
                      <td>
                        <input className="mono" aria-label={`match for route ${i + 1}`} value={r.match} style={{ width: 110 }}
                          placeholder="model-*" onChange={(e) => setRoute(i, { match: e.target.value })} />
                      </td>
                      <td>
                        <input className="mono" aria-label={`chain for ${r.match || `route ${i + 1}`}`} value={r.chain} style={{ width: "100%" }}
                          placeholder="provider-a, provider-b" onChange={(e) => setRoute(i, { chain: e.target.value })} />
                      </td>
                      <td>
                        <select aria-label={`strategy for ${r.match || `route ${i + 1}`}`} value={r.strategy}
                          onChange={(e) => setRoute(i, { strategy: e.target.value })}>
                          <option value="">inherit ({gStrategy === "weighted-rr" ? "weighted-rr" : "priority"})</option>
                          <option value="priority">priority</option>
                          <option value="weighted-rr">weighted-rr</option>
                        </select>
                      </td>
                      <td>
                        {effWeighted(r) ? (
                          <input className="mono" aria-label={`weights for ${r.match || `route ${i + 1}`}`} value={r.weights} style={{ width: 90 }}
                            placeholder="3, 1" onChange={(e) => setRoute(i, { weights: e.target.value })} />
                        ) : <span className="faint">—</span>}
                      </td>
                      <td>
                        <button className="icon-btn" aria-label={`remove route ${r.match || i + 1}`} title="remove route"
                          onClick={() => setRoutes((rs) => rs && rs.filter((_, j) => j !== i))}><IconX size={14} /></button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
            {(!routes || routes.length === 0) && <div className="faint">no routes — requests resolve by provider model lists</div>}
            <div className="row" style={{ marginTop: 10 }}>
              <button className="btn" onClick={() => setRoutes((rs) => [...(rs ?? []), { match: "", chain: "", strategy: "", weights: "" }])}>
                + add route
              </button>
              <button className="btn primary" onClick={saveRoutes} disabled={busy || !routesDirty()}>
                {busy ? "saving…" : "Save routes"}
              </button>
            </div>
          </div>

          <div className="card">
            <div className="row wrap" style={{ marginBottom: 8 }}>
              <span><span className="muted">listen:</span> {cfg.listen}</span>
              <span><span className="muted">db:</span> {cfg.db_path}</span>
              <span><span className="muted">providers:</span> {cfg.providers?.length ?? 0}</span>
              <span className="spacer" />
              <button className="btn primary" onClick={reload} disabled={busy}>{busy ? "reloading…" : "Reload config"}</button>
              {msg && <span className="badge ok">{msg}</span>}
            </div>
            <pre className="mono config-pre">{JSON.stringify(cfg, null, 2)}</pre>
            <div className="faint">keys are redacted. edit config.yaml on disk, then Reload. SIGHUP also reloads. dashboard edits (keys, providers, routes) rewrite config.yaml — comments in it are not preserved.</div>
          </div>
        </>
      )}
    </>
  );
}
