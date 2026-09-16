import { fmtDur, fmtMs, fmtN, Summary } from "../api";
import { useApi, usePoll } from "../hooks";
import { TimeChart } from "../Chart";
import { Empty, ErrorBanner, PageHead, SkeletonCards, StatCard, fmtPct } from "../components";

export function LatencyTab() {
  const { data, error, loading, reload } = useApi<{ summary: Summary }>("summary?hours=24&bucket=hour");
  usePoll(reload, 10000);
  const sum = data?.summary ?? null;

  return (
    <>
      <PageHead title="Latency" desc="Time-to-first-token and duration over the last 24 hours" />
      {error && <ErrorBanner msg={error} onRetry={reload} />}
      {loading && !sum ? <SkeletonCards /> : sum ? (
        <>
          <div className="grid stats">
            <StatCard label="TTFT p50" value={fmtMs(sum.ttft_p50_ms)} />
            <StatCard label="TTFT p95" value={fmtMs(sum.ttft_p95_ms)} />
            <StatCard label="Avg duration" value={fmtDur(sum.avg_dur_ms)} />
            <StatCard label="Error rate" value={fmtPct(sum.errors, sum.requests)}
              sub={`${fmtN(sum.errors)} of ${fmtN(sum.requests)}`}
              tone={sum.errors > 0 ? "warn" : undefined} />
          </div>
          <div className="card">
            <h3>Errors per hour (24h)</h3>
            <TimeChart data={sum.series} height={200} series={[{ key: "errors", label: "errors" }]} />
          </div>
        </>
      ) : <Empty>no data</Empty>}
    </>
  );
}
