import { useEffect, useMemo, useState } from "react";
import { get, fmtN, fmtMs, fmtUSD, fmtDur, BreakdownRow, Summary, ModelSeriesPoint } from "../api";
import { useApi, usePoll, useSorted } from "../hooks";
import { TimeChart } from "../Chart";
import { cacheHitPct, fmtPct, totalInput, Empty, ErrorBanner, PageHead, SkeletonCards, StatCard } from "../components";

const RANGES: [number, string][] = [[1, "1h"], [24, "24h"], [168, "7d"], [720, "30d"], [2160, "90d"]];

/** Chart bucket granularity for a range: minute at 1h, day at 90d, hour otherwise. */
export const bucketFor = (hours: number) => (hours <= 1 ? "minute" : hours > 720 ? "day" : "hour");

/** Token-share percentage of one row against the grand total. */
export const sharePct = (part: number, total: number) => (total > 0 ? Math.round((part / total) * 100) : 0);

/** Heatmap intensity 0-4 (0 = no activity) relative to the busiest day. */
export const heatLevel = (tokens: number, max: number) =>
  tokens <= 0 || max <= 0 ? 0 : Math.min(4, 1 + Math.floor((4 * tokens) / max));

/** Daily activity cell for the heatmap (server day buckets, keyed by UTC date —
 * buckets are UTC-midnight boundaries, so local keys would mislabel by a day). */
export interface HeatCell { key: string; ts: number; tokens: number }
const DAY_MS = 86400000;
const dayKey = (ts: number) => {
  const d = new Date(ts);
  return `${d.getUTCFullYear()}-${String(d.getUTCMonth() + 1).padStart(2, "0")}-${String(d.getUTCDate()).padStart(2, "0")}`;
};
/** One cell per UTC day over the trailing 12 months. The server groups by
 *  bucket, so days without requests come back as no row at all — zero-fill
 *  them, or a short history collapses the calendar to a couple of columns. */
export const heatCells = (series: Summary["series"], now = Date.now()): HeatCell[] => {
  const today = Math.floor(now / DAY_MS) * DAY_MS; // UTC midnight today
  // series[0] can sit one partial day before the aligned window start — keep it
  const start = Math.min(today - 364 * DAY_MS, series[0]?.ts ?? Infinity);
  const tokens = new Map(series.map((p) => [dayKey(p.ts), p.tok_in + p.tok_out]));
  return Array.from({ length: Math.round((today - start) / DAY_MS) + 1 }, (_, i) => {
    const ts = start + i * DAY_MS;
    const key = dayKey(ts);
    return { key, ts, tokens: tokens.get(key) ?? 0 };
  });
};

const MONTHS = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];
/** "Oct" at the week column where a month starts (pad = leading days of the
 *  first partial week, so columns line up with the grid's Mon-first rows). */
export const monthLabels = (cells: HeatCell[], pad: number) => {
  const out: { label: string; col: number }[] = [];
  let seen = "";
  cells.forEach((c, i) => {
    const month = c.key.slice(0, 7);
    if (month === seen) return;
    seen = month;
    out.push({ label: MONTHS[Number(c.key.slice(5, 7)) - 1], col: Math.floor((pad + i) / 7) });
  });
  return out;
};

/** Total tokens per weekday, Monday-first (UTC — matches the day buckets). */
export const weekdayTotals = (cells: HeatCell[]) => {
  const sums = Array(7).fill(0);
  for (const c of cells) sums[(new Date(c.ts).getUTCDay() + 6) % 7] += c.tokens;
  return sums;
};

/** Busiest day (max tokens), or null when there is no activity at all. */
export const busiestDay = (cells: HeatCell[]) => {
  const best = cells.reduce<HeatCell | null>((b, c) => (!b || c.tokens > b.tokens ? c : b), null);
  return best && best.tokens > 0 ? best : null;
};

