// One row of GET /results. The benchmark writes one row per second.
export type Row = {
  t: number; // timestamp in ms since the epoch
  tps: number;
  errors: number;
  totalTransactions: number;
  totalErrors: number;
};

type ApiRow = {
  timestamp: string;
  tps: number;
  errors: number;
  total_transactions: number;
  total_errors: number;
};

export type Range = { label: string; rows: number | null };

export const ranges: Range[] = [
  { label: "5m", rows: 300 },
  { label: "15m", rows: 900 },
  { label: "1h", rows: 3600 },
  { label: "6h", rows: 21600 },
  { label: "24h", rows: 86400 },
  { label: "all", rows: null },
];

async function request(path: string, init?: RequestInit): Promise<unknown> {
  let res: Response;
  try {
    res = await fetch("/api" + path, init);
  } catch {
    throw new Error("The GUI server does not respond. Check that mini_pgbench_gui is still running.");
  }
  const body: unknown = await res.json().catch(() => null);
  if (!res.ok) {
    const message = (body as { error?: string } | null)?.error;
    throw new Error(message ?? `${path} returned HTTP ${res.status}`);
  }
  return body;
}

// fetchResults returns the latest limit rows, or all rows when limit is null.
export async function fetchResults(limit: number | null): Promise<Row[]> {
  const query = limit === null ? "" : `?limit=${limit}`;
  const body = (await request("/results" + query)) as ApiRow[];
  return body.map((r) => ({
    t: Date.parse(r.timestamp),
    tps: r.tps,
    errors: r.errors,
    totalTransactions: r.total_transactions,
    totalErrors: r.total_errors,
  }));
}

export async function fetchRunning(): Promise<boolean> {
  const body = (await request("/status")) as { running: boolean };
  return body.running;
}

export async function setRunning(running: boolean): Promise<boolean> {
  const body = (await request(running ? "/start" : "/stop", { method: "POST" })) as { running: boolean };
  return body.running;
}

// merge adds fresh rows to cached rows. Rows with the same timestamp are
// replaced. Both arrays are sorted by time. Only the latest keep rows stay,
// unless keep is null.
export function merge(cached: Row[], fresh: Row[], keep: number | null): Row[] {
  if (fresh.length === 0) return cached;
  let cut = cached.length;
  while (cut > 0 && cached[cut - 1].t >= fresh[0].t) cut--;
  const rows = cached.slice(0, cut).concat(fresh);
  return keep === null || rows.length <= keep ? rows : rows.slice(rows.length - keep);
}

// restarted reports whether fresh rows come from a new benchmark process.
// The totals only grow within one process, so a drop means a restart.
export function restarted(cached: Row[], fresh: Row[]): boolean {
  if (cached.length === 0 || fresh.length === 0) return false;
  const before = cached[cached.length - 1];
  const after = fresh[fresh.length - 1];
  return after.totalTransactions < before.totalTransactions || after.totalErrors < before.totalErrors;
}
