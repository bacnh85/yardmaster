import { useState } from "react";
import { post, put, del, ProviderRow } from "../api";
import { useApi } from "../hooks";
import { Confirm, Empty, ErrorBanner, PageHead, toast } from "../components";

interface ProviderForm {
  name: string; wire: string; base_url: string; models: string;
  keys: string; dispatch_interval_ms: number;
  adaptive_thinking: boolean; inject_cache_control: boolean;
}

const EMPTY_FORM: ProviderForm = {
  name: "", wire: "openai", base_url: "", models: "", keys: "",
  dispatch_interval_ms: 0, adaptive_thinking: false, inject_cache_control: false,
};

const urlValid = (s: string) => {
  try { const u = new URL(s); return u.protocol === "http:" || u.protocol === "https:"; } catch { return false; }
};

export function ProvidersTab() {
  const { data, error, loading, reload } = useApi<{ providers: ProviderRow[] }>("providers");
  const provs = data?.providers ?? [];
  const [form, setForm] = useState<ProviderForm | null>(null);
  const [editName, setEditName] = useState(""); // non-empty = editing this provider
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);
  const [removing, setRemoving] = useState<ProviderRow | null>(null);

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
    setBusy(true);
    const body = {
      name: form.name.trim(), wire: form.wire, base_url: form.base_url.trim(),
      models: form.models.split(",").map((s) => s.trim()).filter(Boolean),
      ...(form.keys.trim() ? { keys: form.keys.split("\n").map((s) => s.trim()).filter(Boolean) } : {}),
      dispatch_interval_ms: form.dispatch_interval_ms || 0,
      adaptive_thinking: form.adaptive_thinking,
      inject_cache_control: form.inject_cache_control,
    };
    try {
      if (editName) { await put(`providers/${editName}`, body); toast(`provider "${body.name}" saved`); }
      else { await post("providers", body); toast(`provider "${body.name}" added`); }
      setForm(null);
      reload();
    } catch (e2) {
      setErr(String(e2 instanceof Error ? e2.message : e2));
    } finally {
      setBusy(false);
    }
  };
  const remove = async (p: ProviderRow) => {
    setRemoving(null);
    try {
      await del(`providers/${p.name}`);
      reload();
      toast(`provider "${p.name}" removed`);
    } catch (e2) {
      toast(String(e2), "err");
    }
  };
  const set = (k: keyof ProviderForm) => (e: React.ChangeEvent<HTMLInputElement | HTMLSelectElement | HTMLTextAreaElement>) =>
    setForm((f) => f && { ...f, [k]: e.target.type === "checkbox" ? (e.target as HTMLInputElement).checked : e.target.value });

  const urlBad = !!form && form.base_url.trim() !== "" && !urlValid(form.base_url.trim());

  return (
    <>
      <PageHead title="Upstream providers" desc="LLM backends the router can dispatch to">
        <button className="btn primary" onClick={openAdd}>Add provider</button>
      </PageHead>
      {error && <ErrorBanner msg={error} onRetry={reload} />}

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
              <input id="pf-url" value={form.base_url} required onChange={set("base_url")} placeholder="https://api.example.com/v1" aria-invalid={urlBad} />
              {urlBad && <div className="field-hint">must be a valid http(s) URL</div>}
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
          {err && <div className="form-error">{err}</div>}
          <div className="row" style={{ marginTop: 12 }}>
            <button className="btn primary" type="submit" disabled={busy || urlBad}>{busy ? "saving…" : editName ? "Save changes" : "Add provider"}</button>
            <button className="btn" type="button" onClick={() => setForm(null)}>Cancel</button>
          </div>
        </form>
      )}

      {loading && provs.length === 0 ? <div className="faint" style={{ padding: 12 }}>loading…</div> : provs.map((p) => (
        <div className="card" key={p.name}>
          <div className="row spread baseline">
            <h3 className="provider-title">
              {p.name} <span className="badge muted">{p.wire}</span>
              {p.auth_type === "oauth" && <span className="badge ok">oauth</span>}
            </h3>
            <div className="row">
              <button className="btn sm" onClick={() => openEdit(p)}>edit</button>
              <button className="btn sm danger" onClick={() => setRemoving(p)}>remove</button>
            </div>
          </div>
          <div className="mono faint" style={{ margin: "8px 0" }}>{p.base_url}</div>
          <div className="provider-meta">
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
      {provs.length === 0 && !form && <Empty>no providers configured — add one above</Empty>}

      {removing && (
        <Confirm title={`Remove provider "${removing.name}"?`} danger action="Remove"
          body={<>Routes pointing only at this provider are removed too.</>}
          onDone={(ok) => { if (ok) remove(removing); else setRemoving(null); }} />
      )}
    </>
  );
}
