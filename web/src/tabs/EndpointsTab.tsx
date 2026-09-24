import { useState } from "react";
import { get, post, put, del, KeyCreated, KeyRow } from "../api";
import { useApi } from "../hooks";
import { Confirm, CopyBtn, Empty, ErrorBanner, Modal, PageHead, toast } from "../components";
import { IconPlus } from "../icons";

/** unix ms → local date-time; "—" for legacy keys (no timestamp recorded). */
const fmtWhen = (ms?: number) => (ms ? new Date(ms).toLocaleString() : "—");

const ENDPOINTS: [string, string, string][] = [
  ["POST", "/v1/chat/completions", "OpenAI wire (streaming supported)"],
  ["POST", "/v1/messages", "Anthropic wire (streaming supported)"],
  ["POST", "/v1/messages/count_tokens", "Anthropic token count"],
  ["GET", "/v1/models", "models routable by your key"],
  ["GET", "/v1/usage", "upstream provider usage for your key (?provider=<prefix>)"],
  ["GET", "/healthz", "liveness (no auth)"],
];

export function EndpointsTab() {
  const { data, error, loading, reload } = useApi<{ keys: KeyRow[] }>("keys");
  const keys = data?.keys ?? [];
  const [name, setName] = useState("");
  const [allow, setAllow] = useState("*");
  const [allowUsage, setAllowUsage] = useState(true);
  const [created, setCreated] = useState<KeyCreated | null>(null);
  const [err, setErr] = useState("");
  const [revoking, setRevoking] = useState<KeyRow | null>(null);
  const [editing, setEditing] = useState<KeyRow | null>(null);
  const [showAdd, setShowAdd] = useState(false);
  const [busy, setBusy] = useState(false);

  const base = location.origin;

  const addKey = async (e: React.FormEvent) => {
    e.preventDefault();
    setErr("");
    setBusy(true);
    try {
      const allowList = allow.split(",").map((s) => s.trim()).filter(Boolean);
      const r = (await post("keys", { name: name.trim(), allow: allowList, usage: allowUsage })) as KeyCreated;
      setCreated(r);
      setShowAdd(false);
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
      await del(`keys/${encodeURIComponent(k.name)}`);
      reload();
      toast(`key "${k.name}" revoked`);
    } catch (e2) {
      toast(String(e2), "err");
    }
  };

  const setUsage = async (k: KeyRow, usage: boolean) => {
    try {
      await put(`keys/${encodeURIComponent(k.name)}`, { usage });
      reload();
      toast(`usage ${usage ? "allowed" : "revoked"} for "${k.name}"`);
    } catch (e2) {
      toast(String(e2), "err");
    }
  };

  const saveEdit = async (k: KeyRow, d: KeyDraft) => {
    try {
      const allowList = d.allow.split(",").map((s) => s.trim()).filter(Boolean);
      await put(`keys/${encodeURIComponent(k.name)}`, {
        name: d.name.trim(),
        allow: allowList.length ? allowList : ["*"],
        rpm: Number(d.rpm) || 0,
        usage: d.usage,
        ...(d.key.trim() ? { key: d.key.trim() } : {}), // blank = keep the secret
      });
      setEditing(null);
      reload();
      toast(`key "${d.name.trim()}" saved${d.key.trim() ? " — secret rotated" : ""}`);
    } catch (e2) {
      toast(String(e2 instanceof Error ? e2.message : e2), "err");
    }
  };

  const closeAdd = () => { setShowAdd(false); setErr(""); };

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
        <div className="table-wrap">
          <table>
            <thead><tr><th>method</th><th>path</th><th>notes</th></tr></thead>
            <tbody>
              {ENDPOINTS.map(([m, p, note]) => (
                <tr key={p}><td className="mono">{m}</td><td className="mono">{p}</td><td className="muted">{note}</td></tr>
              ))}
            </tbody>
          </table>
        </div>
        <div className="faint" style={{ marginTop: 8 }}>
          authenticate with <span className="mono">Authorization: Bearer &lt;key&gt;</span> or <span className="mono">x-api-key: &lt;key&gt;</span>
        </div>
      </div>

      <div className="card">
        <div className="row spread" style={{ marginBottom: 12 }}>
          <h3 style={{ margin: 0 }}>API keys</h3>
          <button className="btn primary" onClick={() => setShowAdd(true)}>
            <IconPlus size={14} /> Add API key
          </button>
        </div>
        {created && (
          <div className="key-reveal" style={{ marginBottom: 12 }}>
            <span>
              new key (shown once): <span className="mono">{created.key}</span>
            </span>
            <CopyBtn text={created.key} what="key" />
          </div>
        )}
        {loading && keys.length === 0 ? <div className="faint" style={{ padding: 12 }}>loading…</div>
          : keys.length === 0 ? <Empty>no keys configured</Empty> : (
            <div className="table-wrap" style={{ marginTop: 12 }}>
              <table>
                <thead><tr>
                  <th>name</th><th>API key ID</th><th>key</th>
                  <th>created</th><th>last used</th>
                  <th>allowed models</th><th className="n">rpm</th><th>usage api</th><th />
                </tr></thead>
                <tbody>
                  {keys.map((k) => (
                    <tr key={k.name}>
                      <td>{k.name}</td>
                      <td className="mono" title="stable reference — sha256 of the key (first 16 bytes)">{k.id ?? "—"}</td>
                      <td className="mono">{k.key_suffix}</td>
                      <td className="faint">{fmtWhen(k.created_at)}</td>
                      <td className="faint">{fmtWhen(k.last_used)}</td>
                      <td className="mono">{k.allow.join(", ")}</td>
                      <td className="n">{k.rpm > 0 ? k.rpm.toLocaleString() : "–"}</td>
                      <td>
                        <select
                          value={k.usage === false ? "revoked" : "allowed"}
                          onChange={(e) => setUsage(k, e.target.value === "allowed")}
                          aria-label={`usage permission for ${k.name}`}
                        >
                          <option value="allowed">allowed</option>
                          <option value="revoked">revoked</option>
                        </select>
                      </td>
                      <td className="row" style={{ gap: 6 }}>
                        <button className="btn sm" aria-label={`edit key ${k.name}`} onClick={() => setEditing(k)}>edit</button>
                        <button className="btn sm danger" aria-label={`revoke key ${k.name}`} onClick={() => setRevoking(k)}>revoke</button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
      </div>

      {revoking && (
        <Confirm title={`Revoke key "${revoking.name}"?`} danger action="Revoke"
          body={<>Clients using this key stop working immediately.</>}
          onDone={(ok) => { if (ok) revoke(revoking); else setRevoking(null); }} />
      )}

      {showAdd && (
        <Modal title="Add API key" onClose={closeAdd}>
          <form onSubmit={addKey} style={{ display: "grid", gap: 12 }}>
            <div className="form-grid">
              <div className="field full">
                <label htmlFor="ak-name">name</label>
                <input id="ak-name" placeholder="e.g. pi-tui" value={name} required autoFocus
                  onChange={(e) => setName(e.target.value)} />
              </div>
              <div className="field full">
                <label htmlFor="ak-allow">allowed models</label>
                <input id="ak-allow" placeholder="glm-* or * (optional)" value={allow}
                  onChange={(e) => setAllow(e.target.value)} />
                <div className="field-hint">comma-separated globs; empty = all models</div>
              </div>
              <div className="field full">
                <label className="checkbox"><input type="checkbox" checked={allowUsage} onChange={(e) => setAllowUsage(e.target.checked)} /> allow usage reporting (GET /v1/usage)</label>
              </div>
            </div>
            {err && <div className="form-error">{err}</div>}
            <div className="row end">
              <button type="button" className="btn" onClick={closeAdd}>Cancel</button>
              <button className="btn primary" type="submit" disabled={!name.trim() || busy}>
                {busy ? "creating…" : "Create key"}
              </button>
            </div>
          </form>
        </Modal>
      )}

      {editing && (
        <KeyEdit k={editing} onSave={(d) => saveEdit(editing, d)} onClose={() => setEditing(null)} />
      )}
    </>
  );
}

interface KeyDraft { name: string; allow: string; rpm: number; usage: boolean; key: string }

/** Edit an existing inbound key: rename, scope, rate limit, usage permission,
 *  and optional secret rotation (blank key field = keep the stored secret). */
function KeyEdit({ k, onSave, onClose }: { k: KeyRow; onSave: (d: KeyDraft) => void; onClose: () => void }) {
  const [d, setD] = useState<KeyDraft>(() => ({
    name: k.name, allow: k.allow.join(", "), rpm: k.rpm, usage: k.usage !== false, key: "",
  }));
  return (
    <Modal title={`Edit key: ${k.name}`} onClose={onClose}>
      <form onSubmit={(e) => { e.preventDefault(); if (d.name.trim()) onSave(d); }} style={{ display: "grid", gap: 12 }}>
        <div className="form-grid">
          <div className="field full">
            <label htmlFor="ek-name">name</label>
            <input id="ek-name" value={d.name} required autoFocus onChange={(e) => setD({ ...d, name: e.target.value })} />
          </div>
          <div className="field full">
            <label htmlFor="ek-key">replace API key <span className="muted">(leave blank to keep current)</span></label>
            <input id="ek-key" type="password" value={d.key} placeholder="ar-…" autoComplete="off"
              onChange={(e) => setD({ ...d, key: e.target.value })} />
            <div className="field-hint">rotating a key changes its API key ID; clients must switch to the new value immediately</div>
          </div>
          <div className="field full">
            <label htmlFor="ek-allow">allowed models</label>
            <input id="ek-allow" placeholder="glm-* or * (optional)" value={d.allow}
              onChange={(e) => setD({ ...d, allow: e.target.value })} />
            <div className="field-hint">comma-separated globs; empty = all models</div>
          </div>
          <div className="field">
            <label htmlFor="ek-rpm">rpm limit</label>
            <input id="ek-rpm" type="number" min={0} value={d.rpm}
              onChange={(e) => setD({ ...d, rpm: Number(e.target.value) || 0 })} />
            <div className="field-hint">0 = unlimited</div>
          </div>
          <div className="field">
            <label>usage api</label>
            <label className="checkbox"><input type="checkbox" checked={d.usage}
              onChange={(e) => setD({ ...d, usage: e.target.checked })} /> allow GET /v1/usage</label>
          </div>
        </div>
        <div className="row end">
          <button type="button" className="btn" onClick={onClose}>Cancel</button>
          <button className="btn primary" type="submit" disabled={!d.name.trim()}>
            {d.key.trim() ? "Save & rotate key" : "Save changes"}
          </button>
        </div>
      </form>
    </Modal>
  );
}
