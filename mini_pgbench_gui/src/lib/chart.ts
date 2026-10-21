import type { Row } from "./data";

export const colors = {
  paper: "#f4f7fb",
  minorGrid: "#e1e9f2",
  majorGrid: "#c4d3e3",
  axisText: "#5b6b80",
  tps: "#1f4fbf",
  errors: "#d6332b",
};

const margin = { top: 12, right: 16, bottom: 28, left: 56 };
const labelFont = '12px "Bahnschrift", "DIN Alternate", "Roboto Condensed", "Arial Narrow", sans-serif';

// niceStep returns a round step (1, 2 or 5 times a power of ten) that splits span into about count parts.
function niceStep(span: number, count: number): number {
  const raw = span / count;
  const power = 10 ** Math.floor(Math.log10(raw));
  const fraction = raw / power;
  return (fraction <= 1 ? 1 : fraction <= 2 ? 2 : fraction <= 5 ? 5 : 10) * power;
}

const timeSteps = [1, 2, 5, 10, 15, 30, 60, 120, 300, 600, 900, 1800, 3600, 7200, 10800, 21600, 43200, 86400].map(
  (s) => s * 1000,
);

function formatTime(t: number, step: number): string {
  const d = new Date(t);
  const hh = String(d.getHours()).padStart(2, "0");
  const mm = String(d.getMinutes()).padStart(2, "0");
  const ss = String(d.getSeconds()).padStart(2, "0");
  return step < 60_000 ? `${hh}:${mm}:${ss}` : `${hh}:${mm}`;
}

// Column holds the lowest and highest value of one series in one pixel column.
type Column = { min: number; max: number };

// columns reduces rows to the min and max of each series per pixel column, so
// 86,400 rows cost one pass and at most width points per line.
function columns(rows: Row[], from: number, to: number, width: number) {
  const tps: (Column | undefined)[] = new Array(width);
  const errors: (Column | undefined)[] = new Array(width);
  const scale = (width - 1) / Math.max(to - from, 1);
  for (const r of rows) {
    if (r.t < from || r.t > to) continue;
    const x = Math.round((r.t - from) * scale);
    const a = tps[x];
    if (a) {
      a.min = Math.min(a.min, r.tps);
      a.max = Math.max(a.max, r.tps);
    } else tps[x] = { min: r.tps, max: r.tps };
    const b = errors[x];
    if (b) {
      b.min = Math.min(b.min, r.errors);
      b.max = Math.max(b.max, r.errors);
    } else errors[x] = { min: r.errors, max: r.errors };
  }
  return { tps, errors };
}

function drawLine(
  ctx: CanvasRenderingContext2D,
  cols: (Column | undefined)[],
  x0: number,
  y: (v: number) => number,
  color: string,
) {
  ctx.beginPath();
  let started = false;
  cols.forEach((c, i) => {
    if (!c) return;
    const x = x0 + i + 0.5;
    if (started) ctx.lineTo(x, y(c.max));
    else ctx.moveTo(x, y(c.max));
    started = true;
    if (c.min !== c.max) ctx.lineTo(x, y(c.min));
  });
  ctx.strokeStyle = color;
  ctx.lineWidth = 1.75;
  ctx.lineJoin = "round";
  ctx.stroke();
}

// draw paints the chart for the time window [from, to] onto a canvas of
// width by height CSS pixels.
export function draw(
  canvas: HTMLCanvasElement,
  width: number,
  height: number,
  rows: Row[],
  from: number,
  to: number,
) {
  const ratio = window.devicePixelRatio || 1;
  canvas.width = Math.round(width * ratio);
  canvas.height = Math.round(height * ratio);
  const ctx = canvas.getContext("2d");
  if (!ctx) return;
  ctx.setTransform(ratio, 0, 0, ratio, 0, 0);
  ctx.clearRect(0, 0, width, height);

  const plotW = Math.max(Math.floor(width - margin.left - margin.right), 1);
  const plotH = Math.max(height - margin.top - margin.bottom, 1);
  const { tps, errors } = columns(rows, from, to, plotW);

  let top = 0;
  for (const c of tps) if (c) top = Math.max(top, c.max);
  for (const c of errors) if (c) top = Math.max(top, c.max);
  const yStep = niceStep(Math.max(top, 10), 5);
  const yMax = Math.ceil(Math.max(top, 10) / yStep) * yStep;
  const y = (v: number) => margin.top + plotH - (v / yMax) * plotH;
  const x = (t: number) => margin.left + ((t - from) / Math.max(to - from, 1)) * (plotW - 1) + 0.5;

  // Graph paper: five minor lines per labelled line.
  ctx.lineWidth = 1;
  ctx.strokeStyle = colors.minorGrid;
  ctx.beginPath();
  for (let v = yStep / 5; v < yMax; v += yStep / 5) {
    const py = Math.round(y(v)) + 0.5;
    ctx.moveTo(margin.left, py);
    ctx.lineTo(margin.left + plotW, py);
  }
  ctx.stroke();

  const tStep = timeSteps.find((s) => (to - from) / s <= Math.max(plotW / 110, 2)) ?? 86_400_000;
  const offset = new Date().getTimezoneOffset() * 60_000;
  const firstTick = Math.ceil((from - offset) / tStep) * tStep + offset;

  ctx.strokeStyle = colors.majorGrid;
  ctx.beginPath();
  for (let v = 0; v <= yMax; v += yStep) {
    const py = Math.round(y(v)) + 0.5;
    ctx.moveTo(margin.left, py);
    ctx.lineTo(margin.left + plotW, py);
  }
  for (let t = firstTick; t <= to; t += tStep) {
    const px = Math.round(x(t)) + 0.5;
    ctx.moveTo(px, margin.top);
    ctx.lineTo(px, margin.top + plotH);
  }
  ctx.stroke();

  ctx.font = labelFont;
  ctx.fillStyle = colors.axisText;
  ctx.textAlign = "right";
  ctx.textBaseline = "middle";
  for (let v = 0; v <= yMax; v += yStep) ctx.fillText(v.toLocaleString(), margin.left - 8, y(v));
  ctx.textAlign = "center";
  ctx.textBaseline = "top";
  for (let t = firstTick; t <= to; t += tStep) {
    const px = x(t);
    if (px < margin.left + 24 || px > margin.left + plotW - 24) continue;
    ctx.fillText(formatTime(t, tStep), px, margin.top + plotH + 8);
  }

  ctx.save();
  ctx.beginPath();
  ctx.rect(margin.left, margin.top - 2, plotW, plotH + 4);
  ctx.clip();
  drawLine(ctx, tps, margin.left, y, colors.tps);
  drawLine(ctx, errors, margin.left, y, colors.errors);
  ctx.restore();
}
