import { useEffect, useMemo, useState } from "react";
import { get, put, fmtN, fmtUSD, fmtMs, fmtPrice, fmtTok, type ComboRow, type ComboMemberRow, type ComboUsageRow, type ProviderRow, type CatalogModel } from "../api";
import { useApi, usePoll } from "../hooks";
import { Empty, ErrorBanner, PageHead, Skeleton, StatCard, Modal, Confirm, toast, fmtPct } from "../components";
import { copyText } from "../clipboard";

const RANGES: [number, string][] = [[1, "1h"], [24, "24h"], [168, "7d"], [720, "30d"], [2160, "90d"]];

// ---------- pure helpers (unit-tested) ----------

export interface ComboDraft {
  name: string; type: string; strategy: string;
  members: { provider: string; model: string; keys: string[]; weight: string }[];
}

export const draftOf = (c: ComboRow): ComboDraft => ({
  name: c.name,
  type: c.type === "decision" ? "decision" : "chat",
  strategy: c.strategy === "weighted-rr" ? "weighted-rr" : c.strategy === "priority" ? "priority" : "",
  members: c.members.map((m) => ({
    provider: m.provider,
    // legacy combos carry a top-level model that members defaulted to —
    // resolve it in so saving rewrites the combo into the per-member form
    model: m.model ?? c.model ?? "",
    keys: m.keys ?? [],
    weight: m.weight ? String(m.weight) : "",
  })),
});

export const parseDraft = (d: ComboDraft): ComboRow => ({
  name: d.name.trim(),
  ...(d.type === "decision" ? { type: "decision" } : {}),
  ...(d.strategy === "weighted-rr" ? { strategy: "weighted-rr" } : d.strategy === "priority" ? { strategy: "priority" } : {}),
  members: d.members.map((m) => ({
    provider: m.provider.trim(),
    ...(m.model.trim() ? { model: m.model.trim() } : {}),
    ...(m.keys.length > 0 ? { keys: m.keys } : {}),
    ...(m.weight.trim() && parseInt(m.weight, 10) > 0 ? { weight: parseInt(m.weight, 10) } : {}),
  })),
});

/** Connections (labels) a member can pin, static keys first then oauth accounts. */
export const memberAccounts = (providers: ProviderRow[] | null, provider: string): string[] => {
  const p = providers?.find((x) => x.name === provider);
  if (!p) return [];
  return [...p.connections.map((c) => c.label), ...p.accounts.map((a) => a.name)];
};

/** Model ids one provider serves (curated list; catalog fallback). */
export const providerModels = (providers: ProviderRow[], provider: string): string[] =>
  providers.find((x) => x.name === provider)?.models ?? [];

/** Providers legal for the combo type: decision → classifier-wire only, chat → the rest. */
export const providersForType = (providers: ProviderRow[], type: string): ProviderRow[] =>
  providers.filter((p) => !p.disabled && (type === "decision" ? p.wire === "classifier" : p.wire !== "classifier"));

/** Model ids one provider serves, filtered to decision models when the combo is a decision combo. */
export const memberModelsFor = (providers: ProviderRow[], provider: string, type: string, catalog: CatalogModel[]): string[] =>
  providerModels(providers, provider).filter((id) => {
    if (type !== "decision") return true;
    return catalog.find((c) => c.id === id)?.family === "classifier";
  });

/** Model select option label: "id · 128K · $0.3/$1.2" from the catalog, "—" when unknown. */
export const modelOptionLabel = (id: string, catalog: CatalogModel[]): string => {
  const m = catalog.find((c) => c.id === id);
  if (!m) return id;
  const ctx = m.context ? fmtTok(m.context) : "—";
  const price = m.input < 0 ? "—" : `${fmtPrice(m.input)}/${fmtPrice(m.output)}`;
  return `${id} · ${ctx} · ${price}`;
};

/** Metadata line for the chosen member model (catalog; unknown parts dropped). */
export const modelMetaLine = (id: string, catalog: CatalogModel[]): string => {
  const m = catalog.find((c) => c.id === id);
  if (!m) return "";
  const parts: string[] = [];
  if (m.context) parts.push(`${fmtTok(m.context)} ctx`);
  if (m.input >= 0) parts.push(`${fmtPrice(m.input)} in / ${fmtPrice(m.output)} out`);
  return parts.join(" · ");
};

export interface ComboTotals { requests: number; errors: number; failovers: number; cost: number }

/** Sum one combo's usage rows; failover share = failovers/requests. */
export const comboTotals = (rows: ComboUsageRow[]): ComboTotals =>
  rows.reduce<ComboTotals>((t, r) => ({
    requests: t.requests + r.requests,
    errors: t.errors + r.errors,
    failovers: t.failovers + r.failovers,
    cost: t.cost + r.cost,
  }), { requests: 0, errors: 0, failovers: 0, cost: 0 });