/** Pivot (bucket, model) points into per-bucket rows for TimeChart: top N
 *  models by total tokens, everything else collapsed into "other". Model names
 *  are arbitrary client-supplied strings — they key under a reserved prefix so
 *  a model literally named "ts" can't clobber the row's timestamp (and "other"
 *  merges into the aggregate bucket); the prefix is stripped in the returned
 *  models list and never appears in series keys' meaning. */
export const PIVOT_KEY = "\u0000m:"; // NUL-prefixed: untypeable by clients
export function pivotModels(points: ModelSeriesPoint[], topN = 5) {
  const totals = new Map<string, number>();
  for (const p of points) totals.set(p.model, (totals.get(p.model) ?? 0) + p.tok_in + p.tok_out);
  const top = [...totals.entries()].sort((a, b) => b[1] - a[1]).slice(0, topN).map(([m]) => m);
  const topSet = new Set(top);
  const byTs = new Map<number, Record<string, number>>();
  for (const p of points) {
    let row = byTs.get(p.ts);
    if (!row) byTs.set(p.ts, (row = { ts: p.ts }));
    const key = PIVOT_KEY + (topSet.has(p.model) ? p.model : "other");
    row[key] = (row[key] ?? 0) + p.tok_in + p.tok_out;
  }
  const rows = [...byTs.entries()].sort((a, b) => a[0] - b[0]).map(([, row]) => row as { ts: number; [k: string]: number });
  const models = rows.some((r) => r[PIVOT_KEY + "other"] > 0) ? [...top, "other"] : top;
  return { models, rows };
}

function useSummary(hours: number): { sum: Summary | null; error: string; loading: boolean; reload: () => void } {
  const { data, error, loading, reload } = useApi<{ summary: Summary }>(`summary?hours=${hours}&bucket=${bucketFor(hours)}`);
  usePoll(reload, 10000);
  return { sum: data?.summary ?? null, error, loading, reload };
}

