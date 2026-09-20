import { useEffect, useRef, useState } from "react";
import { post, put, del, get, ProviderRow, CatalogModel, ProbeResult } from "../api";
import { useApi } from "../hooks";
import { Confirm, Empty, ErrorBanner, Modal, PageHead, toast } from "../components";
import { REGISTRY, RegistryProvider, RegistryEntry, entryFor, wireFamily, cmdPlan, CmdPlan } from "../presets";

interface ProviderForm {
  name: string; wire: string; base_url: string; models: string; prefix: string;
  keys: string; session: string;
  dispatch_interval_ms: number;
  adaptive_thinking: boolean; inject_cache_control: boolean;
}

export const EMPTY_FORM: ProviderForm = {
  name: "", wire: "openai", base_url: "", models: "", prefix: "", keys: "", session: "",
  dispatch_interval_ms: 0, adaptive_thinking: false, inject_cache_control: false,
};

const urlValid = (s: string) => {
  try { const u = new URL(s); return u.protocol === "http:" || u.protocol === "https:"; } catch { return false; }
};

const stateBadge = (state: string) =>
  state === "ok" ? "badge ok" : state === "cooldown" ? "badge warn" : state === "error" ? "badge danger" : "badge muted";

export interface ProviderBody {
  name: string; wire: string; base_url: string; models: string[]; prefix: string; session: string;
  keys?: string[];
  preset?: string; disabled?: boolean;
  dispatch_interval_ms: number; adaptive_thinking: boolean; inject_cache_control: boolean;
}

/** Form → API payload. Pure so tests can pin the types the Go server expects. */
export const providerBody = (form: ProviderForm): ProviderBody => ({
  name: form.name.trim(), wire: form.wire, base_url: form.base_url.trim(),
  models: form.models.split(",").map((s) => s.trim()).filter(Boolean),
  prefix: form.prefix.trim().toLowerCase(),
  session: form.session,
  ...(form.keys.trim() ? { keys: form.keys.split("\n").map((s) => s.trim()).filter(Boolean) } : {}),
  dispatch_interval_ms: Number(form.dispatch_interval_ms) || 0, // type=number inputs yield strings; Go rejects string→int
  adaptive_thinking: form.adaptive_thinking,
  inject_cache_control: form.inject_cache_control,
});

/** GET provider row → edit-form prefill (custom providers). Session round-trips:
 *  Go PUT treats omitted = keep, "" = clear — dropping it here would silently
 *  clear session:opencode on save. */
export const rowToForm = (p: ProviderRow): ProviderForm => ({
  name: p.name, wire: p.wire, base_url: p.base_url, models: p.models.join(", "),
  prefix: p.prefix ?? "",
  keys: "", // blank = keep existing keys (server keeps them when omitted)
  session: p.session ?? "",
  dispatch_interval_ms: p.dispatch_interval_ms,
  adaptive_thinking: p.adaptive_thinking, inject_cache_control: p.inject_cache_control,
});

// -1 = unknown pricing (models.dev has no per-token cost) — never shown as "free"
const fmtPrice = (n: number) => (n < 0 ? "—" : n === 0 ? "free" : `$${n}`);
const fmtTok = (n: number) =>
  n >= 1_000_000 ? `${(n / 1_000_000).toFixed(n % 1_000_000 === 0 ? 0 : 1)}M`
  : n >= 1_000 ? `${(n / 1_000).toFixed(n % 1_000 === 0 ? 0 : 1)}K`
  : String(n);

/** Registry card state for one registry provider: which config providers implement it. */
export const groupFor = (r: RegistryProvider, provs: ProviderRow[]): ProviderRow[] =>
  provs.filter((p) => p.preset === r.id || r.entries.some((e) => e.name === p.name));

export const connectedCount = (group: ProviderRow[]): number =>
  group.reduce((n, p) => n + (p.auth_type === "oauth" ? p.accounts.length : p.connections.length), 0);

/** Stored key label: "<code> <label>" unless blank or already prefixed (idempotent). */
export const labelWithCode = (code: string, label: string): string => {
  const t = label.trim();
  if (!t) return "";
  return t.startsWith(code + " ") ? t : `${code} ${t}`;
};

/** One UI connection row per distinct key suffix across a preset's wire entries.
 *  A suffix seen on the SAME provider twice = two distinct keys (server delete is
 *  index-addressed, server_test covers duplicate suffixes) — those never merge. */
