# DESIGN.md — yardmaster dashboard

```yaml
name: yardmaster-dashboard
type: web
colors:
  bg: "#FAFAF9"          # page
  surface: "#FFFFFF"     # cards
  surface2: "#F5F5F4"    # table headers, hovers
  ink: "#1C1917"         # primary text
  muted: "#57534E"       # secondary text (7.4:1 on bg)
  faint: "#5D5651"       # timestamps, disabled
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
  reduced-motion: disable pulse
```

## Overview

Ops dashboard for a self-hosted LLM proxy. Data-first, calm, printable-light.
One accent (teal) carries brand + chart primary; status colors carry meaning only
(ok/warn/danger). No decoration that isn't data.

## Layout

- Header: product name left, live inflight counter + reload right. Sticky.
- Tab bar under header: Live, Usage, Latency, Requests, Keys, Providers, Settings.
- Content: max-width 1200px, 24px gutters.
- Cards: `surface` bg, `sm` elevation, `md` radius, 16px padding. Section titles 16/600.

## Components

- **StatCard**: label (12 muted) + value (25 tabular) + delta line (12).
- **Table**: header row `surface2`, 12px uppercase? NO — 13px/600 sentence case;
  rows 14px, right-aligned numerics, row hover `surface2`, no zebra.
- **Chart**: uPlot; series: accent (primary), faint (secondary); grid `line` at 0.5px;
  legend inline; empty state shows "no data in range" text, never a blank box.
- **Badge**: pill, `accentSoft` bg + accent text (ok/active), `surface2` + muted (neutral).
- **Button**: primary = accent bg/white text; secondary = surface + `line` border;
  focus-visible: 2px accent outline offset 2; disabled: 40% opacity.
- **Input**: surface bg, `line` border, `sm` radius; error state: danger border + message.

## States (contract)

Every interactive element: default / hover / focus-visible / disabled.
Tables: row hover. Live feed: reconnecting state shows muted "reconnecting…" text.
All motion respects `prefers-reduced-motion` (pulse → static).

## Do's & Don'ts

- DO use tabular-nums for every number wider than 3 digits.
- DO right-align numerics in tables; left-align labels.
- DON'T color whole rows by status — color the status cell only.
- DON'T introduce a second accent, gradients, or dark theme.
- DON'T shadow-stack: cards get `sm`, nothing else gets a shadow.
- Chart fills ≥3:1 against track; secondary series muted, today/accent full.
