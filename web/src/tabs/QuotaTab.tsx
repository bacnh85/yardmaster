import { get, fmtResetIn, fmtUSD, type QuotaAccount, type QuotaGroup, type QuotaWindow } from "../api";
import { useApi, usePoll } from "../hooks";
import { Empty, ErrorBanner, PageHead } from "../components";

/** Percent used of a usage window (0 when absent/uncapped). */
export const pct = (w?: QuotaWindow) => (!w || w.cap <= 0 ? 0 : Math.min(100, Math.round((w.used / w.cap) * 100)));

/** Monthly allowance as a bar-ready window: cap = plan total (identified from
 *  the account's window caps), used = total − remaining. Undefined when the
 *  plan total is unknown. Monthly resets on the account's billing-cycle date,
 *  which the provider API does not expose. */
export const monthlyWindow = (a: QuotaAccount): QuotaWindow | undefined =>
  a.monthly_total && a.monthly_credits != null && a.monthly_total > 0
    ? { used: Math.max(0, a.monthly_total - a.monthly_credits), cap: a.monthly_total }
    : undefined;

const SOURCE_LABEL: Record<string, string> = { commandcode: "Command Code", deepseek: "DeepSeek" };

/** Client mirror of the server matcher (internal/server/quota.go quotaSource).
 *  host (not hostname) keeps the port in play, matching Go's url.Host compare. */
export const isQuotaProvider = (base_url: string) => {
  try {
    const h = new URL(base_url).host.toLowerCase();
    return h === "api.commandcode.ai" || h === "api.deepseek.com";
  } catch { return false; }
};

function Bar({ p }: { p: number }) {
  const tone = p >= 95 ? "danger" : p >= 80 ? "warn" : "";
  return (
    <div className="quota-bar" role="img" aria-label={`${p}% used`}>
      <div className={`quota-fill ${tone}`} style={{ width: `${p}%` }} />
    </div>
  );
}

/** One usage window cell: bar + $used/$cap + reset countdown. */
const WindowCell = ({ w }: { w?: QuotaWindow }) => {
  if (!w) return <span className="faint">—</span>;
  return (
    <div className="quota-cell">
      <Bar p={pct(w)} />
      <span className="quota-meta">
        <span className="num">{fmtUSD(w.used)} / {fmtUSD(w.cap)}</span>
        {w.reset_at ? <span className="faint">resets in {fmtResetIn(w.reset_at)}</span> : null}
      </span>
    </div>
  );
};

/** Accounts table for one quota group — shared by the Quota tab and the
 *  Command Code provider detail page. */
export function QuotaTable({ accounts }: { accounts: QuotaAccount[] }) {
  return (
    <div className="table-wrap">
      <table>
        <thead><tr>
          <th>account</th>
          <th>5-hour</th>
          <th>weekly</th>
          <th>monthly</th>
        </tr></thead>
        <tbody>
          {accounts.map((a, i) => (
            <tr key={`${i}-${a.label}-${a.suffix}`}>
              <td>
                {a.label} <span className="mono faint">{a.suffix}</span>
                {a.limited && <span className="badge warn" title="provider reports this account as rate-limited">limited</span>}
                {a.err && <div className="field-err">{a.err}</div>}
              </td>
              <td><WindowCell w={a.five_hour} /></td>
              <td><WindowCell w={a.weekly} /></td>
              <td title="monthly allowance or prepaid balance · reset date not exposed by the provider API">
                {monthlyWindow(a)
                  ? <WindowCell w={monthlyWindow(a)} />
                  : a.monthly_credits != null
                    ? <span className="quota-cell"><span className="num">{fmtUSD(a.monthly_credits, a.currency)} <span className="faint">left</span></span></span>
                    : <span className="faint">—</span>}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

export function QuotaTab() {
  const { data, error, loading, reload } = useApi<{ quotas: QuotaGroup[] }>("quota");
  usePoll(reload, 30000);
  const quotas = data?.quotas ?? [];
  return (
    <>
      <PageHead title="Quota" desc="Provider subscription usage windows per account">
        <button className="btn sm" title="bypass the 60s server cache"
          onClick={() => { get("quota?refresh=1").then(reload).catch(reload); }}>refresh</button>
      </PageHead>
      {error && <ErrorBanner msg={error} onRetry={reload} />}
      {loading && quotas.length === 0 && <div className="faint" style={{ padding: 8 }}>loading…</div>}
      {!loading && quotas.length === 0 && !error && (
        <Empty>no providers expose usage yet — connect Command Code or DeepSeek under Providers</Empty>
      )}
      {quotas.map((g) => (
        <div className="card" key={g.source} style={{ marginBottom: 16 }}>
          <div className="row spread baseline">
            <h3>{SOURCE_LABEL[g.source] ?? g.source}</h3>
            <span className="faint" title="usage is cached for 60s">fetched {new Date(g.fetched_at).toLocaleTimeString()}</span>
          </div>
          <QuotaTable accounts={g.accounts} />
        </div>
      ))}
    </>
  );
}
