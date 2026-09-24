import { useEffect, useRef } from "react";
import uPlot from "uplot";
import "uplot/dist/uPlot.min.css";

// Hover tooltip: crosshair + one row per series at the cursor (replaces
// legend.live, which used to rewrite the legend row under the mouse).
function tooltipPlugin() {
  const tip = document.createElement("div");
  tip.className = "u-tip";
  tip.style.display = "none";
  let attached = false;
  return {
    hooks: {
      // the plot instance only exists inside hooks — attach the tip on first use
      setCursor: [
        (u: uPlot) => {
          if (!attached) { u.over.appendChild(tip); attached = true; }
          show(u);
        },
      ],
      setData: [hide],
    },
  };

  function show(u: uPlot) {
    const i = u.cursor.idx;
    if (i == null) { hide(); return; }
    const fmtTs = (t: number) => new Date(t * 1000).toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
    const compact = (v: number) =>
      Math.abs(v) >= 1e6 ? (v / 1e6).toFixed(1) + "M"
      : Math.abs(v) >= 1e3 ? (v / 1e3).toFixed(Math.abs(v) >= 1e4 ? 0 : 1) + "k"
      : String(Math.round(v));
    const money = (v: number) => "$" + (Math.abs(v) >= 1 ? v.toFixed(2) : v >= 0.01 ? v.toFixed(2) : v.toPrecision(2));
    const t = (u.data[0][i] ?? 0) * 1000;
    let rows = "";
    for (let s = 1; s < u.series.length; s++) {
      const v = u.data[s]?.[i];
      const ser = u.series[s];
      if (v == null) continue;
      const dollar = (ser.scale ?? "y") === "2";
      rows += `<div class="u-tip-row"><span class="u-tip-dot" style="background:${ser.stroke}"></span>` +
        `<span class="u-tip-label">${typeof ser.label === "string" ? ser.label : ""}</span>` +
        `<span class="u-tip-val">${dollar ? money(v) : compact(v)}</span></div>`;
    }
    tip.innerHTML = `<div class="u-tip-head">${fmtTs(t)}</div>${rows}`;
    tip.style.display = "";
    const left = u.cursor.left ?? 0, top = u.cursor.top ?? 0;
    const w = tip.offsetWidth, h = tip.offsetHeight;
    const flipX = left + w + 16 > u.over.offsetWidth;
    const flipY = top + h + 16 > u.over.offsetHeight;
    tip.style.left = (flipX ? left - w - 10 : left + 10) + "px";
    tip.style.top = (flipY ? Math.max(0, top - h - 10) : top + 10) + "px";
  }
  function hide() { tip.style.display = "none"; }
}

