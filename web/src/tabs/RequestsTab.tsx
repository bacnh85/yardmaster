import { useState } from "react";
import { fmtDur, fmtMs, fmtN, fmtTime, fmtUSD, ReqRow } from "../api";
import { useApi, usePoll, useSorted } from "../hooks";
import { Empty, ErrCell, ErrorBanner, Modal, PageHead, Skeleton, StatusBadge } from "../components";

export function RequestsTab() {
  const { data, error, loading, reload } = useApi<{ requests: ReqRow[] }>("requests?limit=100");
  usePoll(reload, 5000);
  const rows = data?.requests ?? [];
  const { sorted, th } = useSorted(rows, "ts");
  const [detail, setDetail] = useState<ReqRow | null>(null);

  return (
    <>
      <PageHead title="Requests" desc="Last 100 proxied requests — click a row for details" />
      {error && <ErrorBanner msg={error} onRetry={reload} />}
      {loading && rows.length === 0 ? (
        <div className="card">
          <Skeleton h={13} w="100%" />
          <div style={{ height: 8 }} />
          <Skeleton h={13} w="100%" />
          <div style={{ height: 8 }} />
          <Skeleton h={13} w="80%" />
        </div>
      ) : rows.length === 0 ? (
        <Empty>no requests yet</Empty>
      ) : (
        <div className="card table-card">
          <div className="table-wrap">
            <table>
              <thead><tr>
                {th("ts", "time")}
                {th("model", "model")}
                {th("provider", "provider")}
                {th("key", "key")}
                {th("status", "status")}
                <th aria-label="error" className="col-lg" />
                {th("ttft_ms", "ttft", true)}
                {th("dur_ms", "dur", true)}
                {th("tok_in", "in", true)}
                {th("tok_out", "out", true)}
                {th("cache_read", "cache", true, "col-md")}
                {th("cost_usd", "cost", true, "col-lg")}
                {th("attempts", "tries", true, "col-md")}
              </tr></thead>
              <tbody>
                {sorted.map((r) => (
                  <tr key={r.id} className="clickable" tabIndex={0}
                    aria-label={`request detail: ${r.model}, status ${r.status}`}  /* keep implicit row role — role="button" would break the table pattern */
                    onClick={() => setDetail(r)}
                    onKeyDown={(e) => (e.key === "Enter" || e.key === " ") && (e.preventDefault(), setDetail(r))}>
                    <td className="mono">{fmtTime(r.ts)}</td>
                    <td>{r.model}</td>
                    <td>{r.provider}</td>
                    <td>{r.key}</td>
                    <td><StatusBadge status={r.status} /></td>
                    <td className="col-lg"><ErrCell err={r.err} /></td>
                    <td className="n">{fmtMs(r.ttft_ms)}</td>
                    <td className="n">{fmtDur(r.dur_ms)}</td>
                    <td className="n">{fmtN(r.tok_in)}</td>
                    <td className="n">{fmtN(r.tok_out)}</td>
                    <td className="n col-md">{fmtN(r.cache_read)}</td>
                    <td className="n col-lg">{fmtUSD(r.cost_usd)}</td>
                    <td className="n col-md">{r.attempts}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}
      {detail && <RequestDetail r={detail} onClose={() => setDetail(null)} />}
    </>
  );
}

function RequestDetail({ r, onClose }: { r: ReqRow; onClose: () => void }) {
  const f: [string, React.ReactNode][] = [
    ["time", new Date(r.ts).toLocaleString()],
    ["key", r.key],
    ["model", r.model],
    ["provider", r.provider],
    ["status", <StatusBadge status={r.status} />],
    ["stream", r.stream ? "yes" : "no"],
    ["ttft", fmtMs(r.ttft_ms)],
    ["duration", fmtDur(r.dur_ms)],
    ["tokens in", fmtN(r.tok_in)],
    ["tokens out", fmtN(r.tok_out)],
    ["cache read", fmtN(r.cache_read)],
    ["cache write", fmtN(r.cache_write)],
    ["cost", fmtUSD(r.cost_usd)],
    ["attempts", fmtN(r.attempts)],
  ];
  return (
    <Modal title="Request detail" onClose={onClose}>
      <table className="kv">
        <tbody>
          {f.map(([k, v]) => <tr key={k}><th>{k}</th><td className="num">{v}</td></tr>)}
        </tbody>
      </table>
      {r.err && (
        <div className="detail-err">
          <div className="label">error</div>
          <pre className="mono">{r.err}</pre>
        </div>
      )}
    </Modal>
  );
}
