import { useEffect, useState } from "react";
import { get, login } from "./api";
import { Toaster } from "./components";
import { TABS, tabFromHash, type Tab } from "./route";
import {
  IconActivity, IconChart, IconGauge, IconList, IconKey, IconServer, IconSettings,
  IconSun, IconMoon,
} from "./icons";
import { LiveTab } from "./tabs/LiveTab";
import { UsageTab } from "./tabs/UsageTab";
import { LatencyTab } from "./tabs/LatencyTab";
import { RequestsTab } from "./tabs/RequestsTab";
import { EndpointsTab } from "./tabs/EndpointsTab";
import { ProvidersTab } from "./tabs/ProvidersTab";
import { SettingsTab } from "./tabs/SettingsTab";

// read once at module init: the pw-login flow clears the hash before tabs mount
const BOOT = new URLSearchParams(location.hash.replace(/^#\/?/, ""));

const NAV: { group: string; tabs: { id: Tab; label: string; icon: React.ReactNode }[] }[] = [
  {
    group: "Proxy", tabs: [
      { id: "live", label: "Live", icon: <IconActivity /> },
      { id: "usage", label: "Usage", icon: <IconChart /> },
      { id: "latency", label: "Latency", icon: <IconGauge /> },
      { id: "requests", label: "Requests", icon: <IconList /> },
    ],
  },
  {
    group: "Access", tabs: [
      { id: "endpoints", label: "Endpoints", icon: <IconKey /> },
      { id: "providers", label: "Providers", icon: <IconServer /> },
    ],
  },
  { group: "System", tabs: [{ id: "settings", label: "Settings", icon: <IconSettings /> }] },
];

// #/tab in the hash keeps the tab across refresh/back; legacy #tab=... and #pw=... still honored
const tabFromLocation = (): Tab => tabFromHash(location.hash);

export default function App() {
  const [authed, setAuthed] = useState<boolean | null>(null);
  const [tab, setTab] = useState<Tab>(tabFromLocation);
  const [theme, setTheme] = useState<string>(() => document.documentElement.dataset.theme || "light");
  const [version, setVersion] = useState("");

  // deep link: #pw=<password>&tab=<tab> (hash stays client-side; also enables
  // headless captures). pw is consumed and cleared from the hash after login.
  useEffect(() => {
    const pw = BOOT.get("pw");
    if (pw) {
      history.replaceState(null, "", location.pathname);
      login(pw).then(() => setAuthed(true)).catch(() => setAuthed(false));
      return;
    }
    get("summary?hours=1").then(() => setAuthed(true)).catch(() => setAuthed(false));
  }, []);

  // two-way tab <-> URL hash
  useEffect(() => {
    const onHash = () => setTab(tabFromLocation());
    window.addEventListener("hashchange", onHash);
    return () => window.removeEventListener("hashchange", onHash);
  }, []);
  const selectTab = (t: Tab) => {
    setTab(t);
    history.pushState(null, "", `#/${t}`);
  };

  useEffect(() => { document.documentElement.dataset.theme = theme; }, [theme]);
  useEffect(() => {
    get("version").then((v: { version: string }) => setVersion(v.version)).catch(() => {});
  }, []);

  if (authed === null) return <div className="empty">loading…</div>;
  if (!authed) return <Login onOk={() => setAuthed(true)} />;

  return (
    <div className="shell">
      <nav className="side" aria-label="main navigation">
        <div className="brand">agent-router</div>
        {NAV.map((g) => (
          <div key={g.group} className="nav-group-wrap">
            <div className="nav-group">{g.group}</div>
            {g.tabs.map((t) => (
              <button key={t.id} role="tab" aria-selected={tab === t.id} className="nav-item" aria-label={t.label}
                onClick={() => selectTab(t.id)}>
                {t.icon}<span>{t.label}</span>
              </button>
            ))}
          </div>
        ))}
        <div className="side-foot">
          <button className="icon-btn" aria-label="toggle theme" title="toggle theme"
            onClick={() => setTheme(theme === "dark" ? "light" : "dark")}>
            {theme === "dark" ? <IconSun /> : <IconMoon />}
          </button>
          <div className="ver faint">{version ? `v${version}` : ""}</div>
        </div>
      </nav>
      <main>
        {tab === "live" && <LiveTab />}
        {tab === "usage" && <UsageTab />}
        {tab === "latency" && <LatencyTab />}
        {tab === "requests" && <RequestsTab />}
        {tab === "endpoints" && <EndpointsTab />}
        {tab === "providers" && <ProvidersTab />}
        {tab === "settings" && <SettingsTab />}
      </main>
      <Toaster />
    </div>
  );
}

function Login({ onOk }: { onOk: () => void }) {
  const [pw, setPw] = useState("");
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);
  return (
    <form className="login card" onSubmit={(e) => {
      e.preventDefault();
      setBusy(true);
      login(pw).then(onOk).catch(() => { setErr("wrong password"); setBusy(false); });
    }}>
      <h3>agent-router</h3>
      <input type="password" placeholder="admin password" value={pw} autoFocus
        onChange={(e) => setPw(e.target.value)} />
      {err && <div className="error">{err}</div>}
      <button className="btn primary" type="submit" disabled={busy || !pw}>{busy ? "signing in…" : "Sign in"}</button>
    </form>
  );
}