// Ponytail: replaced the Summary-typed import — TimeChart now takes generic
// point rows (ts + arbitrary numeric keys) so model pivots can share it.
export function TimeChart({
  data,
  height = 220,
  series,
  smooth = false,
}: {
  data: { ts: number; [k: string]: number }[];
  height?: number;
  series: { key: string; label: string; axis?: 2 }[]; // axis: 2 → right-hand axis (own scale)
  smooth?: boolean; // spline paths — for cumulative-ish token/cost lines, not spiky request/error series
}) {
  const el = useRef<HTMLDivElement>(null);
  const plot = useRef<uPlot | null>(null);
  // latest committed payload: theme rebuilds run with narrow deps and must not redraw a render that never committed
  const dataRef = useRef(data);
  type Point = { ts: number; [k: string]: number };
  const aligned = (d: Point[]): uPlot.AlignedData =>
    [d.map((p) => p.ts / 1000), ...series.map((s) => d.map((p) => p[s.key] as number))];

  // build once per config (presence, height, series set, point mode); NOT per data tick
  useEffect(() => {
    if (!el.current || data.length === 0) return;
    const build = (payload: uPlot.AlignedData) => {
      if (!el.current) return null;
      // chart colors follow the active theme via CSS custom properties
      const cs = getComputedStyle(el.current);
      const v = (name: string, fallback: string) => cs.getPropertyValue(name).trim() || fallback;
      const c1 = v("--chart1", "#115E59"), c2 = v("--chart2", "#A8A29E");
      const colors = series.map((_, i) => {
        const c = v(`--chart${i + 1}`, "");
        return i === 0 ? c1 : c || c2;
      });
      const axis = v("--axis", "#78716C"), grid = v("--grid", "#E7E5E4");
      const fill = v("--chart-fill", "rgba(17,94,89,0.08)");
      const compact = (v: number) =>
        Math.abs(v) >= 1e6 ? (v / 1e6).toFixed(1) + "M"
        : Math.abs(v) >= 1e3 ? (v / 1e3).toFixed(v >= 1e4 ? 0 : 1) + "k"
        : String(Math.round(v));
      const dual = series.some((s) => s.axis === 2);
      const opts: uPlot.Options = {
        width: el.current.clientWidth,
        height,
        scales: { x: { time: true } },
        axes: [
          { stroke: axis, grid: { stroke: grid, width: 0.5 } },
          // dual: token scale keeps a LEFT value axis (side 3) while cost takes
          // the right; without an axis mapped to scale "y" uPlot draws no tick
          // labels for the token series at all
          ...(dual ? [{ stroke: axis, grid: { stroke: grid, width: 0.5 }, side: 3 as const,
                      values: (u: uPlot, vals: number[]) => vals.map(compact) }] : []),
          dual
            ? { stroke: axis, grid: { show: false }, side: 1, scale: "2",
                values: (u: uPlot, vals: number[]) => vals.map((x) => "$" + (Math.abs(x) >= 100 ? compact(x) : x >= 1 ? x.toFixed(2) : x >= 0.1 ? x.toFixed(2) : x.toFixed(3))) }
            : { stroke: axis, grid: { stroke: grid, width: 0.5 }, side: 1,
                values: (u: uPlot, vals: number[]) => vals.map(compact) },
        ],
        series: [
          {},
          ...series.map((s, i) => ({
            label: s.label,
            stroke: colors[i],
            width: 1.5,
            fill: i === 0 ? fill : undefined,
            spanGaps: true,
            scale: s.axis === 2 ? "2" : "y",
            points: { show: data.length < 30 },
            ...(smooth ? { paths: uPlot.paths.spline?.() } : {}),
          })),
        ],
        legend: { show: true, live: false }, // static key; hover values live in the tooltip plugin
        plugins: [tooltipPlugin()],
      };
      return new uPlot(opts, payload, el.current);
    };
    plot.current?.destroy();
    plot.current = build(aligned(data));
    // resizes: resize the existing plot, never rebuild
    const ro = new ResizeObserver(() => {
      if (!el.current || !plot.current) return;
      const w = el.current.clientWidth;
      if (w > 0 && Math.abs(w - plot.current.width) > 2) {
        plot.current.setSize({ width: w, height });
      }
    });
    ro.observe(el.current);
    // chart colors are read from CSS vars at build time — rebuild on theme change
    const mo = new MutationObserver(() => {
      plot.current?.destroy();
      plot.current = build(aligned(dataRef.current)); // committed data only — closure `data` is stale here
    });
    mo.observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme"] });
    return () => { ro.disconnect(); mo.disconnect(); plot.current?.destroy(); plot.current = null; };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [data.length > 0, height, JSON.stringify(series), data.length < 30, smooth]);

  // poll ticks: update data in place — same canvas, no teardown, no scroll shift
  useEffect(() => {
    dataRef.current = data; // committed only after this runs — theme rebuilds read it
    if (plot.current && data.length > 0) plot.current.setData(aligned(data));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [data]);

  if (data.length === 0) {
    return <div className="empty">no data in range</div>;
  }
  return <div className="chart" ref={el} />;
}
