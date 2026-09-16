import { useEffect, useRef, useState } from "react";
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
  series: { key: "requests" | "tok_in" | "tok_out" | "cost" | "errors"; label: string; scale?: "left" | "right" }[];
}) {
  const el = useRef<HTMLDivElement>(null);
  const plot = useRef<uPlot | null>(null);

  useEffect(() => {
    if (!el.current) return;
    if (data.length === 0) {
      if (plot.current) { plot.current.destroy(); plot.current = null; }
      return;
    }
    const build = () => {
      if (!el.current) return null;
      // chart colors follow the active theme via CSS custom properties
      const cs = getComputedStyle(el.current);
      const v = (name: string, fallback: string) => cs.getPropertyValue(name).trim() || fallback;
      const c1 = v("--chart1", "#115E59"), c2 = v("--chart2", "#A8A29E");
      const axis = v("--axis", "#78716C"), grid = v("--grid", "#E7E5E4");
      const fill = v("--chart-fill", "rgba(17,94,89,0.08)");
      const xs = data.map((d) => d.ts / 1000);
      const charts = series.map((s) => data.map((d) => d[s.key] as number));
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
      return new uPlot(opts, [xs, ...charts], el.current);
    };
    plot.current?.destroy();
    // build after layout settles (fonts/grid), then track resizes
    let ro: ResizeObserver | null = null;
    requestAnimationFrame(() => requestAnimationFrame(() => {
      if (!el.current) return;
      plot.current = build();
      ro = new ResizeObserver(() => {
        if (!el.current || !plot.current) return;
        const w = el.current.clientWidth;
        if (w > 0 && Math.abs(w - plot.current.width) > 2) {
          plot.current.destroy();
          plot.current = build();
        }
      });
      ro.observe(el.current);
    }));
    return () => { ro?.disconnect(); plot.current?.destroy(); plot.current = null; };
  }, [data, height, JSON.stringify(series)]);

  if (data.length === 0) {
    return <div className="empty">no data in range</div>;
  }
  return <div className="chart" ref={el} />;
}
