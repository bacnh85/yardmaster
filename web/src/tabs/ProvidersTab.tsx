import { useEffect, useRef, useState } from "react";
import { post, put, del, get, ProviderRow, CatalogModel, ProbeResult, QuotaGroup, CooldownRow } from "../api";
import { useApi, usePoll } from "../hooks";
import { Confirm, Empty, ErrorBanner, Modal, PageHead, Skeleton, toast } from "../components";
import { IconEdit, IconPlay, IconX } from "../icons";
import { QuotaTable, isQuotaProvider } from "./QuotaTab";
import { REGISTRY, RegistryProvider, RegistryEntry, entryFor, wireFamily, cmdPlan, planLadder, normPlan, CmdPlan } from "../presets";

interface ProviderForm {
  name: string; wire: string; base_url: string; models: string; prefix: string;
  keys: string; session: string; rotation: string; subscription: string;
  dispatch_interval_ms: number;
  adaptive_thinking: boolean; inject_cache_control: boolean;
  zcode_signing: boolean;
  extra_headers: string; body_overrides: string; // JSON text ("" = none)
}

export const EMPTY_FORM: ProviderForm = {
  name: "", wire: "openai", base_url: "", models: "", prefix: "", keys: "", session: "", rotation: "", subscription: "",
  dispatch_interval_ms: 0, adaptive_thinking: false, inject_cache_control: false,
  zcode_signing: false, extra_headers: "", body_overrides: "",
};

const urlValid = (s: string) => {
  try { const u = new URL(s); return u.protocol === "http:" || u.protocol === "https:"; } catch { return false; }
};

const stateBadge = (state: string) =>
  state === "ok" ? "badge ok" : state === "cooldown" ? "badge warn" : state === "error" ? "badge danger" : "badge muted";

export interface ProviderBody {
  name: string; wire: string; base_url: string; models: string[]; prefix: string; session: string; rotation: string;
  subscription?: string;
  keys?: string[];
  preset?: string; disabled?: boolean;
  dispatch_interval_ms: number; adaptive_thinking: boolean; inject_cache_control: boolean;
  zcode_signing: boolean;
  extra_headers?: Record<string, string>; body_overrides?: Record<string, unknown>;
}

/** JSON-text form fields → objects; blank = omitted (server keeps stored). */
export const parseJsonField = (s: string): Record<string, unknown> => {
  if (!s.trim()) return {} as Record<string, unknown>;
  const v = JSON.parse(s); // throws → save() shows the error
  if (typeof v !== "object" || v === null || Array.isArray(v)) throw new Error("must be a JSON object");
  return v as Record<string, unknown>;
};

/** Form → API payload. Pure so tests can pin the types the Go server expects. */
export const providerBody = (form: ProviderForm): ProviderBody => ({
  name: form.name.trim(), wire: form.wire, base_url: form.base_url.trim(),
  models: form.models.split(",").map((s) => s.trim()).filter(Boolean),
  prefix: form.prefix.trim().toLowerCase(),
  session: form.session,
  rotation: form.rotation,
  subscription: form.subscription,
  ...(form.keys.trim() ? { keys: form.keys.split("\n").map((s) => s.trim()).filter(Boolean) } : {}),
  dispatch_interval_ms: Number(form.dispatch_interval_ms) || 0, // type=number inputs yield strings; Go rejects string→int
  adaptive_thinking: form.adaptive_thinking,
  inject_cache_control: form.inject_cache_control,
  zcode_signing: form.zcode_signing,
  ...(Object.keys(parseJsonField(form.extra_headers)).length ? { extra_headers: parseJsonField(form.extra_headers) as Record<string, string> } : {}),
  ...(Object.keys(parseJsonField(form.body_overrides)).length ? { body_overrides: parseJsonField(form.body_overrides) } : {}),
});

/** Row → full PUT body for provider actions (model visibility, expose-all,
 *  rotation). Round-trips EVERY editable field — the server treats omitted
 *  advanced fields as keep-stored, but sending the stored values explicitly
 *  keeps intentional flips propagating and stale partials from diverging. */
export const providerUpdateBody = (p: ProviderRow, models: string[]): ProviderBody => ({
  name: p.name, wire: p.wire, base_url: p.base_url, models,
  prefix: p.prefix ?? "", session: p.session ?? "", rotation: p.rotation ?? "",
  subscription: p.subscription ?? "",
  preset: p.preset, disabled: p.disabled,
  dispatch_interval_ms: p.dispatch_interval_ms,
  adaptive_thinking: p.adaptive_thinking, inject_cache_control: p.inject_cache_control,
  zcode_signing: p.zcode_signing ?? false,
  ...(p.extra_headers ? { extra_headers: p.extra_headers } : {}),
  ...(p.body_overrides ? { body_overrides: p.body_overrides } : {}),
});

/** GET provider row → edit-form prefill (custom providers). Session round-trips:
 *  Go PUT treats omitted = keep, "" = clear — dropping it here would silently
 *  clear session:opencode on save. */
