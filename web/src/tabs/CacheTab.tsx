import { useEffect, useState } from "react";
import { get, fmtN, fmtUSD, BreakdownRow, Summary } from "../api";
import { useApi, usePoll, useSorted } from "../hooks";
import { TimeChart } from "../Chart";
import { cacheHitPct, totalInput, Empty, ErrorBanner, PageHead, SkeletonCards, StatCard } from "../components";

const RANGES: [number, string][] = [[1, "1h"], [24, "24h"], [168, "7d"], [720, "30d"], [2160, "90d"]];
const bucketFor = (hours: number) => (hours <= 1 ? "minute" : hours > 720 ? "day" : "hour");

/** Cache Rate: share of requests that hit the cache (cache_read > 0). */
export const cacheRate = (cached: number | undefined, requests: number) =>
  requests > 0 && cached !== undefined ? Math.round((cached / requests) * 100) : null;

/** The three derived cells a CacheBreakTable row shows, as rendered:
 * rate/cached degrade to null on pre-upgrade servers (cached_requests absent). */
export const cacheRowCells = (r: BreakdownRow) => ({
  total: totalInput(r),
  reuse: cacheHitPct(r),
  rate: cacheRate(r.cached_requests, r.requests),
  cached: r.cached_requests,
});

function useSummary(hours: number): { sum: Summary | null; error: string; loading: boolean; reload: () => void } {
  const { data, error, loading, reload } = useApi<{ summary: Summary }>(`summary?hours=${hours}&bucket=${bucketFor(hours)}`);
  usePoll(reload, 10000);
  return { sum: data?.summary ?? null, error, loading, reload };
}

export function CacheTab() {
  const [hours, setHours] = useState(24);
  const { sum, error, loading, reload } = useSummary(hours);
  const [byProvider, setByProvider] = useState<BreakdownRow[]>([]);
  const [byModel, setByModel] = useState<BreakdownRow[]>([]);

  useEffect(() => {
    // dead-flag: a slow response for the old range must not overwrite the new
    let dead = false;
    const grab = (by: string, set: (rows: BreakdownRow[]) => void) =>
      get(`breakdown?by=${by}&hours=${hours}`)
        .then((r: { breakdown: BreakdownRow[] }) => { if (!dead) set(r.breakdown); })
        .catch(() => { if (!dead) set([]); });
    grab("provider", setByProvider);
    grab("model", setByModel);
    return () => { dead = true; };
  }, [hours]);

  const rate = sum ? cacheRate(sum.cached_requests, sum.requests) : null;
  // cacheHitPct is the single source for the reuse math (cacheHitPct.test.ts
  // locks its semantics); 0 for no-data matches its locked 0-traffic contract
  const ratio = sum ? cacheHitPct(sum) : 0;
  const nPer = !!sum && sum.requests > 0;

  return (
    <>
      <PageHead title="Cache" desc="Provider-side prompt cache: hit rate, token reuse and savings">
        <div className="seg" role="group" aria-label="time range">
          {RANGES.map(([h, label]) => (
            <button key={h} aria-pressed={hours === h} onClick={() => setHours(h)}>{label}</button>
          ))}
        </div>
      </PageHead>
      {error && <ErrorBanner msg={error} onRetry={reload} />}
      {loading && !sum ? <SkeletonCards /> : sum && (
        <>
          <div className="grid stats-5">
            <StatCard label="Cache Rate" value={rate === null ? "–" : `${rate}%`}
              sub={nPer && sum.cached_requests !== undefined ? `${fmtN(sum.cached_requests)} / ${fmtN(sum.requests)} requests` : undefined} />
            <StatCard label="Cache Reuse Ratio" value={`${ratio}%`} sub="cache read / total input tokens" />
            <StatCard label="Cache Read Tokens" value={fmtN(sum.cache_read)} sub="read from cache" />
            <StatCard label="Cache Write Tokens" value={fmtN(sum.cache_write)} sub="written to cache" />
            <StatCard label="Est. Cost Saved" value={sum.cache_saved_usd === undefined ? "–" : fmtUSD(sum.cache_saved_usd)} sub="provider-side cache" tone="ok" />
          </div>
          <div className="card">
            <h3>Cached vs fresh input</h3>
            <TimeChart data={sum.series} height={220} smooth
              series={[{ key: "cache_read", label: "cache read" }, { key: "tok_in", label: "fresh input" }]} />
          </div>
          <div className="card"><h3>By provider</h3><CacheBreakTable rows={byProvider} /></div>
          <div className="card"><h3>By model</h3><CacheBreakTable rows={byModel} /></div>
        </>
      )}
    </>
  );
}

/** Breakdown table for the Cache tab: cache-focused columns per provider/model. */
export function CacheBreakTable({ rows }: { rows: BreakdownRow[] }) {
  // sort key must match the displayed value: "total input" shows tok_in + cached parts
  const { sorted, th } = useSorted(rows.map((r) => ({ ...r, tok_total: totalInput(r) })), "requests");
  if (rows.length === 0) return <Empty>no data in range</Empty>;
  return (
    <div className="table-wrap">
      <table className="cache-t">
        <thead><tr>
          {th("name", "name")}
          {th("requests", "reqs", true)}
          {th("tok_total", "total input", true)}
          {th("cache_read", "cache read", true)}
          {th("cache_write", "cache write", true)}
          <th className="n">reuse</th>
          <th className="n">rate</th>
          <th className="n">cached</th>
        </tr></thead>
        <tbody>
          {sorted.map((r) => {
            const { total, reuse, rate, cached } = cacheRowCells(r);
            return (
              <tr key={r.name}>
                <td>{r.name}</td>
                <td className="n">{fmtN(r.requests)}</td>
                <td className="n">{fmtN(total)}</td>
                <td className="n">{fmtN(r.cache_read)}</td>
                <td className="n">{fmtN(r.cache_write ?? 0)}</td>
                <td className="n">{reuse}%</td>
                <td className="n">{rate === null ? "–" : `${rate}%`}</td>
                <td className="n">{cached === undefined ? "–" : fmtN(cached)}</td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}
