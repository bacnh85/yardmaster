import { useEffect, useRef, useState } from "react";
import { fmtN } from "./api";
import { copyText } from "./clipboard";
import { IconX } from "./icons";

/* ---- loading / error / empty ---- */

export const Skeleton = ({ h = 16, w }: { h?: number; w?: number | string }) => (
  <div className="skeleton" style={{ height: h, width: w }} />
);

export const SkeletonCards = ({ n = 4 }: { n?: number }) => (
  <div className="grid stats">
    {Array.from({ length: n }, (_, i) => (
      <div className="card stat" key={i}>
        <Skeleton h={12} w="40%" />
        <div style={{ height: 8 }} />
        <Skeleton h={24} w="60%" />
      </div>
    ))}
  </div>
);

export function ErrorBanner({ msg, onRetry }: { msg: string; onRetry?: () => void }) {
  return (
    <div className="error-banner" role="alert">
      <span>couldn&apos;t load data: {msg}</span>
      {onRetry && <button className="btn sm" onClick={onRetry}>retry</button>}
    </div>
  );
}

export const Empty = ({ children }: { children: React.ReactNode }) => (
  <div className="empty">{children}</div>
);

/* ---- cards / badges ---- */

export function StatCard({ label, value, sub, tone }: { label: string; value: string; sub?: string; tone?: "ok" | "warn" | "danger" }) {
  const cls = tone === "danger" ? "stat danger" : tone === "warn" ? "stat warn" : tone === "ok" ? "stat ok" : "stat";
  return (
    <div className={`card ${cls}`}>
      <div className="label">{label}</div>
      <div className="value num">{value}</div>
      {sub && <div className="sub">{sub}</div>}
    </div>
  );
}

export function StatusBadge({ status }: { status: number }) {
  const cls = status < 300 ? "ok" : status < 500 ? "warn" : "danger";
  return <span className={`badge ${cls} num`}>{status}</span>;
}

/* ---- modal ---- */

export function Modal({ title, onClose, children, wide }: {
  title: string; onClose: () => void; children: React.ReactNode; wide?: boolean;
}) {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    // focus the dialog (unless a child grabbed focus, e.g. autoFocus inputs);
    // restore focus to the trigger on close. ponytail: no full focus trap — Esc
    // + overlay click cover it; add a trap if tabbing into the page behind matters.
    const prev = document.activeElement as HTMLElement | null;
    if (ref.current && !ref.current.contains(document.activeElement)) ref.current.focus();
    const k = (e: KeyboardEvent) => e.key === "Escape" && onClose();
    window.addEventListener("keydown", k);
    return () => { window.removeEventListener("keydown", k); prev?.focus?.(); };
  }, [onClose]);
  return (
    <div className="modal-overlay" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
      <div ref={ref} tabIndex={-1} className={`modal ${wide ? "wide" : ""}`} role="dialog" aria-modal="true" aria-label={title}>
        <div className="modal-head">
          <h3>{title}</h3>
          <button className="icon-btn" onClick={onClose} aria-label="close"><IconX /></button>
        </div>
        <div className="modal-body">{children}</div>
      </div>
    </div>
  );
}

export function Confirm({ title, body, danger, action, onDone }: {
  title: string; body: React.ReactNode; danger?: boolean; action: string;
  onDone: (ok: boolean) => void;
}) {
  return (
    <Modal title={title} onClose={() => onDone(false)}>
      <div style={{ display: "grid", gap: 12 }}>
        <div>{body}</div>
        <div className="row end">
          <button className="btn" onClick={() => onDone(false)}>Cancel</button>
          <button className={`btn ${danger ? "danger solid" : "primary"}`} onClick={() => onDone(true)}>{action}</button>
        </div>
      </div>
    </Modal>
  );
}

/* ---- toasts ---- */

interface ToastItem { id: number; msg: string; kind: "ok" | "err" }
let pushToast: ((msg: string, kind?: "ok" | "err") => void) | null = null;

export const toast = (msg: string, kind: "ok" | "err" = "ok") => pushToast?.(msg, kind);

export function Toaster() {
  const [items, setItems] = useState<ToastItem[]>([]);
  useEffect(() => {
    pushToast = (msg, kind = "ok") => {
      const id = Math.random();
      setItems((a) => [...a, { id, msg, kind }]);
      setTimeout(() => setItems((a) => a.filter((i) => i.id !== id)), 3500);
    };
    return () => { pushToast = null; };
  }, []);
  if (items.length === 0) return null;
  return (
    <div className="toaster" aria-live="polite">
      {items.map((i) => <div key={i.id} className={`toast ${i.kind}`}>{i.msg}</div>)}
    </div>
  );
}

/* ---- page header ---- */

export function PageHead({ title, desc, children }: { title: string; desc?: string; children?: React.ReactNode }) {
  return (
    <div className="page-head">
      <div>
        <h2>{title}</h2>
        {desc && <div className="page-desc">{desc}</div>}
      </div>
      {children && <div className="page-actions">{children}</div>}
    </div>
  );
}

/* ---- copy button ---- */

export function CopyBtn({ text, what }: { text: string; what: string }) {
  const [done, setDone] = useState(false);
  return (
    <button className="btn sm" onClick={() => {
      copyText(text).then(() => {
        setDone(true);
        setTimeout(() => setDone(false), 1500);
      }).catch(() => toast(`could not copy ${what}`, "err"));
    }}>
      {done ? "copied" : `copy ${what}`}
    </button>
  );
}

export const ErrCell = ({ err }: { err: string }) =>
  err ? <span className="err mono" title={err}>{err.slice(0, 60)}{err.length > 60 ? "…" : ""}</span> : null;

export const fmtPct = (n: number, d: number) => (d > 0 ? `${Math.round((n / d) * 100)}%` : "–");
export const cacheHitPct = (s: { cache_read: number; tok_in: number }) =>
  s.tok_in > 0 ? Math.round((s.cache_read / s.tok_in) * 100) : 0;

export { fmtN };
