import { useState } from "react";
import { get, post, del, KeyCreated, KeyRow } from "../api";
import { useApi } from "../hooks";
import { Confirm, CopyBtn, Empty, ErrorBanner, PageHead, toast } from "../components";

const ENDPOINTS: [string, string, string][] = [
  ["POST", "/v1/chat/completions", "OpenAI wire (streaming supported)"],
  ["POST", "/v1/messages", "Anthropic wire (streaming supported)"],
  ["POST", "/v1/messages/count_tokens", "Anthropic token count"],
  ["GET", "/v1/models", "models routable by your key"],
  ["GET", "/healthz", "liveness (no auth)"],
];

export function EndpointsTab() {
  const { data, error, loading, reload } = useApi<{ keys: KeyRow[] }>("keys");
  const keys = data?.keys ?? [];
  const [name, setName] = useState("");
  const [allow, setAllow] = useState("*");
  const [created, setCreated] = useState<KeyCreated | null>(null);
  const [err, setErr] = useState("");
  const [revoking, setRevoking] = useState<KeyRow | null>(null);
  const [busy, setBusy] = useState(false);

  const base = location.origin;

  const addKey = async (e: React.FormEvent) => {
    e.preventDefault();
    setErr("");
    setBusy(true);
    try {
      const allowList = allow.split(",").map((s) => s.trim()).filter(Boolean);
      const r = (await post("keys", { name: name.trim(), allow: allowList })) as KeyCreated;
      setCreated(r);
      setName("");
      reload();
      toast(`key "${r.key.slice(0, 12)}…" created`);
    } catch (e2) {
      setErr(String(e2 instanceof Error ? e2.message : e2));
    } finally {
      setBusy(false);
    }
  };

  const revoke = async (k: KeyRow) => {
    setRevoking(null);
    try {
      await del(`keys/${k.name}`);
      reload();
      toast(`key "${k.name}" revoked`);
    } catch (e2) {
      toast(String(e2), "err");
    }
  };

  return (
    <>
      <PageHead title="Endpoints &amp; keys" desc="Point your agents here; issue and revoke dashboard API keys" />
      {error && <ErrorBanner msg={error} onRetry={reload} />}

      <div className="card">
        <h3>Inbound endpoints</h3>
        <div className="key-reveal" style={{ marginBottom: 12 }}>
          <span className="mono">{base}/v1</span>
          <CopyBtn text={base + "/v1"} what="base URL" />
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
            <CopyBtn text={created.key} what="key" />
          </div>
        )}
        <form className="add-key" onSubmit={addKey}>
          <input placeholder="key name" value={name} required onChange={(e) => setName(e.target.value)} aria-label="key name" />
          <input placeholder="allowed models, e.g. glm-* or *" value={allow} onChange={(e) => setAllow(e.target.value)} aria-label="allowed models" />
          <button className="btn primary" type="submit" disabled={!name.trim() || busy}>{busy ? "adding…" : "Add key"}</button>
        </form>
        {err && <div className="form-error">{err}</div>}
        {loading && keys.length === 0 ? <div className="faint" style={{ padding: 12 }}>loading…</div>
          : keys.length === 0 ? <Empty>no keys configured</Empty> : (
            <table style={{ marginTop: 12 }}>
              <thead><tr><th>name</th><th>key</th><th>allowed models</th><th className="n">rpm limit</th><th /></tr></thead>
              <tbody>
                {keys.map((k) => (
                  <tr key={k.name}>
                    <td>{k.name}</td>
                    <td className="mono">{k.key_suffix}</td>
                    <td className="mono">{k.allow.join(", ")}</td>
                    <td className="n">{k.rpm > 0 ? k.rpm.toLocaleString() : "–"}</td>
                    <td><button className="btn sm danger" onClick={() => setRevoking(k)}>revoke</button></td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
      </div>

      {revoking && (
        <Confirm title={`Revoke key "${revoking.name}"?`} danger action="Revoke"
          body={<>Clients using this key stop working immediately.</>}
          onDone={(ok) => { if (ok) revoke(revoking); else setRevoking(null); }} />
      )}
    </>
  );
}
