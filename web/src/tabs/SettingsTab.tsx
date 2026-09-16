import { useEffect, useState } from "react";
import { get } from "../api";
import { ErrorBanner, PageHead, Skeleton } from "../components";

interface Cfg { listen: string; db_path: string; providers?: unknown[] }

export function SettingsTab() {
  const [cfg, setCfg] = useState<Cfg | null>(null);
  const [err, setErr] = useState("");
  const [msg, setMsg] = useState("");
  const [busy, setBusy] = useState(false);

  const load = () => get("config").then((c: Cfg) => { setCfg(c); setErr(""); }).catch((e: unknown) => setErr(String(e instanceof Error ? e.message : e)));
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

  return (
    <>
      <PageHead title="Settings" desc="Live configuration (redacted) and reload" />
      {err && <ErrorBanner msg={err} onRetry={load} />}
      {!cfg && !err ? (
        <div className="card"><Skeleton h={200} w="100%" /></div>
      ) : cfg && (
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
          <div className="faint">keys are redacted. edit config.yaml on disk, then Reload. SIGHUP also reloads. dashboard edits (keys, providers) rewrite config.yaml — comments in it are not preserved.</div>
        </div>
      )}
    </>
  );
}
