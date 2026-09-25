import { useRef, useState } from "react";
import { post, type ProbeResult } from "./api";
import { Empty } from "./components";
import { IconPlay } from "./icons";

/** One selectable playground target: a config provider (key source) + its models. */
export interface PgTarget {
  provider: string; // config provider name (key source)
  models: string[];
  connections: { label: string; suffix: string; disabled?: boolean }[]; // key picker options
}

/** Shared chat playground: model + key pickers, conversation thread, Enter-to-send.
 *  Used by the Models tab and provider detail pages. Single-turn threads POST
 *  the legacy `prompt` shape; multi-turn threads POST `messages`. */
export function Playground({ targets, model, onModel }: {
  targets: PgTarget[];
  model: string;
  onModel: (m: string) => void;
}) {
  const thread = useRef<{ role: "user" | "assistant"; text: string }[]>([]);
  const [ver, setVer] = useState(0); // thread is a ref so a busy re-render never resets it
  const [input, setInput] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  const [last, setLast] = useState<ProbeResult | null>(null);
  const target = targets.find((t) => t.models.includes(model)) ?? targets.find((t) => t.models.length > 0) ?? targets[0];
  const [provider, setProvider] = useState(target?.provider ?? "");
  const [keyIndex, setKeyIndex] = useState(0);
  const listRef = useRef<HTMLDivElement>(null);

  const effProvider = targets.some((t) => t.provider === provider) ? provider : target?.provider ?? "";
  const keys = targets.find((t) => t.provider === effProvider)?.connections ?? [];
  const modelOptions = targets.find((t) => t.provider === effProvider)?.models ?? [];

  const send = async () => {
    const text = input.trim();
    if (!text || busy || !model || !effProvider) return;
    const messages = [...thread.current, { role: "user" as const, text }];
    thread.current = messages;
    setInput(""); setBusy(true); setErr(""); setVer((v) => v + 1);
    requestAnimationFrame(() => listRef.current?.scrollTo({ top: listRef.current.scrollHeight }));
    try {
      const single = messages.length === 1 && messages[0].role === "user";
      const body = single
        ? { provider: effProvider, model, prompt: messages[0].text, key_index: keyIndex }
        : { provider: effProvider, model, messages: messages.map((m) => ({ role: m.role, content: m.text })), key_index: keyIndex };
      const res = (await post("playground", body)) as ProbeResult;
      thread.current = [...messages, { role: "assistant" as const, text: res.text || "(empty response)" }];
      setLast(res);
    } catch (e) {
      setErr(String(e instanceof Error ? e.message : e).slice(0, 200));
      thread.current = messages; // keep the user turn so retry is one Enter away
    } finally {
      setBusy(false);
      setVer((v) => v + 1);
      requestAnimationFrame(() => listRef.current?.scrollTo({ top: listRef.current.scrollHeight }));
    }
  };

  if (targets.length === 0 || modelOptions.length === 0) {
    return <Empty>this provider has no exposed models yet — expose one above, then test it here</Empty>;
  }
  return (
    <div className="pg">
      <div className="pg-pickers">
        <div className="field">
          <label htmlFor="pg-model">model</label>
          <select id="pg-model" value={modelOptions.includes(model) ? model : modelOptions[0]}
            onChange={(e) => onModel(e.target.value)}>
            {modelOptions.map((m) => <option key={m} value={m}>{m}</option>)}
          </select>
        </div>
        <div className="field">
          <label htmlFor="pg-key">Select key</label>
          <select id="pg-key" value={Math.min(keyIndex, Math.max(0, keys.length - 1))}
            onChange={(e) => setKeyIndex(Number(e.target.value))}>
            {keys.map((c, i) => <option key={`${c.suffix}-${i}`} value={i}>{c.label || "key"} …{c.suffix}{c.disabled ? " (disabled)" : ""}</option>)}
          </select>
        </div>
      </div>
      <div className="pg-thread" ref={listRef} aria-live="polite" aria-label="conversation">
        {thread.current.length === 0 && !busy && (
          <Empty>Send a message to start the conversation · Shift+Enter for newline · Enter to send</Empty>
        )}
        {thread.current.map((m, i) => (
          <div key={i} className={`pg-msg ${m.role}`}>
            <span className="pg-role">{m.role === "user" ? "you" : "assistant"}</span>
            <div className="pg-text">{m.text}</div>
          </div>
        ))}
        {busy && <div className="faint pg-thinking">thinking…</div>}
      </div>
      {err && <div className="form-error">{err}</div>}
      {last && thread.current[thread.current.length - 1]?.role === "assistant" && (
        <div className="provider-meta pg-metrics">
          <span><span className="muted">tokens:</span> {last.tok_in} in / {last.tok_out} out</span>
          <span><span className="muted">TTFT:</span> {last.ttft_ms}ms</span>
          <span><span className="muted">duration:</span> {last.dur_ms}ms</span>
          <span><span className="muted">est. cost:</span> ${last.cost_usd.toFixed(6)}</span>
        </div>
      )}
      <form className="pg-input" onSubmit={(e) => { e.preventDefault(); send(); }}>
        <textarea rows={1} aria-label="message" placeholder="Send a message…"
          value={input} disabled={busy}
          onChange={(e) => setInput(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); send(); }
          }} />
        <button className="btn primary" type="submit" disabled={busy || !input.trim()}
          title="send (Enter)"><IconPlay size={14} /></button>
      </form>
    </div>
  );
}