export interface ConnRow { suffix: string; label: string; targets: { p: ProviderRow; idx: number }[] }
export const connRows = (group: ProviderRow[]): ConnRow[] => {
  const bySuffix = new Map<string, ConnRow>();
  const rows: ConnRow[] = [];
  for (const p of group) p.connections.forEach((c, idx) => {
    const row = bySuffix.get(c.suffix);
    if (row && !row.targets.some((t) => t.p.name === p.name)) {
      if (!row.label) row.label = c.label;
      row.targets.push({ p, idx });
    } else {
      const fresh: ConnRow = { suffix: c.suffix, label: c.label, targets: [{ p, idx }] };
      rows.push(fresh);
      if (!row) bySuffix.set(c.suffix, fresh);
    }
  });
  return rows;
};

/** Agent-facing id: "prefix/model" when a routing prefix is set. */
const prefixedId = (prefix: string, id: string): string => (prefix ? `${prefix}/${id}` : id);

/** Models payload for serveOn + whether it needs confirmation: appending to a
 *  WILDCARD entry (empty models = serves everything) narrows it to one model,
 *  which must never happen without an explicit confirm. */
export const serveTargetModels = (sub: Pick<ProviderRow, "models">, id: string): { models: string[]; confirm: boolean } =>
  sub.models.length === 0 ? { models: [id], confirm: true } : { models: [...sub.models, id], confirm: false };

/** Effective family of a catalog model within a preset group: curation wins —
 *  a model curated on a responses-wire entry is "responses" even when models.dev
 *  has no family for it yet (muse-spark, gpt-5.6-luna ship with family ""), and
 *  minimax curated on the claude entry is "anthropic" even when tagged "chat". */
export const familyFor = (group: ProviderRow[], m: { id: string; family: string }): string => {
  const curated = group.find((p) => p.models.includes(m.id));
  return curated ? wireFamily(curated.wire) : m.family;
};

export function ProvidersTab({ detail, onOpenDetail, onCloseDetail }: {
  detail: string; onOpenDetail: (id: string) => void; onCloseDetail: () => void;
}) {
  const { data, error, loading, reload } = useApi<{ providers: ProviderRow[] }>("providers");
  const provs = data?.providers ?? [];

  if (detail) {
    const r = REGISTRY.find((x) => x.id === detail);
    if (r) return <ProviderDetail r={r} provs={provs} error={error} loading={loading} reload={reload} onBack={onCloseDetail} />;
  }
  return <ProviderList provs={provs} error={error} loading={loading} reload={reload} onOpenDetail={onOpenDetail} />;
}

/* ---------------- list: registry cards + custom providers ---------------- */

function ProviderList({ provs, error, loading, reload, onOpenDetail }: {
  provs: ProviderRow[]; error: string; loading: boolean; reload: () => void; onOpenDetail: (id: string) => void;
}) {
  // only preset-matched providers are managed by their registry detail page;
  // legacy name-matched ones (preset "") stay here so they remain editable/deletable
  const custom = provs.filter((p) => !REGISTRY.some((r) => r.id === p.preset));
  return (
    <>
      <PageHead title="API Key Providers" desc="Add a key once and the router routes, retries, and rate-limits on your behalf.">
      </PageHead>
      {error && <ErrorBanner msg={error} onRetry={reload} />}

      <div className="provider-grid">
        {REGISTRY.map((r) => {
          const group = groupFor(r, provs);
          const conns = connectedCount(group);
          const allDisabled = group.length > 0 && group.every((p) => p.disabled);
          return (
            <button key={r.id} className="card provider-card" onClick={() => onOpenDetail(r.id)}
              aria-label={`Open ${r.title}`}>
              <div className="row spread">
                <span className="monogram" aria-hidden="true">{r.code}</span>
                <span className={conns > 0 && !allDisabled ? "livedot on" : "livedot"} />
              </div>
              <h3 className="provider-title">{r.title}</h3>
              <div className="faint provider-desc">{r.desc}</div>
              <div className="row spread baseline" style={{ marginTop: 12 }}>
                <span className={conns > 0 ? "badge ok" : "badge muted"}>
                  {conns > 0 ? `${conns} connected` : "not configured"}
                </span>
                {allDisabled && <span className="badge warn">disabled</span>}
              </div>
            </button>
          );
        })}
      </div>

      <h3 style={{ margin: "24px 0 8px" }}>Custom providers</h3>
      <CustomProviders provs={custom} reload={reload} />
      {loading && provs.length === 0 && <div className="faint" style={{ padding: 12 }}>loading…</div>}
    </>
  );
}

/* ---------------- custom providers (non-registry): legacy cards + form ---------------- */

