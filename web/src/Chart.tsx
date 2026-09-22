import { useEffect, useRef } from "react";
import uPlot from "uplot";
import "uplot/dist/uPlot.min.css";
import { Summary } from "./api";

export function TimeChart({
  data,
  height = 220,
  series,
}: {
  data: Summary["series"];
  height?: number;
  series: { key: "requests" | "tok_in" | "tok_out" | "cost" | "errors"; label: string }[];
}) {
  const el = useRef<HTMLDivElement>(null);
  const plot = useRef<uPlot | null>(null);
  // latest committed payload: theme rebuilds run with narrow deps and must not redraw a render that never committed
  const dataRef = useRef(data);
  const aligned = (d: Summary["series"]): uPlot.AlignedData =>
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
      const axis = v("--axis", "#78716C"), grid = v("--grid", "#E7E5E4");
      const fill = v("--chart-fill", "rgba(17,94,89,0.08)");
      const compact = (v: number) =>
        Math.abs(v) >= 1e6 ? (v / 1e6).toFixed(1) + "M"
        : Math.abs(v) >= 1e3 ? (v / 1e3).toFixed(v >= 1e4 ? 0 : 1) + "k"
        : String(Math.round(v));
      const opts: uPlot.Options = {
        width: el.current.clientWidth,
        height,
        scales: { x: { time: true } },
        axes: [
          { stroke: axis, grid: { stroke: grid, width: 0.5 } },
          { stroke: axis, grid: { stroke: grid, width: 0.5 }, side: 1,
            values: (u: uPlot, vals: number[]) => vals.map(compact) },
        ],
        series: [
          {},
          ...series.map((s, i) => ({
            label: s.label,
            stroke: i === 0 ? c1 : c2,
            width: 1.5,
            fill: i === 0 ? fill : undefined,
            spanGaps: true,
            points: { show: data.length < 30 },
          })),
        ],
        legend: { show: true, live: false },
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
  }, [data.length > 0, height, JSON.stringify(series), data.length < 30]);

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
