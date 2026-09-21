import { useCallback, useEffect, useMemo, useState } from "react";
import { get } from "./api";

/** Run fn immediately and on an interval; cleaned up on unmount/deps change. */
export function usePoll(fn: () => void, ms: number) {
  useEffect(() => {
    fn();
    const t = setInterval(fn, ms);
    return () => clearInterval(t);
  }, [fn, ms]);
}

/** Force a re-render every `ms` (for ticking elapsed times). */
export function useTick(ms = 1000) {
  const [, setN] = useState(0);
  useEffect(() => {
    const t = setInterval(() => setN((n) => n + 1), ms);
    return () => clearInterval(t);
  }, [ms]);
}

export interface ApiState<T> {
  data: T | null;
  error: string;
  loading: boolean;
  reload: () => void;
}

/** Fetch an admin API path; keeps stale data while reloading, exposes retry. */
export function useApi<T>(path: string | null): ApiState<T> {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const load = useCallback(() => {
    if (!path) return;
    get(path)
      .then((d: T) => { setData(d); setError(""); setLoading(false); })
      .catch((e: unknown) => { setError(String(e instanceof Error ? e.message : e)); setLoading(false); });
  }, [path]);
  useEffect(() => { load(); }, [load]);
  return { data, error, loading, reload: load };
}

/** Click-to-sort table state. `th` renders a sortable header cell. */
export function useSorted<T extends object>(rows: T[], initialKey: keyof T, initialDir: 1 | -1 = -1) {
  const [key, setKey] = useState<keyof T>(initialKey);
  const [dir, setDir] = useState<1 | -1>(initialDir);
  const sort = (k: keyof T) => (key === k ? setDir((d) => (d === 1 ? -1 : 1)) : (setKey(k), setDir(-1)));
  const sorted = useMemo(() => {
    return [...rows].sort((a, b) => cmpVals(a[key], b[key], dir));
  }, [rows, key, dir]);
  const th = (k: keyof T, label: string, num = false, cls = "") => (
    <th
      className={`${num ? "n " : ""}sortable${cls ? ` ${cls}` : ""}`}
      aria-sort={key === k ? (dir === 1 ? "ascending" : "descending") : "none"}
      tabIndex={0}
      onClick={() => sort(k)}
      onKeyDown={(e) => (e.key === "Enter" || e.key === " ") && (e.preventDefault(), sort(k))}
    >
      {label}
      <span className="sort-ind" aria-hidden>{key === k ? (dir === 1 ? "▲" : "▼") : "↕"}</span>
    </th>
  );
  return { sorted, th };
}

/** Sort comparator: numbers numeric, strings lexical, null/undefined last regardless of dir. */
export const cmpVals = (x: unknown, y: unknown, dir: 1 | -1 = 1) => {
  if (x == null || y == null) return (x == null ? 1 : 0) - (y == null ? 1 : 0);
  const c = typeof x === "number" && typeof y === "number" ? x - y : String(x).localeCompare(String(y));
  return c * dir;
};