export function UsageTab() {
  const [hours, setHours] = useState(24);
  const { sum, error, loading, reload } = useSummary(hours);
  const tsApi = useApi<{ series: ModelSeriesPoint[] }>(`timeseries?by=model&hours=${hours}&bucket=${bucketFor(hours)}`);
  // 12-month heatmap window — fetched once per mount, never polled
  const daily = useApi<{ summary: Summary }>(`summary?hours=8760&bucket=day`);
  // retention explains a mostly-grey calendar (rows older than N days are pruned)
  const cfg = useApi<{ retention_days?: number }>("config");
  const [byModel, setByModel] = useState<BreakdownRow[]>([]);
  const [byProvider, setByProvider] = useState<BreakdownRow[]>([]);
  const [byKey, setByKey] = useState<BreakdownRow[]>([]);

  useEffect(() => {
    // dead-flag: a slow response for the old range must not overwrite the new
    let dead = false;
    const grab = (by: string, set: (rows: BreakdownRow[]) => void) =>
      get(`breakdown?by=${by}&hours=${hours}`)
        .then((r: { breakdown: BreakdownRow[] }) => { if (!dead) set(r.breakdown); })
        .catch(() => { if (!dead) set([]); });
    grab("model", setByModel); grab("provider", setByProvider); grab("key_name", setByKey);
    return () => { dead = true; };
  }, [hours]);

  const pivot = useMemo(() => pivotModels(tsApi.data?.series ?? []), [tsApi.data]);
  const cells = useMemo(() => heatCells(daily.data?.summary.series ?? []), [daily.data]);
  const auxError = tsApi.error || daily.error; // aux fetches are one-shot — don't let them fail silently

  const nPer = sum && sum.requests > 0; // guards: ratios are meaningless without traffic

  return (
    <>
      <PageHead title="Usage" desc="Volume, tokens and cost across your providers">
        <div className="seg" role="group" aria-label="time range">
          {RANGES.map(([h, label]) => (
            <button key={h} aria-pressed={hours === h} onClick={() => setHours(h)}>{label}</button>
          ))}
        </div>
      </PageHead>
      {(error || auxError) && (
        <ErrorBanner msg={error || auxError}
          onRetry={() => { reload(); tsApi.reload(); daily.reload(); }} />
      )}
      {loading && !sum ? <SkeletonCards /> : sum && (
        <>
          <div className="grid stats">
            <StatCard label="Requests" value={fmtN(sum.requests)} sub={`${fmtN(sum.errors)} errors`} tone={sum.errors > 0 ? "warn" : undefined} />
            <StatCard label="Tokens in" value={fmtN(totalInput(sum))} sub={`${cacheHitPct(sum)}% cache read`} />
            <StatCard label="Tokens out" value={fmtN(sum.tok_out)} />
            <StatCard label="Cost" value={fmtUSD(sum.cost_usd)} />
          </div>
          <div className="grid stats">
            <StatCard label="Avg tokens / req" value={nPer ? fmtN(Math.round(totalInput(sum) / sum.requests)) : "–"} />
            <StatCard label="Cost / req" value={nPer ? fmtUSD(sum.cost_usd / sum.requests) : "–"} />
            <StatCard label="Avg duration" value={fmtDur(sum.avg_dur_ms)} />
            <StatCard label="Error rate" value={fmtPct(sum.errors, sum.requests)} />
          </div>
          <div className="card">
            <h3>Tokens &amp; cost</h3>
            <TimeChart data={sum.series} height={220} smooth
              series={[{ key: "tok_in", label: "tok in" }, { key: "tok_out", label: "tok out" }, { key: "cost", label: "cost (USD)", axis: 2 }]} />
          </div>
          <div className="card">
            <h3>Requests &amp; errors</h3>
            <TimeChart data={sum.series} height={180} smooth
              series={[{ key: "requests", label: "requests" }, { key: "errors", label: "errors" }]} />
          </div>
          {pivot.rows.length > 0 && (
            <div className="card">
              <h3>Tokens by model</h3>
              <TimeChart data={pivot.rows} height={220} smooth
                // rows store values under PIVOT_KEY+model (the collision-safe key) — series must look the same up; bare name stays the label
                series={pivot.models.map((m) => ({ key: PIVOT_KEY + m, label: m }))} />
            </div>
          )}
          <div className="card">
            <h3>Activity — last 12 months</h3>
            <div className="heat-row">
              <div>
                <Heatmap cells={cells} />
                <ActivityNote cells={cells} retention={cfg.data?.retention_days ?? 0} />
              </div>
              <Highlights cells={cells} />
            </div>
          </div>
          <div className="card"><h3>By model</h3><BreakTable rows={byModel} /></div>
          <div className="grid half">
            <div className="card"><h3>By provider</h3><BreakTable rows={byProvider} /></div>
            <div className="card"><h3>By API key</h3><BreakTable rows={byKey} /></div>
          </div>
        </>
      )}
    </>
  );
}

function Heatmap({ cells }: { cells: HeatCell[] }) {
  const max = Math.max(...cells.map((c) => c.tokens), 0);
  // pad the leading partial week so each day lands on its weekday row (Mon-first, UTC)
  const pad = (new Date(cells[0].ts).getUTCDay() + 6) % 7;
  const cols = Math.ceil((pad + cells.length) / 7);
  return (
    <div className="heat-wrap">
      {/* label strip is absolutely positioned in px: the grid is a fixed 11px + 3px rhythm */}
      <div className="heat-months" style={{ width: cols * 14 }} aria-hidden="true">
        {monthLabels(cells, pad).map((m) => (
          <span key={m.label + m.col} style={{ left: m.col * 14 }}>{m.label}</span>
        ))}
      </div>
      <div className="heat" role="img"
        aria-label={`daily token activity, ${cells[0].key} to ${cells[cells.length - 1].key}`}>
        {Array.from({ length: pad }, (_, i) => <div key={`p${i}`} className="heat-pad" aria-hidden />)}
        {cells.map((c) => (
          <div key={c.key} className={`heat-cell heat-${heatLevel(c.tokens, max)}`}
            title={`${c.key} — ${fmtN(c.tokens)} tokens`} />
        ))}
      </div>
    </div>
  );
}

