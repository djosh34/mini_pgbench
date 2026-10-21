<script lang="ts">
  import { onDestroy } from "svelte";
  import { colors, draw } from "./lib/chart";
  import { fetchResults, fetchRunning, merge, ranges, restarted, setRunning, type Row } from "./lib/data";

  const rangeKey = "mini_pgbench.range";
  const pausedKey = "mini_pgbench.paused";
  const pollMs = 2000;

  let rangeLabel = $state(ranges.find((r) => r.label === localStorage.getItem(rangeKey))?.label ?? "15m");
  const range = $derived(ranges.find((r) => r.label === rangeLabel) ?? ranges[1]);
  let paused = $state(localStorage.getItem(pausedKey) === "true");
  let rows = $state.raw<Row[]>([]);
  let running = $state<boolean | null>(null);
  let error = $state<string | null>(null);
  let loaded = $state(false);
  let updatedAt = $state<Date | null>(null);
  let switching = $state(false);

  $effect(() => localStorage.setItem(rangeKey, rangeLabel));
  $effect(() => localStorage.setItem(pausedKey, String(paused)));

  // Polling state. Bumping generation drops the answer of a request still in flight.
  let generation = 0;
  let timer: ReturnType<typeof setTimeout> | undefined;
  let reload = true;
  let lastFetch = 0;

  async function poll() {
    const gen = generation;
    const startedAt = Date.now();
    try {
      let next: Row[];
      if (reload) {
        next = await fetchResults(range.rows);
      } else {
        // Ask for every second since the last good fetch, plus a margin.
        const since = Math.ceil((startedAt - lastFetch) / 1000) + 5;
        const fresh = await fetchResults(range.rows === null ? since : Math.min(since, range.rows));
        next = restarted(rows, fresh) ? await fetchResults(range.rows) : merge(rows, fresh, range.rows);
      }
      const isRunning = await fetchRunning();
      if (gen !== generation) return;
      rows = next;
      running = isRunning;
      reload = false;
      lastFetch = startedAt;
      updatedAt = new Date();
      error = null;
    } catch (e) {
      if (gen !== generation) return;
      error = (e as Error).message;
      running = null;
      reload = true;
    } finally {
      if (gen === generation) {
        loaded = true;
        timer = setTimeout(poll, pollMs);
      }
    }
  }

  // restart reloads the whole range, then polls again unless paused.
  function restart() {
    generation++;
    clearTimeout(timer);
    reload = true;
    if (!paused) poll();
  }

  function chooseRange(r: (typeof ranges)[number]) {
    if (r.label === rangeLabel) return;
    rangeLabel = r.label;
    rows = [];
    loaded = false;
    restart();
  }

  function togglePause() {
    paused = !paused;
    restart();
  }

  async function switchWorkload(start: boolean) {
    switching = true;
    try {
      running = await setRunning(start);
    } catch (e) {
      error = (e as Error).message;
    } finally {
      switching = false;
    }
  }

  restart();
  onDestroy(() => {
    generation++;
    clearTimeout(timer);
  });

  const latest = $derived(rows.at(-1));
  const number = new Intl.NumberFormat();
  const clock = new Intl.DateTimeFormat(undefined, { hour: "2-digit", minute: "2-digit", second: "2-digit" });

  let canvas: HTMLCanvasElement | undefined = $state();
  let width = $state(0);
  let height = $state(0);

  $effect(() => {
    if (!canvas || width === 0 || height === 0) return;
    const to = latest?.t ?? Date.now();
    const from = range.rows === null ? (rows[0]?.t ?? to - 60_000) : to - range.rows * 1000;
    draw(canvas, width, height, rows, from, Math.max(to, from + 1000));
  });
</script>

