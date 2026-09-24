import { useEffect, useRef, useState } from "react";
import { get, fmtN, type CatalogModel, type ProviderRow } from "../api";
import { useApi } from "../hooks";
import { useSorted, cmpVals } from "../hooks";
import { Empty, ErrorBanner, PageHead, Skeleton } from "../components";
import { REGISTRY, registryFor, wireFamily } from "../presets";
import { Playground, type PgTarget } from "../Playground";
import { IconList, IconPlay } from "../icons";
import { canonModelId } from "./ProvidersTab";

export interface CatalogRowUI {
  id: string; name: string; family: string;
  context: number; max_output: number;   // 0 = unknown
  input: number; output: number; cache_read: number; // -1 = unknown price
  reasoning: boolean; tool_call: boolean; image: boolean; free: boolean; manual: boolean;
  providers: { name: string; prefix: string; exposed: boolean }[];
}

/** Server CatalogRow → UI row: null out unknown prices so cmpVals sorts them
 *  last (BreakTable precedent), drop unknown context/max to 0 → "—". */
export const toRow = (m: CatalogModel & { providers?: { name: string; prefix?: string; exposed: boolean }[] }): CatalogRowUI => ({
  id: m.id, name: m.name ?? "", family: m.family,
  context: m.context ?? 0, max_output: m.max_output ?? 0,
  input: m.input, output: m.output, cache_read: m.cache_read,
  reasoning: !!m.reasoning, tool_call: !!m.tool_call, image: !!m.image,
  free: !!m.free, manual: !!m.manual,
  providers: (m.providers ?? []).map((p) => ({ name: p.name, prefix: p.prefix ?? "", exposed: p.exposed })),
});

export const sortPrice = (n: number): number | null => (n < 0 ? null : n);

/** Filter chain (pure, unit-tested): text substring on id+name, provider name
 *  or registry title, price class, capability flags, exposed-only toggle.
 *  exposedOnly drops unexposed provider ENTRIES from the returned rows — a
 *  model exposed on one provider must not list/badge under another where it's
 *  catalog-only. */
export const filterRows = (rows: CatalogRowUI[], f: Filters): CatalogRowUI[] => {
  const out: CatalogRowUI[] = [];
  for (const m of rows) {
    const provs = f.exposedOnly ? m.providers.filter((p) => p.exposed) : m.providers;
    if (
    (!f.text || m.id.toLowerCase().includes(f.text) || m.name.toLowerCase().includes(f.text)) &&
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

const fmtPrice = (n: number) => (n < 0 ? "—" : n === 0 ? "free" : `$${n}`);
const fmtTok = (n: number) =>
  n >= 1_000_000 ? `${(n / 1_000_000).toFixed(n % 1_000_000 === 0 ? 0 : 1)}M`
  : n >= 1_000 ? `${(n / 1_000).toFixed(n % 1_000 === 0 ? 0 : 1)}K`
  : String(n);

export function ModelsTab() {
  const cat = useApi<{ models: (CatalogModel & { providers?: { name: string; prefix?: string; exposed: boolean }[] })[] }>("catalog");
  const provReq = useApi<{ providers: ProviderRow[] }>("providers");
  const [filters, setFilters] = useState<Filters>(DEFAULT_FILTERS);
  const [limit, setLimit] = useState(initialLimit);
  const sentinel = useRef<HTMLDivElement>(null);
  const pgRef = useRef<HTMLDivElement>(null);
  // playground state: selected model + the provider (key source) serving it
  const [pg, setPg] = useState<null | { model: string; provider: string }>(null);

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
                    <th className="col-lg">providers</th>
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
                  {visible.map((m) => (
                    <tr key={m.id}>
                      <td className="mono" title={m.name || m.id}>{m.id}</td>
                      <td className="col-lg">
                        {m.providers.map((p) => (
                          <span key={p.name} className={`badge ${p.exposed ? "ok" : "muted"}`} title={p.exposed ? "exposed" : "in catalog, not exposed"}>{p.name}</span>
                        ))}
                      </td>
                      <td className="num">{m.context ? fmtTok(m.context) : "—"}</td>
                      <td className="num col-md">{m.max_output ? fmtTok(m.max_output) : "—"}</td>
                      <td className="num">{fmtPrice(m.input)}</td>
                      <td className="num">{fmtPrice(m.output)}</td>
                      <td className="num col-md">{m.cache_read > 0 ? fmtPrice(m.cache_read) : "—"}</td>
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
                  ))}
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