// ---------- component ----------

export function CombosTab() {
  const combosReq = useApi<{ combos: ComboRow[] }>("combos");
  const provReq = useApi<{ providers: ProviderRow[] }>("providers");
  const catReq = useApi<{ models: CatalogModel[] }>("catalog");
  const [hours, setHours] = useState(24);
  const usageReq = useApi<{ usage: ComboUsageRow[] }>(`combos/usage?hours=${hours}`);
  usePoll(usageReq.reload, 10000); // routing analytics stay live without a manual reload
  const [draft, setDraft] = useState<ComboDraft | null>(null); // open editor when non-null
  const [delName, setDelName] = useState("");
  const [busy, setBusy] = useState(false);

  const combos = combosReq.data?.combos ?? [];
  const usage = usageReq.data?.usage ?? [];
  const byCombo = useMemo(() => {
    const m = new Map<string, ComboUsageRow[]>();
    for (const r of usage) m.set(r.combo, [...(m.get(r.combo) ?? []), r]);
    return m;
  }, [usage]);

  const save = async () => {
    if (!draft) return;
    setBusy(true);
    try {
      const next = parseDraft(draft);
      if (!next.name || next.members.length === 0) throw new Error("name and at least one member are required");
      if (next.members.some((m) => !m.model)) throw new Error("every member needs a provider and a model");
      // PUT swaps the whole list: replace the edited entry (or append)
      const list = combos.some((c) => c.name === next.name)
        ? combos.map((c) => (c.name === next.name ? next : c))
        : [...combos, next];
      await put("combos", list);
      toast("combos saved");
      setDraft(null);
      combosReq.reload();
    } catch (e) {
      toast(String(e instanceof Error ? e.message : e), "err");
    } finally {
      setBusy(false);
    }
  };

  const remove = async () => {
    setBusy(true);
    try {
      await put("combos", combos.filter((c) => c.name !== delName));
      toast("combo removed");
      setDelName("");
      combosReq.reload();
    } catch (e) {
      toast(String(e instanceof Error ? e.message : e), "err");
    } finally {
      setBusy(false);
    }
  };

  const setMember = (i: number, patch: Partial<ComboDraft["members"][number]>) =>
    setDraft((d) => d && { ...d, members: d.members.map((m, j) => (j === i ? { ...m, ...patch } : m)) });

  return (
    <>
      <PageHead title="Combos" desc="Virtual pooled models: one id fanning out over several providers, with routing analytics">
        <button className="btn primary" onClick={() => setDraft({ name: "", type: "chat", strategy: "", members: [{ provider: "", model: "", keys: [], weight: "" }] })}>
          + New combo
        </button>
      </PageHead>
      {combosReq.error && <ErrorBanner msg={combosReq.error} onRetry={combosReq.reload} />}
      {/* usage polls every 10s — never let a failed tick pass as "no traffic" */}
      {usageReq.error && <ErrorBanner msg={usageReq.error} onRetry={usageReq.reload} />}

      {!combosReq.data && !combosReq.error ? (
        <div className="card"><Skeleton h={160} w="100%" /></div>
      ) : combos.length === 0 ? (
        <div className="card">
          <Empty>
            no combos yet — a combo pools models across providers behind a single <span className="mono">combo/&lt;name&gt;</span> id your agents request like any model.
          </Empty>
        </div>
      ) : (
        <>
          <div className="card" style={{ marginBottom: 16 }}>
            <table className="table">
              <thead>
                <tr><th>combo id</th><th className="col-lg">members</th><th>strategy</th><th className="n">requests</th><th className="n col-lg">failover</th><th></th></tr>
              </thead>
              <tbody>
                {combos.map((c) => {
                  const rows = byCombo.get(`combo/${c.name}`) ?? [];
                  const t = comboTotals(rows);
                  return (
                    <tr key={c.name}>
                      <td className="mono">{`combo/${c.name}`}{c.type === "decision" && <span className="badge" style={{ marginLeft: 6 }} title="decision models — advertised on /v1/systemone/models">clf</span>}</td>
                      <td className="col-lg">{c.members.map((m) => (
                        <span key={m.provider} className="mono faint" style={{ marginRight: 8, fontSize: 13 }}>{m.provider}/{m.model || "?"}</span>
                      ))}</td>
                      <td>{c.strategy === "weighted-rr" ? "weighted-rr" : "priority"}</td>
                      <td className="n">{fmtN(t.requests)}</td>
                      <td className="n col-md">{t.requests > 0 ? fmtPct(t.failovers, t.requests) : "–"}</td>
                      <td>
                        <button className="icon-btn" aria-label={`copy combo id ${c.name}`} title={`copy combo/${c.name}`} onClick={() => copyText(`combo/${c.name}`).then(() => toast("combo id copied")).catch(() => toast("could not copy combo id", "err"))}>⧉</button>
                        <button className="icon-btn" aria-label={`edit combo ${c.name}`} title="edit combo" onClick={() => setDraft(draftOf(c))}>✎</button>
                        <button className="icon-btn" aria-label={`remove combo ${c.name}`} title="remove combo" onClick={() => setDelName(c.name)}>✕</button>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>

          <div className="row" style={{ marginBottom: 12 }}>
            <div className="seg" role="group" aria-label="usage range">
              {RANGES.map(([h, label]) => (
                <button key={h} aria-pressed={hours === h} onClick={() => setHours(h)}>{label}</button>
              ))}
            </div>
          </div>

          {combos.map((c) => {
            const rows = byCombo.get(`combo/${c.name}`) ?? [];
            const t = comboTotals(rows);
            const memberModels = new Map(c.members.map((m) => [m.provider, m.model || c.model || ""]));
            return (
              <div className="card" key={c.name} style={{ marginBottom: 16 }}>
                <h3>{`combo/${c.name}`}</h3>
                <div className="grid stats">
                  <StatCard label="Requests" value={fmtN(t.requests)} />
                  <StatCard label="Failover rate" value={t.requests > 0 ? fmtPct(t.failovers, t.requests) : "–"} sub={`${fmtN(t.failovers)} skipped member tries`} />
                  <StatCard label="Errors" value={fmtN(t.errors)} tone={t.errors > 0 ? "danger" : undefined} />
                  <StatCard label="Cost" value={fmtUSD(t.cost)} />
                </div>
                {rows.length > 0 ? (
                  <table className="table">
                    <thead>
                      <tr><th>provider</th><th className="col-lg">model</th><th className="n">share</th><th className="n">requests</th><th className="n">tokens</th><th className="n">ttft p50</th><th className="n">failovers</th><th className="n">cost</th></tr>
                    </thead>
                    <tbody>
                      {rows.map((r) => {
                        const mid = memberModels.get(r.provider) || "";
                        const meta = mid ? modelMetaLine(mid, catReq.data?.models ?? []) : "";
                        return (
                        <tr key={r.provider}>
                          <td>{r.provider}</td>
                          <td className="col-lg">
                            <span className="mono" style={{ fontSize: 13 }}>{mid || "—"}</span>
                            {meta && <div className="faint" style={{ fontSize: 12 }}>{meta}</div>}
                          </td>
                          <td className="n">
                            {(() => {
                              const pct = Math.round((r.requests / Math.max(t.requests, 1)) * 100);
                              return (
                                <span className="share">
                                  <span className="quota-bar" role="img" aria-label={`${pct}% of requests`}>
                                    <span className="quota-fill" style={{ width: `${pct}%` }} />
                                  </span>
                                  {pct}%
                                </span>
                              );
                            })()}
                          </td>
                          <td className="n">{fmtN(r.requests)}</td>
                          <td className="n">{fmtN(r.tok_in + r.tok_out)}</td>
                          <td className="n">{fmtMs(r.ttft_p50_ms)}</td>
                          <td className="n">{fmtN(r.failovers)}</td>
                          <td className="n">{fmtUSD(r.cost)}</td>
                        </tr>
                        );
                      })}
                    </tbody>
                  </table>
                ) : (
                  <div className="faint">no traffic in range — point an agent at <span className="mono">{`combo/${c.name}`}</span></div>
                )}
              </div>
            );
          })}
        </>
      )}

      {draft && (
        <Modal title={combos.some((c) => c.name === draft.name) ? `Edit combo/${draft.name}` : "New combo"} onClose={() => setDraft(null)} wide>
          <div className="form-grid">
            <div className="field">
              <label htmlFor="cb-name">name</label>
              <input id="cb-name" className="mono" value={draft.name} placeholder="deepseek-v4.1-flash"
                onChange={(e) => setDraft({ ...draft, name: e.target.value })} />
              <div className="field-hint">exposed as <span className="mono">combo/{draft.name || "<name>"}</span></div>
            </div>
            <div className="field">
              <label htmlFor="cb-type">combo type</label>
              <select id="cb-type" value={draft.type}
                onChange={(e) => setDraft((d) => d && ({
                  ...d,
                  type: e.target.value,
                  // switching type prunes members whose provider can't serve it
                  // (unset members stay — they're placeholders, not illegal)
                  members: d.members.filter((m) => !m.provider || providersForType(provReq.data?.providers ?? [], e.target.value).some((p) => p.name === m.provider)),
                }))}>
                <option value="chat">chat models</option>
                <option value="decision">decision models</option>
              </select>
              <div className="field-hint">decision = System One models (Jev) over classifier providers · advertised on /v1/systemone/models</div>
            </div>
            <div className="field">
              <label htmlFor="cb-strategy">strategy</label>
              <select id="cb-strategy" value={draft.strategy} onChange={(e) => setDraft({ ...draft, strategy: e.target.value })}>
                <option value="">priority (default)</option>
                <option value="weighted-rr">weighted-rr</option>
              </select>
              <div className="field-hint">priority = member order, rest as fallbacks · weighted-rr = spread by weights</div>
            </div>
          </div>

          <h3 style={{ margin: "16px 0 8px" }}>Members</h3>
          <table className="table">
            <thead>
              <tr><th>provider</th><th>model there</th><th>keys/accounts</th><th>{draft.strategy === "weighted-rr" ? "weight" : ""}</th><th></th></tr>
            </thead>
            <tbody>
              {draft.members.map((m, i) => (
                <tr key={i}>
                  <td>
                    <select aria-label={`provider ${i + 1}`} value={m.provider}
                      onChange={(e) => setMember(i, { provider: e.target.value, keys: [] })}>
                      <option value="">—</option>
                      {providersForType(provReq.data?.providers ?? [], draft.type).map((p) => (
                        <option key={p.name} value={p.name}>{p.name}</option>
                      ))}
                    </select>
                  </td>
                  <td>
                    {(() => {
                      const cat = catReq.data?.models ?? [];
                      const models = memberModelsFor(provReq.data?.providers ?? [], m.provider, draft.type, cat);
                      return models.length > 0 ? (
                        <>
                        <select className="mono" aria-label={`model at provider ${i + 1}`} value={m.model}
                          onChange={(e) => setMember(i, { model: e.target.value })}>
                          <option value="">—</option>
                          {models.map((id) => <option key={id} value={id}>{modelOptionLabel(id, cat)}</option>)}
                        </select>
                        {m.model && (() => {
                          const meta = modelMetaLine(m.model, cat);
                          const isClf = cat.find((c) => c.id === m.model)?.family === "classifier";
                          return meta ? (
                            <div className="faint" style={{ fontSize: 12 }}>{isClf ? "clf · " : ""}{meta}</div>
                          ) : isClf ? (
                            <div className="faint" style={{ fontSize: 12 }}>clf · decision model</div>
                          ) : null;
                        })()}
                        </>
                      ) : (
                        <input className="mono" aria-label={`model at provider ${i + 1}`} value={m.model} style={{ width: 170 }}
                          placeholder="upstream model id"
                          onChange={(e) => setMember(i, { model: e.target.value })} />
                      );
                    })()}
                  </td>
                  <td>
                    {m.provider && memberAccounts(provReq.data?.providers ?? [], m.provider).length > 0 ? (
                      <div className="row wrap" style={{ gap: 8 }}>
                        {memberAccounts(provReq.data?.providers ?? [], m.provider).map((label) => (
                          <label key={label} className="row" style={{ gap: 4, fontSize: 13 }}>
                            <input type="checkbox" checked={m.keys.includes(label)}
                              onChange={(e) => setMember(i, { keys: e.target.checked ? [...m.keys, label] : m.keys.filter((k) => k !== label) })} />
                            {label}
                          </label>
                        ))}
                      </div>
                    ) : <span className="faint">all (auto)</span>}
                  </td>
                  <td>
                    {draft.strategy === "weighted-rr" ? (
                      <input className="mono" aria-label={`weight ${i + 1}`} value={m.weight} style={{ width: 60 }} placeholder="1"
                        onChange={(e) => setMember(i, { weight: e.target.value })} />
                    ) : null}
                  </td>
                  <td>
                    <button className="icon-btn" aria-label={`remove member ${i + 1}`} title="remove member"
                      onClick={() => setDraft((d) => d && { ...d, members: d.members.filter((_, j) => j !== i) })}>✕</button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          <div className="row" style={{ marginTop: 8 }}>
            <button className="btn" onClick={() => setDraft((d) => d && { ...d, members: [...d.members, { provider: "", model: "", keys: [], weight: "" }] })}>
              + add member
            </button>
            <span className="spacer" />
            <button className="btn primary" onClick={save} disabled={busy}>{busy ? "saving…" : "Save combo"}</button>
          </div>
        </Modal>
      )}

      {delName && (
        <Confirm
          title={`Remove combo/${delName}?`}
          body="Agents requesting this id will stop resolving. Config is rewritten; keys and providers are untouched."
          danger action="Remove" onDone={(ok) => { setDelName(""); if (ok) remove(); }}
        />
      )}
    </>
  );
}