<div class="mx-auto flex min-h-screen max-w-[1800px] flex-col px-5 py-5 sm:px-8 sm:py-6">
  <header class="flex flex-wrap items-end justify-between gap-x-10 gap-y-5">
    <div class="flex flex-wrap items-end gap-x-10 gap-y-4">
      <h1 class="font-display text-2xl font-semibold tracking-tight text-ink">mini_pgbench</h1>
      <dl class="flex gap-x-8">
        <div>
          <dt class="text-sm text-muted">Total transactions</dt>
          <dd class="font-display text-3xl leading-tight font-semibold text-tps tabular-nums">
            {latest ? number.format(latest.totalTransactions) : "0"}
          </dd>
        </div>
        <div>
          <dt class="text-sm text-muted">Total errors</dt>
          <dd class="font-display text-3xl leading-tight font-semibold text-errors tabular-nums">
            {latest ? number.format(latest.totalErrors) : "0"}
          </dd>
        </div>
      </dl>
    </div>

    <div class="flex items-center gap-3">
      <p class="flex items-center gap-2 text-sm font-medium text-ink" aria-live="polite">
        <span
          class={[
            "size-2.5 rounded-full",
            running === true && "bg-run motion-safe:animate-pulse",
            running === false && "bg-stopped",
            running === null && "bg-muted/40",
          ]}
        ></span>
        {running === null ? "Unknown" : running ? "Running" : "Stopped"}
      </p>
      <div class="flex">
        <button
          class="btn rounded-l-md"
          disabled={switching || running === true}
          onclick={() => switchWorkload(true)}>Start</button
        >
        <button
          class="btn -ml-px rounded-r-md"
          disabled={switching || running === false}
          onclick={() => switchWorkload(false)}>Stop</button
        >
      </div>
    </div>
  </header>

  <div class="mt-6 flex flex-wrap items-center justify-between gap-3">
    <div class="flex" role="group" aria-label="Time range">
      {#each ranges as r, i (r.label)}
        <button
          class={["btn", i === 0 && "rounded-l-md", i === ranges.length - 1 && "rounded-r-md", i > 0 && "-ml-px"]}
          aria-pressed={r.label === rangeLabel}
          onclick={() => chooseRange(r)}>{r.label}</button
        >
      {/each}
    </div>
    <div class="flex items-center gap-4">
      <p class="text-sm text-muted tabular-nums">
        {#if paused}
          Updates paused
        {:else if updatedAt}
          Updated {clock.format(updatedAt)}
        {/if}
      </p>
      <button class="btn rounded-md" aria-pressed={paused} onclick={togglePause}>
        {paused ? "Resume updates" : "Pause updates"}
      </button>
    </div>
  </div>

  {#if error}
    <div role="alert" class="mt-4 border-l-4 border-errors bg-errors/8 px-4 py-3 text-sm text-ink">
      <p class="font-semibold">No data from the benchmark</p>
      <p class="mt-0.5">{error}</p>
      {#if !paused}<p class="mt-0.5 text-muted">Retrying every 2 seconds.</p>{/if}
    </div>
  {/if}

  <section class="mt-4 flex min-h-[360px] flex-1 flex-col border border-grid bg-white">
    <div class="flex flex-wrap items-center gap-x-6 gap-y-1 px-4 pt-3 text-sm text-ink">
      <span class="flex items-center gap-2">
        <span class="h-0.5 w-5" style:background={colors.tps}></span>
        Transactions per second
        <span class="font-display font-semibold tabular-nums">{latest ? number.format(latest.tps) : ""}</span>
      </span>
      <span class="flex items-center gap-2">
        <span class="h-0.5 w-5" style:background={colors.errors}></span>
        Errors per second
        <span class="font-display font-semibold tabular-nums">{latest ? number.format(latest.errors) : ""}</span>
      </span>
    </div>
    <div class="relative flex-1" bind:clientWidth={width} bind:clientHeight={height}>
      <canvas bind:this={canvas} class="absolute inset-0 size-full">
        Chart of transactions and errors per second over the selected time range.
      </canvas>
      {#if loaded && rows.length === 0 && !error}
        <p class="absolute inset-0 grid place-items-center text-sm text-muted">
          No results yet. The benchmark adds one row per second.
        </p>
      {:else if !loaded}
        <p class="absolute inset-0 grid place-items-center text-sm text-muted">Loading {range.label} of results…</p>
      {/if}
    </div>
  </section>
</div>