function CustomProviders({ provs, reload }: { provs: ProviderRow[]; reload: () => void }) {
  const [form, setForm] = useState<ProviderForm | null>(null);
  const [editName, setEditName] = useState("");
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);
  const [removing, setRemoving] = useState<ProviderRow | null>(null);
  const [pg, setPg] = useState<null | { provider: string; model: string; prompt: string; busy: boolean; err: string; res: ProbeResult | null }>(null);

  const set = (k: keyof ProviderForm) => (e: React.ChangeEvent<HTMLInputElement | HTMLSelectElement | HTMLTextAreaElement>) =>
    setForm((f) => f && { ...f, [k]: e.target.type === "checkbox" ? (e.target as HTMLInputElement).checked : e.target.value });
  const urlBad = !!form && form.base_url.trim() !== "" && !urlValid(form.base_url.trim());

  const save = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!form) return;
    setErr(""); setBusy(true);
    const body = providerBody(form);
    try {
      if (editName) { await put(`providers/${encodeURIComponent(editName)}`, body); toast(`provider "${body.name}" saved`); }
      else { await post("providers", body); toast(`provider "${body.name}" added`); }
      setForm(null); reload();
    } catch (e2) {
      setErr(String(e2 instanceof Error ? e2.message : e2));
    } finally { setBusy(false); }
  };
  const remove = async (p: ProviderRow) => {
    setRemoving(null);
    try { await del(`providers/${encodeURIComponent(p.name)}`); reload(); toast(`provider "${p.name}" removed`); }
    catch (e2) { toast(String(e2), "err"); }
  };
  const openPlayground = (p: ProviderRow) =>
    setPg({ provider: p.name, model: p.models[0] ?? "", prompt: "Reply with the single word: pong", busy: false, err: "", res: null });
  const runPlayground = async () => {
    if (!pg || !pg.model || !pg.prompt.trim() || pg.busy) return;
    setPg({ ...pg, busy: true, err: "", res: null });
    try {
      const res = (await post("playground", { provider: pg.provider, model: pg.model, prompt: pg.prompt, max_tokens: 256 })) as ProbeResult;
      setPg((s) => s && { ...s, busy: false, res });
    } catch (e2) {
      setPg((s) => s && { ...s, busy: false, err: String(e2 instanceof Error ? e2.message : e2) });
    }
  };

  return (
    <>
      <button className="btn primary" style={{ marginBottom: 12 }}
        onClick={() => { setEditName(""); setForm({ ...EMPTY_FORM }); setErr(""); }}>Add custom provider</button>

      {form && (
        <form className="card" onSubmit={save} aria-label={editName ? "edit provider" : "add provider"}>
          <h3>{editName ? `Edit provider: ${editName}` : "Add custom provider"}</h3>
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
                <option value="responses">responses</option>
              </select>
            </div>
            <div className="field">
              <label htmlFor="pf-session">session headers</label>
              <select id="pf-session" value={form.session} onChange={set("session")}>
                <option value="">none</option>
                <option value="opencode">opencode</option>
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
              <label htmlFor="pf-prefix">routing prefix (optional)</label>
              <input id="pf-prefix" value={form.prefix} onChange={set("prefix")} placeholder="acme" aria-describedby="pf-prefix-hint" />
              <div className="field-hint" id="pf-prefix-hint">models are exposed to agents as prefix/model</div>
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

      {provs.map((p) => (
        <div className="card" key={p.name}>
          <div className="row spread baseline provider-head">
            <h3 className="provider-title">
              {p.name} <span className="badge muted">{p.wire}</span>
              {p.auth_type === "oauth" && <span className="badge ok">oauth</span>}
              {p.session === "opencode" && <span className="badge muted">opencode session</span>}
              {p.disabled && <span className="badge warn">disabled</span>}
            </h3>
            <div className="row">
              <button className="btn sm" onClick={() => openPlayground(p)} disabled={p.auth_type === "oauth" || p.models.length === 0}>playground</button>
              <button className="btn sm" onClick={() => { setEditName(p.name); setForm(rowToForm(p)); setErr(""); }}>edit</button>
              <button className="btn sm danger" onClick={() => setRemoving(p)}>remove</button>
            </div>
          </div>
          <div className="mono faint" style={{ margin: "8px 0" }}>{p.base_url}</div>
          <div className="provider-meta">
            <span><span className="muted">models:</span> {p.models.length > 0 ? p.models.map((m) => prefixedId(p.prefix ?? "", m)).join(", ") : "any"}</span>
            {p.accounts.map((a) => (
              <span key={a.name}>
                <span className="muted">account:</span> {a.name}
                {a.state && <span className={stateBadge(a.state)}>{a.state}</span>}
              </span>
            ))}
          </div>
        </div>
      ))}
      {provs.length === 0 && !form && <Empty>no custom providers — registry providers are configured from their cards above</Empty>}

      {removing && (
        <Confirm title={`Remove provider "${removing.name}"?`} danger action="Remove"
          body={<>Routes pointing only at this provider are removed too.</>}
          onDone={(ok) => { if (ok) remove(removing); else setRemoving(null); }} />
      )}

      {pg && (
        <Modal title={`Playground: ${pg.provider}`} onClose={() => setPg(null)} wide>
          <div className="form-grid">
            <div className="field">
              <label htmlFor="pg-model">model</label>
              <select id="pg-model" value={pg.model} onChange={(e) => setPg({ ...pg, model: e.target.value })}>
                {pg.model === "" && <option value="">(configure provider models first)</option>}
                {(provs.find((p) => p.name === pg.provider)?.models ?? []).map((m) => (
                  <option key={m} value={m}>{m}</option>
                ))}
              </select>
            </div>
            <div className="field full">
              <label htmlFor="pg-prompt">prompt</label>
              <textarea id="pg-prompt" rows={3} value={pg.prompt} onChange={(e) => setPg({ ...pg, prompt: e.target.value })} />
            </div>
          </div>
          <div className="row" style={{ marginTop: 12 }}>
            <button className="btn primary" onClick={runPlayground} disabled={pg.busy || !pg.model || !pg.prompt.trim()}>
              {pg.busy ? "running…" : "run"}
            </button>
          </div>
          {pg.err && <div className="form-error" style={{ marginTop: 12 }}>{pg.err}</div>}
          {pg.res && <PlaygroundResult res={pg.res} />}
        </Modal>
      )}
    </>
  );
}

function PlaygroundResult({ res }: { res: ProbeResult }) {
  return (
    <div style={{ marginTop: 12 }}>
      <div className="field"><label>response</label></div>
      <div className="config-pre" style={{ whiteSpace: "pre-wrap" }}>{res.text || "(empty response)"}</div>
      <div className="provider-meta" style={{ marginTop: 8 }}>
        <span><span className="muted">tokens:</span> {res.tok_in} in / {res.tok_out} out</span>
        <span><span className="muted">TTFT:</span> {res.ttft_ms}ms</span>
        <span><span className="muted">duration:</span> {res.dur_ms}ms</span>
        <span><span className="muted">est. cost:</span> ${res.cost_usd.toFixed(6)}</span>
      </div>
    </div>
  );
}

/* ---------------- registry provider detail page ---------------- */

function ProviderDetail({ r, provs, error, loading, reload, onBack }: {
  r: RegistryProvider; provs: ProviderRow[]; error: string; loading: boolean; reload: () => void; onBack: () => void;
}) {
  const group = groupFor(r, provs);
  const conns = connectedCount(group);

  // add-connection form
  const [adding, setAdding] = useState(false);
  const [label, setLabel] = useState("");
  const [key, setKey] = useState("");
  const [addBusy, setAddBusy] = useState(false);
  // per-model playground result
  const [test, setTest] = useState<null | { model: string; busy: boolean; err: string; res: ProbeResult | null }>(null);
  const [catalog, setCatalog] = useState<null | { loading: boolean; err: string; models: CatalogModel[] }>(null);
  const catReq = useRef(0);
  const [familyFilter, setFamilyFilter] = useState<"all" | "exposed" | "free" | "paid">("all");
  // plan tier filter (plan-capable presets only); persists per preset, defaults
  // to the common plan so paid-tier models never sneak into expose-alls
  const [planFilter, setPlanFilter] = useState<"all" | CmdPlan>(() => {
    if (!r.plans) return "all";
    const v = localStorage.getItem(`plan-${r.id}`);
    return v === "goat" || v === "pro" || v === "max" || v === "all" ? v : "goat";
  });
  const pickPlan = (v: "all" | CmdPlan) => {
    setPlanFilter(v);
    if (r.plans) localStorage.setItem(`plan-${r.id}`, v);
  };
  const [filter, setFilter] = useState("");
  const [serveWildcard, setServeWildcard] = useState<null | { m: CatalogModel; sub: ProviderRow }>(null);

  const catalogSource = group.find((p) => !p.disabled) || group[0];

  const fetchCatalog = () => {
    if (!catalogSource) return;
    const req = ++catReq.current;
    setCatalog({ loading: true, err: "", models: [] });
    get(`providers/${encodeURIComponent(catalogSource.name)}/models`)
      .then((d) => {
        if (req !== catReq.current) return;
        setCatalog({ loading: false, err: "", models: (d as { models: CatalogModel[] }).models });
      })
      .catch((e2) => {
        if (req !== catReq.current) return;
        setCatalog({ loading: false, err: String(e2 instanceof Error ? e2.message : e2), models: [] });
      });
  };
  // auto-load on open (and when the source connection changes); refresh button still available
  useEffect(() => { fetchCatalog(); /* eslint-disable-line react-hooks/exhaustive-deps */ }, [catalogSource?.name]);

  /** Add connection: append key+label to every provider in the group; create missing entries. */
  const addConnection = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!key.trim()) return;
    setAddBusy(true);
    try {
      const stored = labelWithCode(r.code, label);
      for (const entry of r.entries) {
        const existing = group.find((p) => p.name === entry.name);
        if (!existing) {
          await post("providers", {
            name: entry.name, wire: entry.wire, base_url: entry.base_url,
            models: [], session: entry.session ?? "", preset: r.id, prefix: r.prefix,
            dispatch_interval_ms: 0, adaptive_thinking: false, inject_cache_control: false,
            keys: [key.trim()],
            ...(stored ? { keyLabels: [stored] } : {}),
          });
        } else {
          await post(`providers/${encodeURIComponent(entry.name)}/keys`, { key: key.trim(), label: stored });
        }
      }
      toast(`connection "${stored || "key"}" added to ${r.title}`);
      setAdding(false); setLabel(""); setKey("");
      reload();
    } catch (e2) {
      reload(); // refresh the group so a retry sees entries created before the failure
      toast(String(e2 instanceof Error ? e2.message : e2), "err");
    } finally { setAddBusy(false); }
  };

  const removeConnection = async (row: ConnRow) => {
    let lastErr = "";
    for (const { p, idx } of row.targets) {
      try { await del(`providers/${encodeURIComponent(p.name)}/keys/${idx}`); }
      catch (e2) { lastErr = String(e2 instanceof Error ? e2.message : e2); }
    }
    if (lastErr) toast(lastErr, "err");
    else toast(`connection ${row.label || row.suffix} removed`);
    reload();
  };

  /** Retest: one-shot ping on this provider's first model. */
  const retest = async (p: ProviderRow) => {
    if (p.models.length === 0) { toast("no models configured for a test", "err"); return; }
    toast(`testing ${p.name} / ${p.models[0]}…`);
    try {
      const res = (await post("playground", { provider: p.name, model: p.models[0], prompt: "Reply with the single word: pong", max_tokens: 16 })) as ProbeResult;
      toast(`${p.name}: ${res.text.slice(0, 40) || "(empty)"} · ${res.tok_out} out · ${res.dur_ms}ms`);
    } catch (e2) {
      toast(String(e2 instanceof Error ? e2.message : e2).slice(0, 140), "err");
    }
  };

  /** Model visibility: id present in the family-matching sub-provider's models list. */
  const modelsOf = (family: string): ProviderRow | undefined => {
    const entry = r.entries.find((e) => e.family === family);
    return group.find((p) => p.name === entry?.name) || group.find((p) => wireFamily(p.wire) === family);
  };
  const isVisible = (m: CatalogModel): boolean => {
    const sub = modelsOf(familyFor(group, m));
    // empty models = wildcard (serves any model): every catalog row is served
    return !!sub && (sub.models.length === 0 || sub.models.includes(m.id));
  };
  const toggleVisible = async (m: CatalogModel) => {
    // no wire guess for unknown-family models: a single click could curate a
    // responses-only model onto the chat wire. "expose all <family>" collects them.
    const sub = modelsOf(familyFor(group, m));
    if (!sub) { toast("unknown wire for this model — pick one in its family column", "err"); return; }
    const wildcard = sub.models.length === 0;
    let next: string[];
    if (wildcard) {
      // unchecking a wildcard row curates to the family catalog minus that id —
      // never silently restrict an "any model" provider to a single model
      const familyIds = (catalog?.models ?? []).filter((x) => familyFor(group, x) === familyFor(group, m)).map((x) => x.id);
      if (familyIds.length === 0) { toast("load the catalog first (refresh)", "err"); return; }
      next = familyIds.filter((x) => x !== m.id);
    } else {
      next = sub.models.includes(m.id) ? sub.models.filter((x) => x !== m.id) : [...sub.models, m.id];
    }
    try {
      await put(`providers/${encodeURIComponent(sub.name)}`, {
        name: sub.name, wire: sub.wire, base_url: sub.base_url, models: next,
        prefix: sub.prefix ?? "", session: sub.session ?? "", preset: sub.preset, disabled: sub.disabled,
        dispatch_interval_ms: sub.dispatch_interval_ms,
        adaptive_thinking: sub.adaptive_thinking, inject_cache_control: sub.inject_cache_control,
      });
      reload();
      toast(`${prefixedId(r.prefix, m.id)} ${wildcard || sub.models.includes(m.id) ? "hidden" : "visible"}`);
    } catch (e2) { toast(String(e2 instanceof Error ? e2.message : e2), "err"); }
  };

  /** Curate an unknown-family model onto an explicitly chosen wire. On a
   *  wildcard entry (empty models = serves everything) adding one model would
   *  silently un-serve the rest — require an explicit confirm first. */
  const serveOn = (m: CatalogModel, family: string) => {
    const entry = r.entries.find((e) => e.family === family);
    const sub = entry && group.find((p) => p.name === entry.name);
    if (!sub) { toast(`no ${family} connection to serve it on`, "err"); return; }
    const { models, confirm } = serveTargetModels(sub, m.id);
    if (confirm) { setServeWildcard({ m, sub }); return; }
    doServe(m, sub, models, family);
  };
  const doServe = async (m: CatalogModel, sub: ProviderRow, models: string[], family: string) => {
    try {
      await put(`providers/${encodeURIComponent(sub.name)}`, {
        name: sub.name, wire: sub.wire, base_url: sub.base_url, models,
        prefix: sub.prefix ?? "", session: sub.session ?? "", preset: sub.preset, disabled: sub.disabled,
        dispatch_interval_ms: sub.dispatch_interval_ms,
        adaptive_thinking: sub.adaptive_thinking, inject_cache_control: sub.inject_cache_control,
      });
      reload();
      toast(`${prefixedId(r.prefix, m.id)} exposed on ${family}`);
    } catch (e2) { toast(String(e2 instanceof Error ? e2.message : e2), "err"); }
  };

  const testModel = async (entry: RegistryEntry | undefined, m: CatalogModel) => {
    const sub = entry ? group.find((p) => p.name === entry.name) : undefined;
    if (!sub) { toast("no provider serves this model family", "err"); return; }
    setTest({ model: m.id, busy: true, err: "", res: null });
    try {
      const res = (await post("playground", { provider: sub.name, model: m.id, prompt: "Reply with the single word: pong", max_tokens: 32 })) as ProbeResult;
      setTest({ model: m.id, busy: false, err: "", res });
    } catch (e2) {
      setTest({ model: m.id, busy: false, err: String(e2 instanceof Error ? e2.message : e2).slice(0, 200), res: null });
    }
  };

  /** Expose every catalog model of one wire family on its entry's models list. */
  const exposeAll = async (e: RegistryEntry) => {
    if (!catalog?.models.length) { toast("load the catalog first", "err"); return; }
    const sub = group.find((p) => p.name === e.name);
    if (!sub) { toast(`add a connection first — ${e.name} does not exist yet`, "err"); return; }
    // strict: unknown-family models are never swept onto a guessed wire — they
    // get an explicit per-row "serve on…" picker instead
    const ids = catalog.models
      .filter((m) => familyFor(group, m) === e.family)
      .filter((m) => planFilter === "all" || cmdPlan(m.id) === planFilter)
      .map((m) => m.id);
    if (ids.length === 0) { toast(`no ${e.family} models${planFilter !== "all" ? ` on ${planFilter}` : ""}`, "err"); return; }
    const next = Array.from(new Set([...sub.models, ...ids]));
    try {
      await put(`providers/${encodeURIComponent(sub.name)}`, {
        name: sub.name, wire: sub.wire, base_url: sub.base_url, models: next,
        prefix: sub.prefix ?? "", session: sub.session ?? "", preset: sub.preset, disabled: sub.disabled,
        dispatch_interval_ms: sub.dispatch_interval_ms,
        adaptive_thinking: sub.adaptive_thinking, inject_cache_control: sub.inject_cache_control,
      });
      reload();
      toast(`${ids.length} ${e.family} models exposed${r.prefix ? ` as ${r.prefix}/<model>` : ""}`);
    } catch (e2) { toast(String(e2 instanceof Error ? e2.message : e2), "err"); }
  };

  const shown = (catalog?.models ?? []).filter((m) =>
    (planFilter === "all" || cmdPlan(m.id) === planFilter) &&
    (familyFilter === "all" || (familyFilter === "exposed" ? isVisible(m) : familyFilter === "free" ? m.free : !m.free)) &&
    (!filter || prefixedId(r.prefix, m.id).includes(filter)));

  return (
    <>
      <PageHead title={r.title} desc={r.desc}>
        <button className="btn" onClick={onBack}>← all providers</button>
      </PageHead>
      {error && <ErrorBanner msg={error} onRetry={reload} />}
      <div className="faint" style={{ marginBottom: 12 }}>
        {connRows(group).length} connection{connRows(group).length === 1 ? "" : "s"}
        {r.prefix && <> · visible models are served as <span className="mono">{r.prefix}/&lt;model&gt;</span></>}
      </div>

      <div className="card">
        <div className="row spread baseline">
          <h3>Connections</h3>
          <button className="btn primary sm" onClick={() => setAdding((a) => !a)}>{adding ? "close" : "+ add"}</button>
        </div>
        {adding && (
          <form className="form-grid" onSubmit={addConnection} aria-label="add connection" style={{ margin: "12px 0" }}>
            <div className="field">
              <label htmlFor="conn-label">label</label>
              <input id="conn-label" value={label} onChange={(e) => setLabel(e.target.value)} placeholder="mail@bacnh.com" />
              <div className="field-hint">stored as "{r.code} mail@bacnh.com"</div>
            </div>
            <div className="field full">
              <label htmlFor="conn-key">API key</label>
              <input id="conn-key" type="password" value={key} required onChange={(e) => setKey(e.target.value)} placeholder="sk-…" />
            </div>
            <div className="field full">
              <button className="btn primary" type="submit" disabled={addBusy || !key.trim()}>
                {addBusy ? "saving…" : "add connection"}
              </button>
            </div>
          </form>
        )}
        {loading && <div className="faint" style={{ padding: 8 }}>loading…</div>}
        {!loading && conns === 0 && !adding && (
          <Empty>not configured — add an API key connection above</Empty>
        )}
        {connRows(group).map((row) => {
          const target = row.targets.map((t) => t.p).find((p) => p.models.length > 0) ?? row.targets[0].p;
          const rowKey = row.targets.map((t) => `${t.p.name}:${t.idx}`).join("|"); // unique even with duplicate suffixes
          return (
            <div key={rowKey} className="provider-meta" style={{ borderTop: "1px solid var(--line)", padding: "8px 0" }}>
              <span>{row.label || "key"}</span>
              <span className="mono faint">{row.suffix}</span>
              {row.targets.every((t) => t.p.disabled) && <span className="badge warn">disabled</span>}
              <button className="btn sm" onClick={() => retest(target)} disabled={target.models.length === 0}>retest</button>
              <button className="icon-btn" aria-label={`remove connection ${row.label}`} style={{ padding: "0 2px" }}
                onClick={() => removeConnection(row)}>✕</button>
            </div>
          );
        })}
      </div>

      <div className="card" style={{ marginTop: 16 }}>
        <div className="row spread baseline">
          <h3>Available Models</h3>
          <div className="row">
            {catalog && !catalog.loading && !catalog.err && (
              <>
                {r.plans && (["all", "goat", "pro", "max"] as const).map((f) => (
                  <button key={`plan-${f}`} className={`btn sm ${planFilter === f ? "primary" : ""}`}
                    title={`show only ${f === "all" ? "every" : f + "-plan"} models`}
                    onClick={() => pickPlan(f)}>{f === "all" ? "all plans" : f}</button>
                ))}
                {(["all", "exposed", "free", "paid"] as const).map((f) => (
                  <button key={f} className={`btn sm ${familyFilter === f ? "primary" : ""}`} onClick={() => setFamilyFilter(f)}>{f}</button>
                ))}
                <input aria-label="filter models" placeholder="filter…" value={filter} onChange={(e) => setFilter(e.target.value)} style={{ width: 140 }} />
                {r.entries.filter((e) => e.family !== "gemini").map((e) => (
                  <button key={e.name} className="btn sm" title={`expose every ${e.familyLabel} model on ${e.name}`}
                    onClick={() => exposeAll(e)}>
                    expose all {e.family}
                  </button>
                ))}
              </>
            )}
            <button className="btn sm primary" onClick={fetchCatalog} disabled={!catalogSource || catalog?.loading}>
              {catalog?.loading ? "fetching…" : catalog ? "refresh" : "load catalog"}
            </button>
          </div>
        </div>
        {catalog && !catalog.loading && !catalog.err && catalogSource && (
          <div className="faint" style={{ padding: "4px 0" }}>
            source: <span className="mono">{catalogSource.base_url}/models</span> · {catalog.models.length} models · {group.reduce((n, p) => n + p.models.length, 0)} exposed
          </div>
        )}
        {!catalogSource && <Empty>this provider has no connections yet — add one above, then load its catalog</Empty>}
        {catalog?.loading && <div className="faint" style={{ padding: 8 }}>fetching model catalog…</div>}
        {catalog?.err && <div className="form-error">{catalog.err}</div>}
        {catalog && !catalog.loading && !catalog.err && (
          <>
            {shown.length === 0 ? <Empty>no models match</Empty> : (
              <div className="table-wrap" style={{ maxHeight: 480 }}>
                <table>
                  <thead>
                    <tr>
                      <th>visible</th><th>model</th><th>family</th>
                      <th className="num">context</th><th className="num">max out</th>
                      <th className="num">$ in</th><th className="num">$ out</th><th className="num">$ cache r/w</th>
                      <th>test</th>
                    </tr>
                  </thead>
                  <tbody>
                    {shown.map((m) => {
                      const entry = r.entries.find((e) => e.family === familyFor(group, m));
                      const cached = m.cache_read > 0 || m.cache_write > 0;
                      return (
                        <tr key={m.id}>
                          <td>
                            {familyFor(group, m) === ""
                              ? <span className="faint" title="pick a wire in the family column">—</span>
                              : <input type="checkbox" aria-label={`toggle ${prefixedId(r.prefix, m.id)}`} checked={isVisible(m)} onChange={() => toggleVisible(m)} />}
                          </td>
                          <td className="mono">{prefixedId(r.prefix, m.id)}</td>
                          <td>
                            {m.family === "gemini"
                              ? <span className="badge muted" title="gemini-native wire is not served yet">gemini</span>
                              : familyFor(group, m)
                                ? <span className="badge muted">{familyFor(group, m)}</span>
                                : <select aria-label={`serve ${prefixedId(r.prefix, m.id)} on`} value=""
                                    title="models.dev has no family metadata yet — pick the wire to serve it on"
                                    onChange={(e) => e.target.value && serveOn(m, e.target.value)} style={{ width: 110 }}>
                                    <option value="">serve on…</option>
                                    {r.entries.map((en) => <option key={en.family} value={en.family}>{en.family}</option>)}
                                  </select>}
                            {m.reasoning && <span className="badge muted" title="extended thinking">think</span>}
                            {m.tool_call && <span className="badge muted" title="tool calling">tools</span>}
                            {m.image && <span className="badge muted" title="image input">img</span>}
                            {r.plans && <span className="badge muted" title={cmdPlan(m.id) ? `${cmdPlan(m.id)} plan` : "new model — plan tier not yet classified"}>{cmdPlan(m.id) || "?"}</span>}
                          </td>
                          <td className="num">{m.context ? fmtTok(m.context) : "—"}</td>
                          <td className="num">{m.max_output ? fmtTok(m.max_output) : "—"}</td>
                          <td className="num">{fmtPrice(m.input)}</td>
                          <td className="num">{fmtPrice(m.output)}</td>
                          <td className="num">{cached ? `${fmtPrice(m.cache_read)} / ${fmtPrice(m.cache_write)}` : "—"}</td>
                          <td>
                            <button className="btn sm" disabled={!entry || test?.busy} title={entry ? `test via ${entry.name}` : "unknown wire — expose the model first"}
                              onClick={() => testModel(entry, m)}>
                              ▶
                            </button>
                          </td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              </div>
            )}
            {test && (
              <div style={{ marginTop: 12 }}>
                <div className="field"><label>test: {prefixedId(r.prefix, test.model)}</label></div>
                {test.busy && <div className="faint" style={{ padding: 8 }}>running…</div>}
                {test.err && <div className="form-error">{test.err}</div>}
                {test.res && <PlaygroundResult res={test.res} />}
              </div>
            )}
          </>
        )}
        {!catalog && <div className="faint" style={{ padding: 8 }}>load the provider's model catalog to browse, hide/show, and test models.</div>}
      </div>

      {serveWildcard && (
        <Confirm title={`Serve ${prefixedId(r.prefix, serveWildcard.m.id)} on ${serveWildcard.sub.name}?`} action="Serve only this model"
          body={<>{serveWildcard.sub.name} currently serves <b>every</b> model on its wire (no curated list). Serving just this one stops the others — prefer "expose all {wireFamily(serveWildcard.sub.wire)}" to keep them.</>}
          onDone={(ok) => { const { m, sub } = serveWildcard; setServeWildcard(null); if (ok) doServe(m, sub, serveTargetModels(sub, m.id).models, wireFamily(sub.wire)); }} />
      )}
    </>
  );
}
