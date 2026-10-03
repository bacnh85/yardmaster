import { Fragment, useEffect, useRef, useState } from "react";
import { get, fmtN, fmtPrice, fmtTok, type CatalogModel, type ProviderRow } from "../api";
import { useApi } from "../hooks";
import { useSorted, cmpVals } from "../hooks";
import { Empty, ErrorBanner, PageHead, Skeleton, toast } from "../components";
import { copyText } from "../clipboard";
import { REGISTRY, registryFor, wireFamily } from "../presets";
import { Playground, type PgTarget } from "../Playground";
import { IconList, IconPlay } from "../icons";
import { canonModelId } from "./ProvidersTab";

export interface CatalogRowUI {
  id: string; name: string; family: string;
  context: number; max_output: number;   // 0 = unknown
  input: number; output: number; cache_read: number; // -1 = unknown price
  reasoning: boolean; tool_call: boolean; image: boolean; free: boolean; manual: boolean;
  providers: { name: string; prefix: string; exposed: boolean; served_as: string;
               input: number; output: number; cache_read: number }[];
  combos: string[]; // combo/<name> ids pooling this model
}

/** Server CatalogRow → UI row: null out unknown prices so cmpVals sorts them
 *  last (BreakTable precedent), drop unknown context/max to 0 → "—". */
export const toRow = (m: CatalogModel): CatalogRowUI => ({
  id: m.id, name: m.name ?? "", family: m.family,
  context: m.context ?? 0, max_output: m.max_output ?? 0,
  input: m.input, output: m.output, cache_read: m.cache_read,
  reasoning: !!m.reasoning, tool_call: !!m.tool_call, image: !!m.image,
  free: !!m.free, manual: !!m.manual,
  providers: (m.providers ?? []).map((p) => ({
    name: p.name, prefix: p.prefix ?? "", exposed: p.exposed, served_as: p.served_as,
    input: p.input ?? -1, output: p.output ?? -1, cache_read: p.cache_read ?? -1,
  })),
  combos: m.combos ?? [],
});

/** Every routable id for a row: served_as per provider + combo ids. Deduped —
 *  a bare id on an unprefixed provider and the same served id twice collapse. */
export const servedIds = (m: CatalogRowUI): string[] =>
  [...new Set([...m.providers.filter((p) => p.exposed).map((p) => p.served_as), ...m.combos])];

/** Display name: strip the vendor namespace prefix ("deepseek/deepseek-v4-flash"
 *  → "deepseek-v4-flash") — the providers column already says who serves it.
 *  Display only; the full id stays in the tooltip and is what copy copies. */
export const displayName = (id: string): string =>
  id.includes("/") ? id.slice(id.indexOf("/") + 1) : id;

/** Price spread across the row's EXPOSED providers: null when none (or one,
 *  or all equal — nothing worth expanding); else [min, max] over each price
 *  class. Known prices only; unknown (-1) never widens the range. */
export const priceSpread = (m: CatalogRowUI): [number, number] | null => {
  const provs = m.providers.filter((p) => p.exposed);
  if (provs.length <= 1) return null;
  const range = (get: (p: CatalogRowUI["providers"][number]) => number): [number, number] | null => {
    const vals = provs.map(get).filter((v) => v >= 0);
    if (vals.length <= 1) return null;
    const min = Math.min(...vals), max = Math.max(...vals);
    return min === max ? null : [min, max];
  };
  return range((p) => p.input) ?? range((p) => p.output) ?? range((p) => p.cache_read);
};

export const sortPrice = (n: number): number | null => (n < 0 ? null : n);

/** Filter chain (pure, unit-tested): text substring on id+name+served ids
 *  (so "nv/" or "combo/" find their rows), provider name or registry title,
 *  price class, capability flags, exposed-only toggle. exposedOnly drops
 *  unexposed provider ENTRIES from the returned rows — a model exposed on one
 *  provider must not list/badge under another where it's catalog-only. */