export const rowToForm = (p: ProviderRow): ProviderForm => ({
  name: p.name, wire: p.wire, base_url: p.base_url, models: p.models.join(", "),
  prefix: p.prefix ?? "",
  keys: "", // blank = keep existing keys (server keeps them when omitted)
  session: p.session ?? "",
  rotation: p.rotation ?? "",
  subscription: p.subscription ?? "",
  dispatch_interval_ms: p.dispatch_interval_ms,
  adaptive_thinking: p.adaptive_thinking, inject_cache_control: p.inject_cache_control,
  zcode_signing: p.zcode_signing ?? false,
  extra_headers: p.extra_headers ? JSON.stringify(p.extra_headers, null, 2) : "",
  body_overrides: p.body_overrides ? JSON.stringify(p.body_overrides, null, 2) : "",
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

/** Next models list for the bulk show/hide toggle over one provider entry, or
 *  null to refuse. Hiding the last visible model yields [] — and in config an
 *  empty models list means WILDCARD (serve everything), so a "hide all" on a
 *  curated entry would silently expand it to every model. The on path can
 *  never produce [] (union with the existing list), so only off can refuse.
 *  shownIds: ids visible on screen (may span wire families — only ids inside
 *  familyIds are ever added); familyIds: the FAMILY catalog of this entry's
 *  wire — the addition universe (callers pass it tier-capped) and the
 *  complement source for wildcard hides (same semantics as the per-row box:
 *  unchecking materializes "family minus unchecked", never narrower). */
export const bulkNextModels = (cur: string[], shownIds: string[], familyIds: string[], on: boolean): string[] | null => {
  if (on) {
    if (cur.length === 0) return cur; // wildcard stays wildcard
    const universe = new Set(familyIds); // never curate outside this entry's family/tier universe
    return Array.from(new Set([...cur, ...shownIds.filter((id) => universe.has(id))]));
  }
  const next = cur.length === 0 ? familyIds.filter((id) => !shownIds.includes(id)) : cur.filter((id) => !shownIds.includes(id));
  return next.length > 0 ? next : null; // hiding the last model = wildcard: refuse
};

/** Plan-tier cap for expose-all/bulk-toggle sweeps: the stored subscription
 *  bounds what any sweep can add — the user's filter can only narrow within
 *  it (tier order goat|free < pro < max; goat/free never coexist in a group).
 *  "" on either side = uncapped by it. */
export const effectiveCap = (planFilter: "all" | CmdPlan, subscription: string): CmdPlan => {
  const tier = (p: string) => ({ goat: 0, free: 0, pro: 1, max: 2 } as Record<string, number>)[p] ?? -1;
  if (planFilter === "all") return (subscription as CmdPlan) || "";
  if (!subscription) return planFilter;
  return tier(planFilter) <= tier(subscription) ? planFilter : (subscription as CmdPlan);
};

/** Ids allowed under a plan cap: "" = all, else only that tier's models. */
export const capIds = (ids: string[], cap: CmdPlan, planOf: (id: string) => CmdPlan = cmdPlan): string[] =>
  cap === "" ? ids : ids.filter((id) => planOf(id) === cap);

/** Effective family of a catalog model within a preset group: curation wins —
 *  a model curated on a responses-wire entry is "responses" even when models.dev
 *  has no family for it yet (muse-spark, gpt-5.6-luna ship with family ""), and
 *  minimax curated on the claude entry is "anthropic" even when tagged "chat". */
export const familyFor = (group: ProviderRow[], m: { id: string; family: string }): string => {
  const curated = group.find((p) => p.models.includes(m.id));
  return curated ? wireFamily(curated.wire) : m.family;
};

/** Canonical model identity for cross-source comparison — mirrors the server's
 *  canonModelID: upstream and curated spellings may differ in case and dot vs
 *  dash notation (claude-haiku-4-5 vs claude-haiku-4.5) for the same model. */
export const canonModelId = (id: string): string => id.toLowerCase().replaceAll(".", "-");

/** Catalog rows + synthetic rows for curated ids the catalog doesn't list
 *  (manually added models). The server enriches curated ids too, so its rows
 *  already carry metadata + manual:true; this stays as a fallback for older
 *  servers that don't (rows then render "—" for unknown metadata).
 *  Ids are deduped against the catalog AND across group entries (one id curated
 *  on two wires — e.g. after a partially failed move — must yield one row, not
 *  duplicate React keys), canonically on both sides. */
export const withCurated = (catalog: CatalogModel[], group: ProviderRow[]): CatalogModel[] => {
  const known = new Set(catalog.map((m) => canonModelId(m.id)));
  const extra: CatalogModel[] = [];
  for (const p of group) {
    for (const id of p.models) {
      const key = canonModelId(id);
      if (known.has(key)) continue; // already a catalog row, or emitted for another entry
      known.add(key);
      extra.push({ id, family: wireFamily(p.wire), input: -1, output: -1, cache_read: 0, cache_write: 0, manual: true });
    }
  }
  return [...catalog, ...extra];
};

/** Curated ids are the ones actually served — they get the wire picker (the
 *  entry holding them IS their family). Catalog-only rows don't. */
export const isCuratedModel = (group: ProviderRow[], id: string): boolean =>
  group.some((p) => p.models.includes(id));

/** Next models list for one row's checkbox. Curated removal uses the entry's own
 *  list as the universe so bulkNextModels still refuses (null) when the hide
 *  would empty a curated entry ([] = wildcard serve-everything); show and
 *  wildcard-hide keep the catalog-universe semantics. The toggled id is always
 *  unioned into the show universe: a manually added id is by definition absent
 *  from the catalog, and without this its re-check would no-op (silent delete,
 *  no undo) instead of re-curating it on the row's wire. */
export const toggleModels = (sub: Pick<ProviderRow, "models">, id: string, familyIds: string[], on: boolean): string[] | null =>
  on ? bulkNextModels(sub.models, [id], familyIds.includes(id) ? familyIds : [...familyIds, id], true)
     : sub.models.includes(id)
       ? bulkNextModels(sub.models, [id], sub.models, false) // curated removal — emptying hide refuses
       : bulkNextModels(sub.models, [id], familyIds, false); // wildcard hide

export function ProvidersTab({ detail, onOpenDetail, onCloseDetail }: {
  detail: string; onOpenDetail: (id: string) => void; onCloseDetail: () => void;
}) {
  const { data, error, loading, reload } = useApi<{ providers: ProviderRow[] }>("providers");
  const cdReq = useApi<CooldownRow[]>("cooldowns");
  usePoll(cdReq.reload, 15000); // cooling badges stay fresh; noop when server is old
  const provs = data?.providers ?? [];
  const cooling = new Set((cdReq.data ?? []).map((c) => c.provider));

  if (detail) {
    const r = REGISTRY.find((x) => x.id === detail);
    // keyed by preset: a pending add-model form (or test result) must never
    // survive navigation onto another provider
    if (r) return <ProviderDetail key={r.id} r={r} provs={provs} error={error} loading={loading} reload={reload} onBack={onCloseDetail} />;
  }
  return <ProviderList provs={provs} error={error} loading={loading} reload={reload} onOpenDetail={onOpenDetail} cooling={cooling} />;
}

/* ---------------- list: registry cards + custom providers ---------------- */

function ProviderList({ provs, error, loading, reload, onOpenDetail, cooling }: {
  provs: ProviderRow[]; error: string; loading: boolean; reload: () => void; onOpenDetail: (id: string) => void; cooling: Set<string>;
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
      <CustomProviders provs={custom} reload={reload} cooling={cooling} />
      {loading && provs.length === 0 && (
        <div className="provider-grid">
          {[0, 1, 2, 3].map((i) => (
            <div key={i} className="card">
              <Skeleton h={36} w={36} />
              <div style={{ height: 12 }} />
              <Skeleton h={14} w="60%" />
              <div style={{ height: 6 }} />
              <Skeleton h={12} w="85%" />
            </div>
          ))}
        </div>
      )}
    </>
  );
}

/* ---------------- custom providers (non-registry): legacy cards + form ---------------- */

function CustomProviders({ provs, reload, cooling }: { provs: ProviderRow[]; reload: () => void; cooling: Set<string> }) {
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
            <div className="field">
              <label htmlFor="pf-rotation">key rotation</label>
              <select id="pf-rotation" value={form.rotation} onChange={set("rotation")}>
                <option value="">inherit (global default)</option>
                <option value="first">first (config order)</option>
                <option value="round_robin">round robin</option>
              </select>
            </div>
            <div className="field">
              <label htmlFor="pf-subscription">subscription plan</label>
              <select id="pf-subscription" value={form.subscription} onChange={set("subscription")}>
                <option value="">none</option>
                <option value="free">free</option>
                <option value="goat">goat</option>
                <option value="pro">pro</option>
                <option value="max">max</option>
              </select>
            </div>
            <div className="field full">
              <label htmlFor="pf-url">base URL</label>
              <input id="pf-url" value={form.base_url} required onChange={set("base_url")} placeholder="https://api.example.com/v1" aria-invalid={urlBad} />
              {urlBad && <div className="field-err">must be a valid http(s) URL</div>}
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
              {form.wire === "anthropic" && (
                <label className="checkbox"><input type="checkbox" checked={form.zcode_signing} onChange={set("zcode_signing")} /> zcode signing (z.ai)</label>
              )}
            </div>
            {form.wire === "anthropic" && (
              <>
                <div className="field">
                  <label htmlFor="pf-xheaders">extra headers (JSON, optional)</label>
                  <input id="pf-xheaders" value={form.extra_headers} onChange={set("extra_headers")} placeholder='{"anthropic-beta": "fast-mode-2026-02-01"}' />
                </div>
                <div className="field">
                  <label htmlFor="pf-bodyovr">body overrides (JSON, optional)</label>
                  <input id="pf-bodyovr" value={form.body_overrides} onChange={set("body_overrides")} placeholder='{"speed": "fast"}' />
                </div>
              </>
            )}
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
              {p.rotation === "round_robin" && <span className="badge muted">round robin</span>}
              {p.subscription && <span className="badge muted" title="curation guardrail: model lists and expose-alls cap to this plan tier">{p.subscription} plan</span>}
              {cooling.has(p.name) && <span className="badge warn" title="rate-limited; cooling down before retry">cooling</span>}
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
  const [subPlan, setSubPlan] = useState<"" | CmdPlan>("");
  const [addBusy, setAddBusy] = useState(false);
  // per-model playground result
  const [test, setTest] = useState<null | { model: string; busy: boolean; err: string; res: ProbeResult | null }>(null);
  const [catalog, setCatalog] = useState<null | { loading: boolean; err: string; models: CatalogModel[] }>(null);
  const catReq = useRef(0);
  const [familyFilter, setFamilyFilter] = useState<"all" | "exposed" | "free" | "paid">("all");
  // plan tier filter (plan-capable presets only); a stored subscription wins,
  // then the persisted choice, then the common plan — so paid-tier models never
  // sneak into expose-alls/toggle-alls on a lower-tier connection. Sweeps also
  // read the stored tier at use time (effectiveCap), so a filter narrowed wider
  // than the subscription can never lift the guardrail.
  const [planFilter, setPlanFilter] = useState<"all" | CmdPlan>(() => {
    if (!r.plans) return "all";
    const sub = normPlan(r.id, group.find((p) => p.subscription)?.subscription || "");
    if (sub) return sub;
    const v = localStorage.getItem(`plan-${r.id}`) ?? "";
    const ladder = ["all", ...planLadder(r.id)] as string[];
    return ladder.includes(v) ? (v as "all" | CmdPlan) : planLadder(r.id)[0];
  });
  // re-sync when the group's subscription changes (add-connection form, dashboard
  // edit elsewhere): the initializer above only runs at mount
  const groupSub = group.find((p) => p.subscription)?.subscription || "";
  const lastSub = useRef<string | null>(null);
  useEffect(() => {
    if (lastSub.current === null) { lastSub.current = groupSub; return; } // skip the mount run
    if (groupSub !== lastSub.current) {
      lastSub.current = groupSub;
      // a sub outside this preset's ladder (legacy goat on ollama) normalizes;
      // an unknown non-empty value filters to "all"
      if (r.plans) pickPlan(groupSub === "" ? "all" : (normPlan(r.id, groupSub) || "all"));
    }
  }, [groupSub]); // eslint-disable-line react-hooks/exhaustive-deps
  const pickPlan = (v: "all" | CmdPlan) => {
    setPlanFilter(v);
    if (r.plans) localStorage.setItem(`plan-${r.id}`, v);
  };
  const [filter, setFilter] = useState("");
  const [serveWildcard, setServeWildcard] = useState<null | { m: Pick<CatalogModel, "id">; sub: ProviderRow; from?: ProviderRow }>(null);
  const [removing, setRemoving] = useState<ConnRow | null>(null);
  const [editing, setEditing] = useState<ConnRow | null>(null);
  // manual model add: id the upstream catalog doesn't list (new/private models)
  const [manual, setManual] = useState<null | { id: string; family: string }>(null);

  const catalogSource = group.find((p) => !p.disabled) || group[0];

  // per-account usage windows (quota-capable presets only); shares the server's
  // 60s cache with the Quota tab
  const quotaSupported = group.some((p) => isQuotaProvider(p.base_url));
  const quotaReq = useApi<{ quotas: QuotaGroup[] }>(quotaSupported ? "quota" : null);
  usePoll(quotaReq.reload, 30000); // keep countdowns as fresh as the Quota tab; noop without a path
  const quota = quotaSupported
    ? quotaReq.data?.quotas.find((g) => g.providers.some((n) => group.some((p) => p.name === n)))
    : undefined;

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
            zcode_signing: false,
            ...(subPlan ? { subscription: subPlan } : {}),
            ...entry.defaults, // preset tricks (zai: cache injection, fast mode, zcode signing)
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

  /** Edit one stored connection: label, optional key rotation, plan tier.
   *  Fans out across the connection's wire entries (same shape as remove). */
  const editConnection = async (row: ConnRow, d: ConnEdit, initialPlan: string) => {
    const stored = labelWithCode(r.code, d.label.trim());
    const nextKey = d.key.trim();
    // Preflight rotation against EVERY target before the first write: the
    // server rejects a duplicate key per provider, so without this a key that
    // collides on one wire entry would rotate the others and split the
    // connection into two rows (mixed secrets). Compare tails — GET only
    // exposes "…<last6>" per connection.
    if (nextKey) {
      const tail = (s: string) => (s.startsWith("…") ? s.slice(1) : s);
      for (const { p, idx } of row.targets) {
        const clash = p.connections.find((c, i) => i !== idx &&
          (nextKey === c.suffix || (c.suffix.startsWith("…") && nextKey.endsWith(tail(c.suffix)))));
        if (clash) {
          toast(`not rotated — this key is already stored on ${p.name} as "${clash.label}"`, "err");
          return;
        }
      }
    }
    let lastErr = "";
    const rotated: string[] = [];
    const stale: string[] = [];
    for (const { p, idx } of row.targets) {
      try {
        await put(`providers/${encodeURIComponent(p.name)}/keys/${idx}`, {
          label: stored,
          ...(nextKey ? { key: nextKey } : {}),
        });
        if (nextKey) rotated.push(p.name);
      } catch (e2) {
        lastErr = String(e2 instanceof Error ? e2.message : e2);
        if (nextKey) stale.push(p.name);
      }
    }
    // plan lives on the provider entry, not the key — apply it per target entry
    if (r.plans) {
      for (const p of planTargetsToWrite(row, d.plan, initialPlan)) {
        try {
          await put(`providers/${encodeURIComponent(p.name)}/subscription`, { plan: d.plan });
        } catch (e2) { lastErr = String(e2 instanceof Error ? e2.message : e2); }
      }
    }
    if (lastErr) {
      // name the split explicitly instead of a bare error: rotated entries now
      // hold the new secret, stale ones still hold the old one
      const split = rotated.length > 0 && stale.length > 0
        ? ` — ${rotated.join(", ")} rotated, ${stale.join(", ")} still on the old key`
        : "";
      toast(`${lastErr}${split}`, "err");
    } else toast(`connection "${stored || row.suffix}" saved${nextKey ? " — key rotated" : ""}`);
    reload();
  };

  /** Key rotation override for one provider entry; the rest round-trips. */
  const setRotation = async (p: ProviderRow, rotation: string) => {
    try {
      await put(`providers/${encodeURIComponent(p.name)}`, { ...providerUpdateBody(p, p.models), rotation });
      reload();
      toast(`${p.name} key rotation: ${rotation || "inherit (global default)"}`);
    } catch (e2) { toast(String(e2 instanceof Error ? e2.message : e2), "err"); }
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
    const on = !isVisible(m);
    // curated ids need no catalog universe — toggleModels removes them against
    // the entry's own list, so a manually added model stays removable offline
    const curated = !on && sub.models.includes(m.id);
    const familyIds = (catalog?.models ?? []).filter((x) => familyFor(group, x) === familyFor(group, m)).map((x) => x.id);
    if (!curated && familyIds.length === 0) { toast("load the catalog first (refresh)", "err"); return; }
    const next = toggleModels(sub, m.id, familyIds, on);
    if (next === null) {
      toast(`cannot hide ${prefixedId(r.prefix, m.id)} — "${sub.name}" would have no visible models left (an empty list means "serve everything")`, "err");
      return;
    }
    try {
      await put(`providers/${encodeURIComponent(sub.name)}`, providerUpdateBody(sub, next));
      reload();
      // a hidden model with no catalog row leaves the table entirely (nothing
      // stays greyed out) — name the way back instead of claiming "hidden".
      // True for manual rows and for providers whose /models is unavailable
      // (the server then serves the curated list itself).
      const vanishes = !(catalog?.models ?? []).some((c) => canonModelId(c.id) === canonModelId(m.id));
      toast(`${prefixedId(r.prefix, m.id)} ${next.includes(m.id) ? "visible" : vanishes ? "removed — re-add with + add model" : "hidden"}`);
    } catch (e2) { toast(String(e2 instanceof Error ? e2.message : e2), "err"); }
  };

  /** Curate an unknown-family model onto an explicitly chosen wire. On a
   *  wildcard entry (empty models = serves everything) adding one model would
   *  silently un-serve the rest — require an explicit confirm first.
   *  Takes a bare id too: manually added models never had catalog metadata. */
  const serveOn = (m: Pick<CatalogModel, "id">, family: string, from?: ProviderRow) => {
    const entry = r.entries.find((e) => e.family === family);
    const sub = entry && group.find((p) => p.name === entry.name);
    if (!sub) { toast(`no ${family} connection to serve it on`, "err"); return; }
    const { models, confirm } = serveTargetModels(sub, m.id);
    if (confirm) { setServeWildcard({ m, sub, from }); return; }
    doServe(m, sub, models, family, from);
  };
  const doServe = async (m: Pick<CatalogModel, "id">, sub: ProviderRow, models: string[], family: string, from?: ProviderRow) => {
    try {
      // target first: a failed add leaves the model served on both wires
      // (harmless duplication) rather than lost from the source
      await put(`providers/${encodeURIComponent(sub.name)}`, providerUpdateBody(sub, models));
      if (from && from.name !== sub.name) {
        await put(`providers/${encodeURIComponent(from.name)}`, providerUpdateBody(from, from.models.filter((x) => x !== m.id)));
      }
      reload();
      toast(`${prefixedId(r.prefix, m.id)} ${from ? `moved to ${family}` : `exposed on ${family}`}`);
    } catch (e2) { toast(String(e2 instanceof Error ? e2.message : e2), "err"); }
  };

  /** Move a curated model to another wire entry. Refuses when the source would
   *  be left empty ([] = wildcard serve-everything), mirroring toggleModels. */
  const moveModel = (m: CatalogModel, family: string) => {
    const src = modelsOf(familyFor(group, m));
    if (!src || src.name === r.entries.find((e) => e.family === family)?.name) return;
    if (src.models.length <= 1) {
      toast(`cannot move ${prefixedId(r.prefix, m.id)} — "${src.name}" would have no visible models left (an empty list means "serve everything")`, "err");
      return;
    }
    serveOn(m, family, src);
  };

  /** Manually add a model id the catalog doesn't list. Curates it onto the
   *  chosen wire through the same path as "serve on…" (wildcard confirm incl.). */
  const addManualModel = (e: React.FormEvent) => {
    e.preventDefault();
    if (!manual) return;
    const id = manual.id.trim();
    if (!id) return;
    const already = group.find((p) => p.models.includes(id));
    if (already) { toast(`${prefixedId(r.prefix, id)} is already exposed on ${already.name}`, "err"); return; }
    const family = manual.family || r.entries[0]?.family || "";
    if (!r.entries.some((en) => en.family === family)) { toast("pick the wire this model serves on", "err"); return; }
    setManual(null);
    serveOn({ id }, family);
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
    // get an explicit per-row "serve on…" picker instead. Sweeps are capped by
    // the stored subscription; the plan filter can only narrow within it.
    const cap = effectiveCap(planFilter, normPlan(r.id, sub.subscription || ""));
    const ids = capIds(
      catalog.models.filter((m) => familyFor(group, m) === e.family).map((m) => m.id),
      cap,
      r.planOf,
    );
    if (ids.length === 0) { toast(`no ${e.family} models${cap ? ` on ${cap}` : ""}`, "err"); return; }
    const next = Array.from(new Set([...sub.models, ...ids]));
    try {
      await put(`providers/${encodeURIComponent(sub.name)}`, providerUpdateBody(sub, next));
      reload();
      toast(`${ids.length} ${e.family} models exposed${r.prefix ? ` as ${r.prefix}/<model>` : ""}`);
    } catch (e2) { toast(String(e2 instanceof Error ? e2.message : e2), "err"); }
  };

  const planOf = r.planOf ?? cmdPlan;
  const shown = withCurated(catalog?.models ?? [], group).filter((m) =>
    (m.manual || planFilter === "all" || planOf(m.id) === planFilter) &&
    (familyFilter === "all" || (familyFilter === "exposed" ? isVisible(m) : m.manual ? false : familyFilter === "free" ? m.free : !m.free)) &&
    (!filter || prefixedId(r.prefix, m.id).includes(filter)));
  // rows without a resolvable wire ("serve on…", no checkbox) can never be
  // toggled — the bulk toggle's state and click must ignore them, or the
  // button reads "[ ] show all" forever and clicking it looks dead
  const toggleableShown = shown.filter((m) => !!modelsOf(familyFor(group, m)));

  /** Bulk show/hide every shown model: one PUT per wire family, refusing
   *  (whole batch aborts, nothing applied) when hiding would empty any entry's
   *  curated list — an empty models list means wildcard (serve everything). */
  const toggleAll = async () => {
    if (!catalog?.models.length) { toast("load the catalog first", "err"); return; }
    const on = !(toggleableShown.length > 0 && toggleableShown.every((m) => isVisible(m)));
    // resolve every target entry first: a refusal aborts the whole batch
    const updates: { p: ProviderRow; models: string[] }[] = [];
    for (const m of shown) {
      const fam = familyFor(group, m);
      const sub = modelsOf(fam);
      if (!sub || updates.some((u) => u.p.name === sub.name)) continue; // unknown wire — per-row picker only
      // family universe: additions stay family- and tier-capped (guardrail);
      // wildcard hides materialize the UNcapped family complement — the exact
      // per-row checkbox semantics (hiding can only narrow a wildcard)
      const famIds = catalog.models.filter((x) => familyFor(group, x) === fam).map((x) => x.id);
      const universe = on ? capIds(famIds, effectiveCap(planFilter, normPlan(r.id, sub.subscription || "")), r.planOf) : famIds;
      const shownIds = shown.filter((x) => familyFor(group, x) === fam).map((x) => x.id);
      const next = bulkNextModels(sub.models, shownIds, universe, on);
      if (next === null) {
        toast(`cannot hide all — "${sub.name}" would have no visible models left (an empty list means "serve everything"). Uncheck models individually, or keep at least one.`, "err");
        return;
      }
      if (next.length !== sub.models.length || next.some((id, i) => id !== sub.models[i])) {
        updates.push({ p: sub, models: next }); // skip no-ops
      }
    }
    for (const { p, models } of updates) {
      try {
        await put(`providers/${encodeURIComponent(p.name)}`, providerUpdateBody(p, models));
      } catch (e2) { toast(String(e2 instanceof Error ? e2.message : e2), "err"); }
    }
    reload();
    if (updates.length > 0) {
      const skipped = shown.length - toggleableShown.length;
      toast(`${on ? "showing" : "hiding"} ${toggleableShown.length} models across ${updates.length} ${updates.length === 1 ? "entry" : "entries"}${skipped > 0 ? ` · ${skipped} row${skipped === 1 ? "" : "s"} need a wire (use "serve on…")` : ""}`);
    }
  };
  const allShownVisible = toggleableShown.length > 0 && toggleableShown.every((m) => isVisible(m));

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
          <button className="btn primary sm" onClick={() => { setSubPlan(normPlan(r.id, group.find((p) => p.subscription)?.subscription || "")); setAdding((a) => !a); }}>{adding ? "close" : "+ add"}</button>
        </div>
        {adding && (
          <form className="form-grid" onSubmit={addConnection} aria-label="add connection" style={{ margin: "12px 0" }}>
            <div className="field">
              <label htmlFor="conn-label">label</label>
              <input id="conn-label" value={label} onChange={(e) => setLabel(e.target.value)} placeholder="you@example.com" />
              <div className="field-hint">stored as "{labelWithCode(r.code, label.trim()) || `${r.code} your-label`}"</div>
            </div>
            <div className="field full">
              <label htmlFor="conn-key">API key</label>
              <input id="conn-key" type="password" value={key} required onChange={(e) => setKey(e.target.value)} placeholder="sk-…" />
            </div>
            {r.plans && (
              <div className="field">
                <label htmlFor="conn-subscription">subscription plan</label>
                <select id="conn-subscription" value={subPlan} onChange={(e) => setSubPlan(e.target.value as "" | CmdPlan)}>
                  <option value="">none</option>
                  {planLadder(r.id).map((p) => <option key={p} value={p}>{p}</option>)}
                </select>
              </div>
            )}
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
              <span title={`key …${row.suffix}`}>
                {row.label || "key"}
                <span className="sr-only"> key …{row.suffix}</span>
              </span>
              {row.targets.every((t) => t.p.disabled) && <span className="badge warn">disabled</span>}
              <span className="spacer" />
              <button className="btn sm" onClick={() => retest(target)} disabled={target.models.length === 0}>retest</button>
              <button className="icon-btn" aria-label={`edit connection ${row.label || row.suffix}`} title="edit label, key, plan"
                onClick={() => setEditing(row)}><IconEdit size={14} /></button>
              <button className="icon-btn" aria-label={`remove connection ${row.label || row.suffix}`} title="remove"
                onClick={() => setRemoving(row)}><IconX size={14} /></button>
            </div>
          );
        })}
      </div>

      <div className="card" style={{ marginTop: 16 }}>
        <h3>Routing</h3>
        <div className="faint" style={{ marginBottom: 8 }}>
          key rotation picks which API key/account starts each request. inherit = use the global default
          (Settings → Global routing). cooldowns apply automatically after 429s.
        </div>
        <div className="form-grid">
          {group.map((p) => (
            <div className="field" key={p.name}>
              <label htmlFor={`rot-${p.name}`}>{group.length > 1 ? p.name : "key rotation"}</label>
              <select id={`rot-${p.name}`} value={p.rotation ?? ""} onChange={(e) => setRotation(p, e.target.value)}>
                <option value="">inherit (global default)</option>
                <option value="first">first (config order)</option>
                <option value="round_robin">round robin</option>
              </select>
            </div>
          ))}
        </div>
      </div>

      {quotaSupported && quotaReq.error && (
        <div className="card" style={{ marginTop: 16 }}>
          <ErrorBanner msg={quotaReq.error} onRetry={quotaReq.reload} />
        </div>
      )}

      {quota && quota.accounts.length > 0 && (
        <div className="card" style={{ marginTop: 16 }}>
          <div className="row spread baseline">
            <h3>Usage</h3>
            <span className="faint" title="usage is cached for 60s">fetched {new Date(quota.fetched_at).toLocaleTimeString()}</span>
          </div>
          <QuotaTable accounts={quota.accounts} />
        </div>
      )}

      <div className="card" style={{ marginTop: 16 }}>
        <div className="row spread baseline">
          <h3>Available Models</h3>
          <div className="row">
            <button className="btn sm" aria-label="add model" title="expose a model the provider's catalog doesn't list"
              onClick={() => setManual((v) => (v ? null : { id: "", family: r.entries[0]?.family ?? "" }))}
              disabled={!catalogSource}>+ add model</button>
            <button className="btn sm primary" onClick={fetchCatalog} disabled={!catalogSource || catalog?.loading}>
              {catalog?.loading ? "fetching…" : catalog ? "refresh" : "load catalog"}
            </button>
          </div>
        </div>
        {manual && (
          <form className="form-grid" onSubmit={addManualModel} aria-label="add model" style={{ margin: "12px 0" }}>
            <div className="field">
              <label htmlFor="mm-id">model id</label>
              <input id="mm-id" className="mono" value={manual.id} required autoFocus
                onChange={(e) => setManual({ ...manual, id: e.target.value })}
                placeholder={r.plans ? "vendor/model-id" : "model-id"} />
              <div className="field-hint">sent upstream as typed — use the provider's catalog id{r.prefix ? `; agents see it as ${r.prefix}/<model>` : ""}</div>
            </div>
            <div className="field">
              <label htmlFor="mm-wire">serve on</label>
              <select id="mm-wire" value={manual.family}
                onChange={(e) => setManual({ ...manual, family: e.target.value })}>
                {r.entries.filter((e) => e.family !== "gemini").map((e) => (
                  <option key={e.family} value={e.family}>{e.familyLabel || e.family} ({e.wire})</option>
                ))}
              </select>
            </div>
            <div className="field full">
              <div className="row">
                <button className="btn primary" type="submit" disabled={!manual.id.trim()}>add model</button>
                <button className="btn" type="button" onClick={() => setManual(null)}>Cancel</button>
              </div>
            </div>
          </form>
        )}
        {catalog && !catalog.loading && !catalog.err && (
          <div className="toolbar">
            {r.plans && (
              <div className="seg" role="group" aria-label="plan tier filter">
                {(["all", ...planLadder(r.id)] as const).map((f) => (
                  <button key={`plan-${f}`} aria-pressed={planFilter === f}
                    title={`show only ${f === "all" ? "every" : f + "-plan"} models`}
                    onClick={() => pickPlan(f)}>{f === "all" ? "all plans" : f}</button>
                ))}
              </div>
            )}
            <div className="seg" role="group" aria-label="visibility filter">
              {(["all", "exposed", "free", "paid"] as const).map((f) => (
                <button key={f} aria-pressed={familyFilter === f} onClick={() => setFamilyFilter(f)}>{f}</button>
              ))}
            </div>
            <input className="grow" aria-label="filter models" placeholder="filter…" value={filter} onChange={(e) => setFilter(e.target.value)} />
            <span className="spacer" />
            {catalogSource && (
              <button className="btn sm" title="show or hide every model currently listed"
                disabled={toggleableShown.length === 0}
                onClick={toggleAll}>
                {allShownVisible ? "[x] hide all" : "[ ] show all"}
              </button>
            )}
            {r.entries.filter((e) => e.family !== "gemini").map((e) => (
              <button key={e.name} className="btn sm" title={`expose every ${e.familyLabel} model on ${e.name}`}
                onClick={() => exposeAll(e)}>
                expose all {e.family}
              </button>
            ))}
          </div>
        )}
        {catalog && !catalog.loading && !catalog.err && catalogSource && (
          <div className="faint" style={{ padding: "4px 0" }}>
            source: <span className="mono">{catalogSource.base_url}/models</span> · {catalog.models.length} models · {group.reduce((n, p) => n + p.models.length, 0)} exposed
          </div>
        )}
        {!catalogSource && <Empty>this provider has no connections yet — add one above, then load its catalog</Empty>}
        {catalog?.loading && <div className="faint" style={{ padding: 8 }}>fetching model catalog…</div>}
        {catalog?.err && <div className="form-error">{catalog.err}</div>}
        {/* curated rows (manual models incl.) stay reachable when the catalog
            can't load — the upstream may be down while its models still work */}
        {catalog && !catalog.loading && (!catalog.err || shown.length > 0) && (
          <>
            {shown.length === 0 ? <Empty>no models match</Empty> : (
              <div className="table-wrap" style={{ maxHeight: 480 }}>
                <table>
                  <thead>
                    <tr>
                      <th>visible</th><th>model</th><th className="col-lg">family</th>
                      <th className="num">context</th><th className="num col-md">max out</th>
                      <th className="num">$ in</th><th className="num">$ out</th><th className="num col-md">$ cache r/w</th>
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
                          <td className="col-lg">
                            {m.family === "gemini"
                              ? <span className="badge muted" title="gemini-native wire is not served yet">gemini</span>
                              : isCuratedModel(group, m.id)
                                ? (
                                  // served models: the wire is a curation choice, not
                                  // metadata — changing it MOVES the model across entries
                                  <select aria-label={`wire for ${prefixedId(r.prefix, m.id)}`} value={familyFor(group, m)}
                                    title="wire this model is served on — changing it moves the model"
                                    onChange={(e) => moveModel(m, e.target.value)} style={{ width: 110 }}>
                                    {r.entries.filter((en) => en.family !== "gemini").map((en) => (
                                      <option key={en.family} value={en.family}>{en.family}</option>
                                    ))}
                                  </select>
                                )
                                : familyFor(group, m)
                                  ? <span className="badge muted" title="not served yet — tick “visible” to expose it">{familyFor(group, m)}</span>
                                  : <select aria-label={`serve ${prefixedId(r.prefix, m.id)} on`} value=""
                                      title="models.dev has no family metadata yet — pick the wire to serve it on"
                                      onChange={(e) => e.target.value && serveOn(m, e.target.value)} style={{ width: 110 }}>
                                      <option value="">serve on…</option>
                                      {r.entries.map((en) => <option key={en.family} value={en.family}>{en.family}</option>)}
                                    </select>}
                            {m.reasoning && <span className="badge muted" title="extended thinking">think</span>}
                            {m.tool_call && <span className="badge muted" title="tool calling">tools</span>}
                            {m.image && <span className="badge muted" title="image input">img</span>}
                            {m.manual && <span className="badge muted" title="added by hand — not in the provider's catalog; metadata comes from models.dev when known">manual</span>}
                            {r.plans && !m.manual && <span className="badge muted" title={planOf(m.id) ? `${planOf(m.id)} plan` : "new model — plan tier not yet classified"}>{planOf(m.id) || "?"}</span>}
                          </td>
                          <td className="num">{m.context ? fmtTok(m.context) : "—"}</td>
                          <td className="num col-md">{m.max_output ? fmtTok(m.max_output) : "—"}</td>
                          <td className="num">{fmtPrice(m.input)}</td>
                          <td className="num">{fmtPrice(m.output)}</td>
                          <td className="num col-md">{cached ? `${fmtPrice(m.cache_read)} / ${fmtPrice(m.cache_write)}` : "—"}</td>
                          <td>
                            <button className="btn sm" disabled={!entry || test?.busy} aria-label={`test ${prefixedId(r.prefix, m.id)}`}
                              title={entry ? `test via ${entry.name}` : "unknown wire — expose the model first"}
                              onClick={() => testModel(entry, m)}>
                              <IconPlay size={12} />
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

      {removing && (
        <Confirm title={`Remove connection "${removing.label || removing.suffix}"?`} danger action="Remove"
          body={<>Requests stop rotating onto this key on {removing.targets.length === 1 ? "1 wire entry" : `${removing.targets.length} wire entries`}.</>}
          onDone={(ok) => { const r = removing; setRemoving(null); if (ok) removeConnection(r); }} />
      )}

      {editing && (
        <ConnEditModal r={r} row={editing} onSave={(d, initialPlan) => { const row = editing; setEditing(null); editConnection(row, d, initialPlan); }}
          onClose={() => setEditing(null)} />
      )}

      {serveWildcard && (
        <Confirm title={`Serve ${prefixedId(r.prefix, serveWildcard.m.id)} on ${serveWildcard.sub.name}?`} action="Serve only this model"
          body={<>{serveWildcard.sub.name} currently serves <b>every</b> model on its wire (no curated list). Serving just this one stops the others — prefer "expose all {wireFamily(serveWildcard.sub.wire)}" to keep them.</>}
          onDone={(ok) => { const { m, sub, from } = serveWildcard; setServeWildcard(null); if (ok) doServe(m, sub, serveTargetModels(sub, m.id).models, wireFamily(sub.wire), from); }} />
      )}
    </>
  );
}

interface ConnEdit { label: string; key: string; plan: string }

/** The tier a connection edit displays: the group's first non-empty
 *  subscription (mirrors the planFilter init), "" only when no target carries
 *  one. Seeding from targets[0] alone would display/clear a later entry's tier
 *  on a label-only save. */
export const connSeedPlan = (row: ConnRow): string =>
  row.targets.map((t) => t.p).find((p) => p.subscription)?.subscription ?? "";

/** Targets whose subscription a save rewrites: none when the user kept the
 *  seeded selection (a label-only save must never converge heterogeneous tiers
 *  or clear the ones it didn't display), else every target not already on the
 *  chosen tier. plan "" = user picked "none" — clears targets that had one. */
export const planTargetsToWrite = (row: ConnRow, plan: string, initialPlan: string) =>
  plan === initialPlan ? [] : row.targets.map((t) => t.p).filter((p) => (p.subscription ?? "") !== plan);

/** Edit one stored connection across the preset's wire entries: label,
 *  optional key rotation (blank = keep), and — on plan presets — the plan
 *  tier of the target provider entries. */
function ConnEditModal({ r, row, onSave, onClose }: {
  r: RegistryProvider; row: ConnRow; onSave: (d: ConnEdit, initialPlan: string) => void; onClose: () => void;
}) {
  // seed the group's first non-empty tier, RAW (not normPlan(...)): a legacy
  // cross-ladder tier (goat on ollama) must round-trip untouched on a
  // label-only save instead of being silently rewritten to free.
  const seededPlan = connSeedPlan(row);
  const [d, setD] = useState<ConnEdit>(() => ({
    label: row.label.replace(new RegExp(`^${r.code} `), ""), // strip the stored code prefix for editing
    key: "",
    plan: seededPlan,
  }));
  return (
    <Modal title={`Edit connection: ${row.label || row.suffix}`} onClose={onClose}>
      <form onSubmit={(e) => { e.preventDefault(); onSave(d, seededPlan); }} style={{ display: "grid", gap: 12 }}>
        <div className="form-grid">
          <div className="field full">
            <label htmlFor="ce-label">label</label>
            <input id="ce-label" value={d.label} autoFocus onChange={(e) => setD({ ...d, label: e.target.value })}
              placeholder="you@example.com" />
            <div className="field-hint">stored as "{labelWithCode(r.code, d.label.trim()) || `${r.code} your-label`}"</div>
          </div>
          <div className="field full">
            <label htmlFor="ce-key">replace API key <span className="muted">(leave blank to keep current)</span></label>
            <input id="ce-key" type="password" value={d.key} placeholder="sk-…" autoComplete="off"
              onChange={(e) => setD({ ...d, key: e.target.value })} />
            <div className="field-hint">the new key replaces {row.suffix} on {row.targets.length === 1 ? "this entry" : `all ${row.targets.length} wire entries`}</div>
          </div>
          {r.plans && (
            <div className="field">
              <label htmlFor="ce-plan">subscription plan</label>
              <select id="ce-plan" value={d.plan} onChange={(e) => setD({ ...d, plan: e.target.value })}>
                <option value="">none</option>
                {planLadder(r.id).map((p) => <option key={p} value={p}>{p}</option>)}
                {/* legacy cross-ladder tier (goat on ollama): keep it visible and
                    selectable, never silently remapped — picking a ladder tier
                    replaces it explicitly */}
                {seededPlan && !planLadder(r.id).includes(seededPlan as CmdPlan) && (
                  <option value={seededPlan}>{seededPlan} (legacy)</option>
                )}
              </select>
              <div className="field-hint">caps model exposure sweeps for this entry</div>
            </div>
          )}
        </div>
        <div className="row end">
          <button type="button" className="btn" onClick={onClose}>Cancel</button>
          <button className="btn primary" type="submit">
            {d.key.trim() ? "Save & rotate key" : "Save changes"}
          </button>
        </div>
      </form>
    </Modal>
  );
}
