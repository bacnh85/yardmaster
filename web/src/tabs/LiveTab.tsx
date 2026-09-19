import { useEffect, useState } from "react";
import { get, fmtDur, fmtMs, fmtN, LivePayload } from "../api";
import { useTick } from "../hooks";
import { Empty, PageHead } from "../components";

const BOOT = new URLSearchParams(location.hash.slice(1)); // read once: pw flow clears the hash

export function LiveTab() {
  const [live, setLive] = useState<LivePayload | null>(null);
  const [connected, setConnected] = useState(false);
  const [everConnected, setEverConnected] = useState(false);
  const [slowStart, setSlowStart] = useState(false);
  useTick(1000); // keep elapsed cells ticking

  useEffect(() => {
    // shot=1: single snapshot via fetch (for headless captures) instead of SSE
    if (BOOT.get("shot")) {
      get("summary?hours=1")
        .then((s: { inflight: number; total: number }) => {
          setLive({ active: [], inflight: s.inflight, total: s.total });
          setConnected(true);
          setEverConnected(true);
        })
        .catch(() => setConnected(false));
      return;
    }
    const es = new EventSource("/admin/api/live");
    es.onopen = () => { setConnected(true); setEverConnected(true); };
    es.onmessage = (m) => {
      try { setLive(JSON.parse(m.data)); } catch { /* partial frame — wait for the next one */ }
    };
    es.onerror = () => setConnected(false);
    return () => es.close();
  }, []);
  // "connecting…" only after a grace period — fast loads must never see a banner
  useEffect(() => {
    if (everConnected) return;
    const t = setTimeout(() => setSlowStart(true), 2500);
    return () => clearTimeout(t);
  }, [everConnected]);

  return (
    <>
      <PageHead title="Live" desc="Requests currently streaming through the proxy" />
      {!connected && everConnected && (
        <div className="reconnecting banner" role="status">connection lost — reconnecting…</div>
      )}
      {!connected && !everConnected && slowStart && (
        <div className="reconnecting banner" role="status">connecting…</div>
      )}
      {!live && connected && <Empty>waiting for data…</Empty>}
      {live && (
        <>
          <div className="grid stats">
            <div className="card stat">
              <div className="label">Active streams</div>
              <div className="value num">
                <span className={`livedot ${live.inflight > 0 ? "on" : ""}`} />
                {fmtN(live.inflight)}
              </div>
              <div className="sub">{fmtN(live.total)} requests since start</div>
            </div>
          </div>
          <div className="card" style={{ padding: 0 }}>
            <h3 className="card-pad">In flight</h3>
            {live.active.length === 0 ? (
              <Empty>no active requests</Empty>
            ) : (
              <div className="table-wrap">
                <table>
                  <thead><tr>
                    <th>model</th><th>provider</th><th>key</th>
                    <th className="n">ttft</th><th className="n">elapsed</th>
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
              </div>
            )}
          </div>
        </>
      )}
    </>
  );
}