export const filterRows = (rows: CatalogRowUI[], f: Filters): CatalogRowUI[] => {
  const out: CatalogRowUI[] = [];
  for (const m of rows) {
    const provs = f.exposedOnly ? m.providers.filter((p) => p.exposed) : m.providers;
    const served = servedIds({ ...m, providers: provs });
    if (
    (!f.text || m.id.toLowerCase().includes(f.text) || m.name.toLowerCase().includes(f.text) ||
      served.some((s) => s.toLowerCase().includes(f.text))) &&
    (f.provider === "all" ||
      provs.some((p) => p.name === f.provider || registryFor({ preset: "", name: p.name })?.id === f.provider)) &&
    (f.price === "all" || (f.price === "free" ? m.free && !sortPrice(m.input) : !m.free && sortPrice(m.input) !== null)) &&
    (!f.reasoning || m.reasoning) &&
    (!f.tool_call || m.tool_call) &&
    (!f.image || m.image) &&
    (!f.exposedOnly || m.providers.some((p) => p.exposed))
    ) out.push(provs.length !== m.providers.length ? { ...m, providers: provs } : m);
  }
  return out;
};

export interface Filters {
  text: string; provider: string; price: "all" | "free" | "paid";
  reasoning: boolean; tool_call: boolean; image: boolean; exposedOnly: boolean;
}

export const DEFAULT_FILTERS: Filters = {
  text: "", provider: "all", price: "all",
  reasoning: false, tool_call: false, image: false, exposedOnly: true,
};

/** Lazy render window: initial rows ≈ one viewport, then +60 per sentinel hit. */
export const initialLimit = () => Math.ceil(window.innerHeight / 36) + 10;
export const PAGE = 60;

