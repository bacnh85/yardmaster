import { useCallback, useEffect, useRef, useState } from "react";
import { genTps, get, fmtDur, fmtMs, fmtN, fmtTime, fmtUSD, ReqRow } from "../api";
import { useApi, usePoll, useSorted } from "../hooks";
import { Empty, ErrCell, ErrorBanner, Modal, PageHead, Skeleton, StatusBadge } from "../components";

/** Rows per page — the head page (live) and every scroll-in page. */
export const PAGE = 100;

export function RequestsTab() {
  const { data, error, loading, reload } = useApi<{ requests: ReqRow[] }>(`requests?limit=${PAGE}`);
  usePoll(reload, 5000); // head page only: rows paged in below are never re-fetched
  const head = data?.requests ?? [];
  const [older, setOlder] = useState<ReqRow[]>([]);
  const [busy, setBusy] = useState(false);
  const [done, setDone] = useState(false);
  const [pageErr, setPageErr] = useState(false);
  const sentinel = useRef<HTMLDivElement>(null);
  const wrap = useRef<HTMLDivElement>(null);

  // tps precomputed onto each row so the generic column sort can key on it
  const rows = [...head, ...older].map((r) => ({ ...r, tps: genTps(r) }));
  const { sorted, th } = useSorted(rows, "ts");
  const [detail, setDetail] = useState<ReqRow | null>(null);

  // Next page = rows strictly older than the oldest loaded one (keyset, so
  // requests landing mid-scroll can't duplicate or skip a row).
  const loadMore = useCallback(() => {
    const oldest = rows[rows.length - 1];
    if (!oldest || busy || done) return;
    setBusy(true);
    get(`requests?limit=${PAGE}&before=${oldest.id}`)
      .then((d: { requests: ReqRow[] }) => {
        setOlder((prev) => {
          const seen = new Set([...head, ...prev].map((r) => r.id));
          return [...prev, ...d.requests.filter((r) => !seen.has(r.id))];
        });
        if (d.requests.length < PAGE) setDone(true); // short page = end of history
      })
      .catch(() => setPageErr(true)) // stop the auto-loop; retry stays a click
      .finally(() => setBusy(false));
  }, [rows, head, busy, done]);

  // keep the observer's callback out of the deps: a new identity per render
  // would re-arm the observer on every poll tick
  const loadRef = useRef(loadMore);
  loadRef.current = loadMore;
  useEffect(() => {
    const el = sentinel.current;
    if (!el || done || pageErr) return;
    // ~one viewport of lookahead: the next page is already loading by the time
    // the user scrolls into the last rows
    const io = new IntersectionObserver((es) => {
      if (es.some((e) => e.isIntersecting)) loadRef.current();
    }, { root: wrap.current, rootMargin: "800px" }); // ~a viewport of lookahead
    io.observe(el);
    return () => io.disconnect();
  }, [done, pageErr, loading]);

  return (
    <>
      <PageHead title="Requests"
        desc="Newest first — scroll for older. Sorting covers the rows loaded so far." />
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
        <>
          <div className="card table-card scroll-y">
            <div className="table-wrap" ref={wrap}>
              <table>
                <thead><tr>
                  {th("ts", "time")}
                  {th("model", "model")}
                  {th("provider", "provider")}
                  {th("key", "key")}
                  {th("status", "status")}
                  <th aria-label="error" className="col-lg" />
                  {th("ttft_ms", "ttft", true)}
                  {th("queue_ms", "queue", true, "col-md")}
                  {th("dur_ms", "dur", true)}
                  {th("tok_in", "in", true)}
                  {th("tok_out", "out", true)}
                  {th("tps", "tok/s", true, "col-md")}
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
                      <td className="n col-md">{r.queue_ms === undefined ? "–" : fmtMs(r.queue_ms)}</td>
                      <td className="n">{fmtDur(r.dur_ms)}</td>
                      <td className="n">{fmtN(r.tok_in)}</td>
                      <td className="n">{fmtN(r.tok_out)}</td>
                      <td className="n col-md">{r.tps?.toFixed(0) ?? "–"}</td>
                      <td className="n col-md">{fmtN(r.cache_read)}</td>
                      <td className="n col-lg">{fmtUSD(r.cost_usd)}</td>
                      <td className="n col-md">{r.attempts}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
              {/* inside the scrollport: the observer's root, and where "end of
                  history" belongs — the bottom of the list */}
              <div ref={sentinel} className="list-foot">
                {busy && <span role="status">loading older requests…</span>}
                {!busy && pageErr && (
                  <>
                    <span>could not load older requests</span>
                    <button className="btn sm" onClick={() => { setPageErr(false); loadRef.current(); }}>retry</button>
                  </>
                )}
                {!busy && !pageErr && done && <span>end of history</span>}
              </div>
            </div>
          </div>
        </>
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
    ["queue", r.queue_ms === undefined ? "–" : fmtMs(r.queue_ms)],
    ["duration", fmtDur(r.dur_ms)],
    ["tokens in", fmtN(r.tok_in)],
    ["tokens out", fmtN(r.tok_out)],
    ["gen tok/s", genTps(r)?.toFixed(1) ?? "–"],
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
