import { useMemo, useState } from "react";

/** One routable model id from GET /admin/api/routable. */
export interface RoutableModel { id: string; family: string }

/** A selected allow entry: an exact routable id, or a custom pattern. */
interface Entry { value: string; pattern: boolean }

/** splitAllow: exact ids present in `models` first, then patterns (globs or
 *  ids no longer routable) — patterns are never silently dropped on edit. */
export function splitAllow(value: string[], models: RoutableModel[]): Entry[] {
  const known = new Set(models.map((m) => m.id));
  return value.map((v) => ({ value: v, pattern: v.includes("*") || !known.has(v) }));
}

/** Selection summary: "2 models · 1 pattern — …" (exact-vs-pattern semantics). */
export function allowSummary(value: string[], models: RoutableModel[]): string {
  const entries = splitAllow(value, models);
  const nId = entries.filter((e) => !e.pattern).length;
  const nPat = entries.length - nId;
  const parts = [
    ...(nId ? [`${nId} model${nId === 1 ? "" : "s"}`] : []),
    ...(nPat ? [`${nPat} pattern${nPat === 1 ? "" : "s"}`] : []),
  ];
  if (parts.length === 0) return "";
  return `${parts.join(" · ")} — exact picks cover current ids only; patterns like glm-* also cover models added later`;
}

/** ModelPicker: searchable multi-select over the routable model ids for a
 *  key's allow list. "All models (*)" toggle covers the whole-keycase; exact
 *  picks are a snapshot of current ids (stated in the summary line) while the
 *  pattern input keeps glob coverage for future ids. */
export function ModelPicker({ models, value, onChange }: {
  models: RoutableModel[];
  value: string[];
  onChange: (v: string[]) => void;
}) {
  const [query, setQuery] = useState("");
  const [pattern, setPattern] = useState("");
  // "all models" is the empty selection (server coerces [] → ["*"]). A plain
  // computed checkbox would be idempotent — unchecking [] lands on ["*"], still
  // all — so leaving all-mode is a local off-switch; any selection or pattern
  // clears it again.
  const [off, setOff] = useState(false);
  const all = !off && (value.length === 0 || (value.length === 1 && value[0] === "*"));
  const q = query.trim().toLowerCase();

  const filtered = useMemo(() => {
    const hits = q ? models.filter((m) => m.id.toLowerCase().includes(q) || m.family.includes(q)) : models;
    return hits.slice(0, 200); // ponytail: cap render; refine search beyond 200 hits
  }, [models, q]);

  const toggle = (id: string) => {
    setOff(false);
    onChange(value.includes(id) ? value.filter((v) => v !== id) : [...value, id]);
  };

  const addPattern = () => {
    const p = pattern.trim();
    if (!p || value.includes(p)) return;
    setOff(false);
    onChange([...value, p]);
    setPattern("");
  };

  const summary = allowSummary(value, models);
  const rowId = (v: string) => `mp-${v.replace(/[^a-z0-9-]/gi, "_")}`;

  return (
    <div className="mp">
      <label className="checkbox" htmlFor="mp-all">
        <input id="mp-all" type="checkbox" checked={all}
          onChange={(e) => (e.target.checked
            ? (setOff(false), onChange([]))
            : (setOff(true), value.length === 1 && value[0] === "*" && onChange([])))} />
        {" all models (*)"}
      </label>
      {!all && (
        <>
          {value.length === 0 && (
            <div className="field-hint">no models selected — saving will share all models (*)</div>
          )}
          {summary && <div className="field-hint">{summary}</div>}
          {value.length > 0 && (
            <div className="mp-chips">
              {splitAllow(value, models).map((e) => (
                <span key={e.value} className="mp-chip">
                  <span className="mono">{e.value}</span>
                  {e.pattern && <span className="badge muted" title="glob pattern — not an exact routable id">pattern</span>}
                  <button type="button" className="mp-x" aria-label={`remove ${e.value}`}
                    onClick={() => onChange(value.filter((v) => v !== e.value))}>×</button>
                </span>
              ))}
            </div>
          )}
          <input type="search" placeholder="search models…" aria-label="search models"
            value={query} onChange={(e) => setQuery(e.target.value)} />
          <div className="mp-list" role="group" aria-label="routable models">
            {models.length === 0 ? <div className="faint" style={{ padding: 8 }}>no routable models — use a pattern below</div>
              : filtered.length === 0 ? <div className="faint" style={{ padding: 8 }}>no match — add it as a pattern below</div>
              : filtered.map((m) => (
                <label key={m.id} className="mp-row" htmlFor={rowId(m.id)}>
                  <input id={rowId(m.id)} type="checkbox" checked={value.includes(m.id)}
                    onChange={() => toggle(m.id)} />
                  <span className="mono">{m.id}</span>
                  {m.family === "decision" && <span className="badge" title="decision models — advertised on /v1/systemone/models">clf</span>}
                </label>
              ))}
            {q && models.filter((m) => m.id.toLowerCase().includes(q) || m.family.includes(q)).length > filtered.length && (
              <div className="faint" style={{ padding: 8 }}>refine search — {filtered.length}+ matches</div>
            )}
          </div>
          <div className="mp-pattern">
            <input placeholder="add pattern (e.g. glm-*)" aria-label="add glob pattern"
              value={pattern} onChange={(e) => setPattern(e.target.value)}
              onKeyDown={(e) => e.key === "Enter" && (e.preventDefault(), addPattern())} />
            <button type="button" className="btn sm" onClick={addPattern} disabled={!pattern.trim()}>Add</button>
            <div className="field-hint">patterns (e.g. glm-*) match current and future model ids</div>
          </div>
        </>
      )}
    </div>
  );
}