export function ModelsTab() {
  const cat = useApi<{ models: CatalogModel[] }>("catalog");
  const provReq = useApi<{ providers: ProviderRow[] }>("providers");
  const [filters, setFilters] = useState<Filters>(DEFAULT_FILTERS);
  const [limit, setLimit] = useState(initialLimit);
  const sentinel = useRef<HTMLDivElement>(null);
  const pgRef = useRef<HTMLDivElement>(null);
  // playground state: selected model + the provider (key source) serving it
  const [pg, setPg] = useState<null | { model: string; provider: string }>(null);
  // rows expanded to per-provider pricing (ids survive lazy-load pagination)
  const [expanded, setExpanded] = useState<Set<string>>(new Set());

  const provs = provReq.data?.providers ?? [];
  const rows = (cat.data?.models ?? []).map(toRow);
  const filtered = filterRows(rows, filters);
  const sort = useSorted(filtered, "id", 1);
  const visible = sort.sorted.slice(0, limit);

  // sentinel loads the next page when scrolled into view; resets on filter/sort change.
  // Re-armed when the table appears/disappears: the sentinel only exists once data
  // has loaded, so a mount-time attach would observe nothing.
  useEffect(() => { setLimit(initialLimit()); }, [filters]);
  useEffect(() => {
    const el = sentinel.current;
    if (!el) return;
    const io = new IntersectionObserver((entries) => {
      if (entries.some((e) => e.isIntersecting)) setLimit((l) => l + PAGE);
    }, { rootMargin: "200px" });
    io.observe(el);
    return () => io.disconnect();
  }, [cat.loading, filtered.length === 0]);

  // playground targets: one per provider that exposes the selected model, most
  // specific first; label falls back to the provider name for customs
  const pgTargets: PgTarget[] = pg
    ? (rows.find((r) => canonModelId(r.id) === canonModelId(pg.model))?.providers ?? [])
        .filter((p) => p.exposed)
        .map((p) => {
          const pr = provs.find((x) => x.name === p.name);
          return {
            provider: p.name,
            models: pr?.models ?? [],
            connections: pr?.connections ?? [],
          };
        })
        .filter((t) => t.models.length > 0)
    : [];

  if (cat.error) return <><PageHead title="Models" desc="Every model across your providers — pricing, context, capabilities." /><ErrorBanner msg={cat.error} onRetry={cat.reload} /></>;

  return (
    <>
      <PageHead title="Models" desc="Every model across your providers — pricing, context, capabilities.">
        <span className="faint">{filtered.length} model{filtered.length === 1 ? "" : "s"}</span>
      </PageHead>
      {cat.loading && <SkeletonCards />}
      {!cat.loading && (
        <div className="card">
          <div className="toolbar">
            <input aria-label="filter models" placeholder="filter…" value={filters.text}
              onChange={(e) => setFilters({ ...filters, text: e.target.value.toLowerCase() })} />
            <select aria-label="provider filter" value={filters.provider}
              onChange={(e) => setFilters({ ...filters, provider: e.target.value })}>
              <option value="all">all providers</option>
              {REGISTRY.map((r) => <option key={r.id} value={r.id}>{r.title}</option>)}
              {[...new Set(rows.flatMap((r) => r.providers.map((p) => p.name)))]
                .filter((n) => !REGISTRY.some((r) => r.entries.some((e) => e.name === n)))
                .map((n) => <option key={n} value={n}>{n}</option>)}
            </select>
            <select aria-label="price filter" value={filters.price}
              onChange={(e) => setFilters({ ...filters, price: e.target.value as Filters["price"] })}>
              <option value="all">any price</option>
              <option value="free">free</option>
              <option value="paid">paid</option>
            </select>
            {([["reasoning", "think"], ["tool_call", "tools"], ["image", "img"]] as const).map(([k, label]) => (
              <button key={k} className={`btn sm chip${filters[k] ? " chip-on" : ""}`} aria-pressed={filters[k]}
                onClick={() => setFilters({ ...filters, [k]: !filters[k] })}>{label}</button>
            ))}
            <span className="spacer" />
            <label className="checkbox">
              <input type="checkbox" checked={filters.exposedOnly}
                onChange={(e) => setFilters({ ...filters, exposedOnly: e.target.checked })} />
              exposed only
            </label>
          </div>
          {filtered.length === 0 ? <Empty>no models match</Empty> : (
            <div className="table-wrap" style={{ maxHeight: "calc(100vh - 320px)" }}>
              <table>
                <thead>
                  <tr>
                    {sort.th("id", "model")}
                    <th className="col-lg">use as</th>
                    <th className="col-lg models-providers">providers</th>
                    {sort.th("context", "context", true)}
                    {sort.th("max_output", "max out", true, "col-md")}
                    {sort.th("input", "$ in", true)}
                    {sort.th("output", "$ out", true)}
                    {sort.th("cache_read", "$ cache", true, "col-md")}
                    <th className="col-lg">caps</th>
                    <th>▶</th>
                  </tr>
                </thead>
                <tbody>
                  {visible.map((m) => {
                    const spread = priceSpread(m);
                    const open = expanded.has(m.id);
                    return (
                    <Fragment key={m.id}>
                    <tr className={open ? "expanded" : undefined}>
                      <td className="mono" title={m.name || m.id}>
                        <span className="model-cell">
                          {spread && (
                            <button className="expand-btn" aria-expanded={open}
                              aria-label={`${open ? "hide" : "show"} per-provider pricing for ${m.id}`}
                              title="per-provider pricing"
                              onClick={() => setExpanded((s) => { const n = new Set(s); if (n.has(m.id)) { n.delete(m.id); } else { n.add(m.id); } return n; })}>
                              {open ? "−" : `+${m.providers.filter((p) => p.exposed).length}`}
                            </button>
                          )}
                          <button className="copy-chip" aria-label={`copy model id ${m.id}`} title={m.name ? `${m.name} (${m.id})` : m.id}
                            onClick={() => copyText(m.id).then(() => toast("model id copied")).catch(() => toast("could not copy", "err"))}>{displayName(m.id)}</button>
                        </span>
                      </td>
                      <td className="col-lg use-as">
                        {servedIds(m).map((s) => (
                          <button key={s} className="copy-chip" aria-label={`copy ${s}`} title="copy — request the model with this id"
                            onClick={() => copyText(s).then(() => toast(`${s} copied`)).catch(() => toast("could not copy", "err"))}>{s}</button>
                        ))}
                      </td>
                      <td className="col-lg models-providers">
                        <span className="badges">
                          {m.providers.map((p) => (
                            <span key={p.name} className={`badge ${p.exposed ? "ok" : "muted"}`} title={p.exposed ? "exposed" : "in catalog, not exposed"}>{p.name}</span>
                          ))}
                        </span>
                      </td>
                      <td className="num">{m.context ? fmtTok(m.context) : "—"}</td>
                      <td className="num col-md">{m.max_output ? fmtTok(m.max_output) : "—"}</td>
                      {spread ? (
                        <td className="num spread" colSpan={3} title="same model, different prices per provider — expand for details">
                          {fmtPrice(spread[0])}–{fmtPrice(spread[1])}
                        </td>
                      ) : (
                        <>
                          <td className="num">{fmtPrice(m.input)}</td>
                          <td className="num">{fmtPrice(m.output)}</td>
                          <td className="num col-md">{m.cache_read > 0 ? fmtPrice(m.cache_read) : "—"}</td>
                        </>
                      )}
                      <td className="col-lg">
                        {m.reasoning && <span className="badge muted" title="extended thinking">think</span>}
                        {m.tool_call && <span className="badge muted" title="tool calling">tools</span>}
                        {m.image && <span className="badge muted" title="image input">img</span>}
                        {m.family === "classifier" && <span className="badge" title="System One decision model — typed answers, no chat">clf</span>}
                        {m.manual && <span className="badge muted" title="added by hand">manual</span>}
                      </td>
                      <td>
                        <button className="btn sm" aria-label={`playground ${m.id}`}
                          title={m.family === "classifier" ? "decision model — no chat playground" : "open in playground"}
                          disabled={!m.providers.some((p) => p.exposed) || m.family === "classifier"}
                          onClick={() => {
                            const first = m.providers.find((p) => p.exposed);
                            setPg({ model: m.id, provider: first?.name ?? "" });
                            requestAnimationFrame(() => pgRef.current?.scrollIntoView({ behavior: "smooth", block: "start" }));
                          }}>
                          <IconPlay size={12} />
                        </button>
                      </td>
                    </tr>
                    {open && spread && m.providers.filter((p) => p.exposed).map((p, i, all) => (
                      <tr key={`${m.id}:${p.name}`} className={i === all.length - 1 ? "prov-row last" : "prov-row"}>
                        <td colSpan={5} className="mono">
                          <span className={i === all.length - 1 ? "tree last" : "tree"} aria-hidden="true">{i === all.length - 1 ? "└──" : "├──"}</span>
                          <span className="badge ok">{p.name}</span>
                          <button className="copy-chip" aria-label={`copy ${p.served_as}`} title={`copy — request the model with this id on ${p.name}`}
                            onClick={() => copyText(p.served_as).then(() => toast(`${p.served_as} copied`)).catch(() => toast("could not copy", "err"))}>{p.served_as}</button>
                        </td>
                        <td className="num">{fmtPrice(p.input)}</td>
                        <td className="num">{fmtPrice(p.output)}</td>
                        <td className="num col-md">{p.cache_read > 0 ? fmtPrice(p.cache_read) : "—"}</td>
                        <td colSpan={2} />
                      </tr>
                    ))}
                    </Fragment>
                    );
                  })}
                </tbody>
              </table>
              <div ref={sentinel} style={{ height: 1 }} aria-hidden="true" />
            </div>
          )}
          {visible.length < filtered.length && (
            <div className="faint" style={{ padding: "6px 0" }}>showing {visible.length} of {fmtN(filtered.length)} — scroll for more</div>
          )}
        </div>
      )}

      <div className="card" ref={pgRef} style={{ marginTop: 16 }}>
        <h3>Playground</h3>
        {pg && pgTargets.length > 0 ? (
          <Playground
            key={`${pg.provider}:${pg.model}`}
            targets={pgTargets}
            model={pg.model}
            onModel={(m) => setPg((s) => s && { ...s, model: m })}
          />
        ) : (
          <Empty>{pg ? "no exposed provider serves this model yet" : "pick a model above and hit ▶ to test it here"}</Empty>
        )}
      </div>
    </>
  );
}

function SkeletonCards() {
  return (
    <div className="card">
      <Skeleton h={14} w="30%" />
      <div style={{ height: 12 }} />
      <Skeleton h={200} />
    </div>
  );
}
