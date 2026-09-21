import { useEffect, useState } from "react";
import { get, fmtN, fmtMs, fmtUSD, BreakdownRow, Summary } from "../api";
import { useApi, usePoll, useSorted } from "../hooks";
import { TimeChart } from "../Chart";
import { cacheHitPct, Empty, ErrorBanner, PageHead, SkeletonCards, StatCard } from "../components";

function useSummary(hours: number): { sum: Summary | null; error: string; loading: boolean; reload: () => void } {
  const { data, error, loading, reload } = useApi<{ summary: Summary }>(`summary?hours=${hours}&bucket=hour`);
  usePoll(reload, 10000);
  return { sum: data?.summary ?? null, error, loading, reload };
}

const RANGES: [number, string][] = [[1, "Last hour"], [24, "Last 24h"], [168, "Last 7d"], [720, "Last 30d"]];

export function UsageTab() {
  const [hours, setHours] = useState(24);
  const { sum, error, loading, reload } = useSummary(hours);
  const [byModel, setByModel] = useState<BreakdownRow[]>([]);
  const [byProvider, setByProvider] = useState<BreakdownRow[]>([]);

  useEffect(() => {
    // dead-flag: a slow response for the old range must not overwrite the new
    let dead = false;
    get(`breakdown?by=model&hours=${hours}`)
      .then((r: { breakdown: BreakdownRow[] }) => { if (!dead) setByModel(r.breakdown); })
      .catch(() => { if (!dead) setByModel([]); });
    get(`breakdown?by=provider&hours=${hours}`)
      .then((r: { breakdown: BreakdownRow[] }) => { if (!dead) setByProvider(r.breakdown); })
      .catch(() => { if (!dead) setByProvider([]); });
    return () => { dead = true; };
  }, [hours]);

  return (
    <>
      <PageHead title="Usage" desc="Volume, tokens and cost across your providers">
        <select value={hours} onChange={(e) => setHours(+e.target.value)} aria-label="time range">
          {RANGES.map(([h, label]) => <option key={h} value={h}>{label}</option>)}
        </select>
      </PageHead>
      {error && <ErrorBanner msg={error} onRetry={reload} />}
      {loading && !sum ? <SkeletonCards /> : sum && (
        <>
          <div className="grid stats">
            <StatCard label="Requests" value={fmtN(sum.requests)} sub={`${fmtN(sum.errors)} errors`} tone={sum.errors > 0 ? "warn" : undefined} />
            <StatCard label="Cost" value={fmtUSD(sum.cost_usd)} />
            <StatCard label="Tokens in" value={fmtN(sum.tok_in)} sub={`${cacheHitPct(sum)}% cache read`} />
            <StatCard label="Tokens out" value={fmtN(sum.tok_out)} />
          </div>
          <div className="card">
            <h3>Requests &amp; errors</h3>
            <TimeChart data={sum.series} height={220}
              series={[{ key: "requests", label: "requests" }, { key: "errors", label: "errors" }]} />
          </div>
          <div className="card">
            <h3>Tokens</h3>
            <TimeChart data={sum.series} height={200}
              series={[{ key: "tok_in", label: "in" }, { key: "tok_out", label: "out" }]} />
          </div>
          <div className="card">
            <h3>Cost</h3>
            <TimeChart data={sum.series} height={200} series={[{ key: "cost", label: "cost (USD)" }]} />
          </div>
          <div className="grid half">
            <div className="card"><h3>By model</h3><BreakTable rows={byModel} /></div>
            <div className="card"><h3>By provider</h3><BreakTable rows={byProvider} /></div>
          </div>
        </>
      )}
    </>
  );
}

function BreakTable({ rows }: { rows: BreakdownRow[] }) {
  const { sorted, th } = useSorted(rows, "requests");
  if (rows.length === 0) return <Empty>no data in range</Empty>;
  return (
    <div className="table-wrap">
      <table>
        <thead><tr>
          {th("name", "name")}
          {th("requests", "reqs", true)}
          {th("errors", "errors", true)}
          {th("tok_in", "tok in", true)}
          {th("tok_out", "tok out", true)}
          {th("cache_read", "cache rd", true, "col-md")}
          {th("cost", "cost", true)}
          {th("ttft_p50_ms", "ttft p50", true, "col-lg")}
        </tr></thead>
        <tbody>
          {sorted.map((r) => (
            <tr key={r.name}>
              <td>{r.name}</td>
              <td className="n">{fmtN(r.requests)}</td>
              <td className="n">{r.errors > 0 ? <span className="err">{fmtN(r.errors)}</span> : 0}</td>
              <td className="n">{fmtN(r.tok_in)}</td>
              <td className="n">{fmtN(r.tok_out)}</td>
              <td className="n col-md">{fmtN(r.cache_read)}</td>
              <td className="n">{fmtUSD(r.cost)}</td>
              <td className="n col-lg">{fmtMs(r.ttft_p50_ms)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
