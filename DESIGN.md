# DESIGN.md — yardmaster dashboard

```yaml
name: yardmaster-dashboard
type: web
colors:
  bg: "#FAFAF9"          # page
  surface: "#FFFFFF"     # cards
  surface2: "#F5F5F4"    # table headers, hovers
  ink: "#1C1917"         # primary text
  muted: "#49433F"       # secondary text (APCA Lc 86 on surface2)
  faint: "#49433F"       # timestamps, disabled
  line: "#E7E5E4"        # borders
  accent: "#115E59"      # teal-800 — links, active tab, primary buttons, chart primary
  accentHover: "#0D4A46" # primary button hover
  accentSoft: "#CCFBF1"  # accent tint for badges
  ok: "#134E4A"          # 2xx status text, live dot
  warn: "#92400E"        # 4xx
  warnSoft: "#FEF3C7"    # warn badge bg
  danger: "#7F1D1D"      # 5xx, errors
  dangerSoft: "#FEE2E2"  # danger badge bg
typography:
  family: system-ui, -apple-system, "Segoe UI", sans-serif
  mono: ui-monospace, "SF Mono", Menlo, monospace
  scale: [12, 14, 16, 20, 25]   # base 14 (data density), ratio ~1.25
  numerics: tabular-nums
spacing: [4, 8, 12, 16, 24, 32] # 8px grid, 4 = half-step inside components
radius: { sm: 6, md: 10 }
elevation:
  flat: none                    # tables, inline blocks
  sm: 0 1px 2px rgb(28 25 23 / 0.06)   # cards
  md: 0 4px 12px rgb(28 25 23 / 0.10)  # popover/live panel
motion:
  live-dot: pulse 2s            # the ONLY looping animation
  micro: background/border-color .12s ease on interactive elements
  reduced-motion: disable pulse
```

## Overview

Ops dashboard for a self-hosted LLM proxy. Data-first, calm, printable-light.
One accent (teal) carries brand + chart primary; status colors carry meaning only
(ok/warn/danger). No decoration that isn't data.

## Layout

- Sidebar nav (sticky): brand; groups Proxy (Live, Usage, Quota, Latency, Requests),
  Access (Endpoints, Providers), System (Settings); theme toggle + version in the footer.
  Collapses to icons below 800px.
- Content: fluid — fills the browser width, no max-width; 24px/32px gutters (16px below 600px).
- Tables: progressive column disclosure — `.col-lg` hides <1200px, `.col-md` hides <900px;
  identity + status columns always stay visible. Horizontal scroll is the last resort.
- Cards: `surface` bg, `sm` elevation, `md` radius, 16px padding. Section titles 16/600.

## Components

- **StatCard**: label (12 muted) + value (25 tabular) + delta line (12).
- **Table**: header row `surface2`, 12px uppercase? NO — 13px/600 sentence case;
  rows 14px, right-aligned numerics, row hover `surface2`, no zebra.
- **Chart**: uPlot; series colors are the categorical token set `--chart1…6`
  (light: teal/stone/amber/indigo/rose/moss 700-800 steps; dark: lighter 300-400
  steps) — data encoding only, never decoration; adjacent series must be
  nameably different. A series with `axis: 2` gets a right-hand axis with its
  own scale (e.g. cost in $). Legend is live (hover = per-date values). Grid
  `line` at 0.5px; empty state shows "no data in range" text, never a blank box.
- **Activity heatmap** (Usage): one column per week, Mon-first 7 rows, aspect-square
  cells, 4 intensity levels as `color-mix` steps of `--accent` over `--surface2`
  (0 = empty surface2); per-cell `title` carries date + value. Day labels, not
  month rulers.
- **Share bar** (breakdown tables): `.quota-bar` at 64px + % text, share of total
  input tokens; sorted implicitly with the token column.
- **Badge**: pill, `accentSoft` bg + accent text (ok/active), `surface2` + muted (neutral).
- **Button**: primary = accent bg/white text; secondary = surface + `line` border;
  focus-visible: 2px accent outline offset 2; disabled: 40% opacity.
- **Input**: surface bg, `line` border, `sm` radius; error state: danger border + message.
- **Segmented control** (`.seg`): connected buttons; `accent-soft` bg + accent text for
  `aria-pressed="true"`. Filter toolbars use it instead of stacks of primary buttons.
- **Connection/account row**: identity is the stored label (`"<CODE> <label>"`) ONLY —
  the masked key suffix is a hover tooltip (`title`), never visible text. Destructive
  actions (remove connection / API key / provider) always confirm via modal.

## States (contract)

Every interactive element: default / hover / focus-visible / disabled.
Tables: row hover. Live feed: reconnecting state shows muted "reconnecting…" text.
All motion respects `prefers-reduced-motion` (pulse → static).

## Do's & Don'ts

- DO use tabular-nums for every number wider than 3 digits.
- DO right-align numerics in tables; left-align labels.
- DO show key/account identity by label only; suffix on hover.
- DO drop secondary table columns on narrow screens (`.col-md`/`.col-lg`) before scrolling.
- DON'T color whole rows by status — color the status cell only.
- DON'T introduce a second accent, gradients, or dark theme.
- DON'T shadow-stack: cards get `sm`, nothing else gets a shadow.
- Chart fills ≥3:1 against track; secondary series muted, today/accent full.