/** What the calendar actually covers — a 12-month window is mostly empty while
 *  the history is short or pruned, and an unlabelled grey grid reads as broken. */
function ActivityNote({ cells, retention }: { cells: HeatCell[]; retention: number }) {
  const active = cells.filter((c) => c.tokens > 0);
  const keep = retention > 0 ? ` History is pruned after ${retention} days.` : "";
  return (
    <div className="heat-cap">
      {active.length === 0
        ? <>No activity in the last 12 months.{keep}</>
        : <>{active.length} of the last {cells.length} days have activity, since {active[0].key}.{keep}</>}
    </div>
  );
}

const WD = ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"];

function Highlights({ cells }: { cells: HeatCell[] }) {
  const best = busiestDay(cells);
  const wk = weekdayTotals(cells);
  const wkMax = Math.max(...wk, 1);
  return (
    <div className="hl-col">
      <div className="hl-day">
        <div className="label">Most active day</div>
        <div className="value num">{best ? best.key : "–"}</div>
        {best && <div className="sub">{fmtN(best.tokens)} tokens</div>}
      </div>
      <div className="wd-bars" role="img" aria-label="total tokens by weekday">
        {wk.map((v, i) => (
          <div className="wd-col" key={i} title={`${WD[i]}: ${fmtN(v)} tokens`}>
            <div className="wd-track">
              <div className="wd-bar" style={{ height: `${Math.max(2, Math.round((v / wkMax) * 100))}%` }} />
            </div>
            <span className="label">{WD[i]}</span>
          </div>
        ))}
      </div>
    </div>
  );
}

export function BreakTable({ rows }: { rows: BreakdownRow[] }) {
  // sort key must match the displayed value: "tok in" shows TOTAL input, so
  // derive it onto the row instead of sorting by the uncached raw field
  const withTotals = rows.map((r) => ({ ...r, tok_total: totalInput(r) }));
  const grand = withTotals.reduce((a, r) => a + r.tok_total, 0);
  const { sorted, th } = useSorted(withTotals, "requests");
  if (rows.length === 0) return <Empty>no data in range</Empty>;
  return (
    <div className="table-wrap">
      <table>
        <thead><tr>
          {th("name", "name")}
          {th("requests", "reqs", true)}
          {th("errors", "errors", true)}
          {th("tok_total", "tok in", true)}
          {th("tok_out", "tok out", true)}
          {th("cache_read", "cache rd", true, "col-md")}
          {th("cost", "cost", true)}
          <th className="n">share</th>
          {th("ttft_p50_ms", "ttft p50", true, "col-lg")}
        </tr></thead>
        <tbody>
          {sorted.map((r) => {
            const pct = sharePct(r.tok_total, grand);
            return (
              <tr key={r.name}>
                <td>{r.name}</td>
                <td className="n">{fmtN(r.requests)}</td>
                <td className="n">{r.errors > 0 ? <span className="err">{fmtN(r.errors)}</span> : 0}</td>
                <td className="n">{fmtN(r.tok_total)}</td>
                <td className="n">{fmtN(r.tok_out)}</td>
                <td className="n col-md">{fmtN(r.cache_read)}</td>
                <td className="n">{fmtUSD(r.cost)}</td>
                <td>
                  <span className="share">
                    <span className="quota-bar" role="img" aria-label={`${pct}% of tokens`}>
                      <span className="quota-fill" style={{ width: `${pct}%` }} />
                    </span>
                    {pct}%
                  </span>
                </td>
                <td className="n col-lg">{fmtMs(r.ttft_p50_ms)}</td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}
