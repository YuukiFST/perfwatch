"use strict";

// Colors from the dashboard tokens in app.css; the canvas cannot read CSS variables.
const color = {
  text: "#ededed", muted: "#a0a0a0", border: "#2e2e2e",
  cpu: "#52a9ff", ram: "#bf7af0", peak: "#e5484d",
  peakFill: "rgba(229, 72, 77, 0.18)", preRollFill: "rgba(160, 160, 160, 0.12)",
};

const pctFormat = new Intl.NumberFormat("pt-BR", { style: "percent", minimumFractionDigits: 1, maximumFractionDigits: 1 });
const oneDecimal = new Intl.NumberFormat("pt-BR", { minimumFractionDigits: 1, maximumFractionDigits: 1 });
const integer = new Intl.NumberFormat("pt-BR");
const dateTime = new Intl.DateTimeFormat("pt-BR", { dateStyle: "short", timeStyle: "medium" });
const timeOnly = new Intl.DateTimeFormat("pt-BR", { timeStyle: "medium" });
const axisTime = new Intl.DateTimeFormat("pt-BR", { hour: "2-digit", minute: "2-digit" });
const axisSeconds = new Intl.DateTimeFormat("pt-BR", { hour: "2-digit", minute: "2-digit", second: "2-digit" });
const axisDay = new Intl.DateTimeFormat("pt-BR", { day: "2-digit", month: "2-digit" });

const pct = (v) => (v == null ? "—" : pctFormat.format(v / 100));
const when = (d) => dateTime.format(d);

function duration(seconds) {
  const s = Math.round(seconds);
  if (s < 60) return `${s} s`;
  const m = Math.floor(s / 60);
  if (m < 60) return s % 60 ? `${m} min ${s % 60} s` : `${m} min`;
  const h = Math.floor(m / 60);
  return m % 60 ? `${h} h ${m % 60} min` : `${h} h`;
}

function bytes(b) {
  const mib = 1024 * 1024;
  if (b >= 1024 * mib) return `${oneDecimal.format(b / (1024 * mib))} GiB`;
  return `${integer.format(Math.round(b / mib))} MiB`;
}

const triggerText = { cpu: "pico de CPU", ram: "pico de RAM", "cpu+ram": "pico de CPU e RAM" };

const $ = (id) => document.getElementById(id);

function el(tag, attrs, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (k === "class") node.className = v;
    else node.setAttribute(k, v);
  }
  node.append(...children);
  return node;
}

function cell(text, label, numeric) {
  return el("td", { class: numeric ? "num" : "", "data-label": label }, text);
}

// An exported report carries every answer the page asks for, and has no server behind it.
const report = window.PERFWATCH_REPORT;

function reportAnswer(url) {
  const u = new URL(url, "http://report/");
  if (u.pathname === "/api/timeline") return report.timeline;
  if (u.pathname === "/api/ranking") return u.searchParams.get("peaks_only") === "true" ? report.ranking_peaks : report.ranking;
  const id = u.pathname.match(/^\/api\/episodes\/(\d+)$/)?.[1];
  if (id && report.episodes[id]) return report.episodes[id];
  throw new Error("este relatório não guarda esses dados");
}

async function getJSON(url) {
  if (report) return reportAnswer(url);
  const res = await fetch(url, { headers: { Accept: "application/json" } });
  const body = await res.json();
  if (!res.ok) throw new Error(body.error || res.statusText);
  return body;
}

// The range shown, and the ranges zoomed out of, newest last.
// hours is the range button that chose the shown range, null after a zoom.
const state = { from: null, to: null, hours: "24", zoomed: [], timeline: null, episode: null };

function query(from, to) {
  const q = new URLSearchParams();
  if (from) q.set("from", from.toISOString());
  if (to) q.set("to", to.toISOString());
  return q;
}

// Each kind of request counts its calls, so an answer that arrives after a newer one is dropped.
const latest = { timeline: 0, ranking: 0, episode: 0 };

async function fresh(kind, url) {
  const n = ++latest[kind];
  const body = await getJSON(url);
  return n === latest[kind] ? body : null;
}

// onLoaded runs only once the range has loaded, so a failed load leaves the zoom history as it was.
async function load(from, to, onLoaded) {
  $("status").textContent = "";
  try {
    const timeline = await fresh("timeline", "api/timeline?" + query(from, to));
    if (!timeline) return;
    onLoaded?.();
    state.timeline = timeline;
    state.from = new Date(timeline.from);
    state.to = new Date(timeline.to);
    $("zoom-out").disabled = state.zoomed.length === 0;
    $("range-label").textContent = `${when(state.from)} a ${when(state.to)}` +
      (report ? ` · relatório gerado em ${when(new Date(report.generated))}` : "");
    renderTimeline(timeline);
    renderEpisodeList(timeline.episodes);
    await loadRanking();
  } catch (err) {
    $("status").textContent = `Não foi possível carregar os dados: ${err.message}`;
  }
}

async function loadRanking() {
  const q = query(state.from, state.to);
  if ($("peaks-only").checked) q.set("peaks_only", "true");
  const ranking = await fresh("ranking", "api/ranking?" + q);
  if (ranking) renderRanking(ranking);
}

// Marks the range button that chose the shown range, or none after a zoom.
function pressRange(hours) {
  state.hours = hours;
  for (const b of document.querySelectorAll(".ranges [data-hours]")) {
    b.setAttribute("aria-pressed", String(b.dataset.hours === hours));
  }
}

// Draws the threshold lines and the shaded spans under the series of a chart.
function overlays(spans, thresholds) {
  return (u) => {
    const { ctx } = u;
    const { left, top, width, height } = u.bbox;
    ctx.save();
    for (const s of spans()) {
      const x0 = u.valToPos(s.from, "x", true);
      const x1 = u.valToPos(s.to, "x", true);
      const a = Math.max(left, Math.min(x0, x1));
      const b = Math.min(left + width, Math.max(x0, x1));
      if (b < left || a > left + width) continue;
      ctx.fillStyle = s.fill;
      ctx.fillRect(a, top, Math.max(b - a, 2 * devicePixelRatio), height);
    }
    const t = thresholds();
    const lines = [[t.cpu_start_percent, [6, 4], "CPU"], [t.ram_start_percent, [2, 3], "RAM"]];
    ctx.strokeStyle = color.peak;
    ctx.fillStyle = color.peak;
    ctx.lineWidth = devicePixelRatio;
    ctx.font = `${11 * devicePixelRatio}px system-ui, sans-serif`;
    // The higher line is labelled above itself and the lower one below, so close lines keep
    // their labels apart.
    lines.sort((a, b) => b[0] - a[0]);
    lines.forEach(([value, dash, name], i) => {
      const y = Math.round(u.valToPos(value, "y", true)) + 0.5;
      ctx.setLineDash(dash.map((d) => d * devicePixelRatio));
      ctx.beginPath();
      ctx.moveTo(left, y);
      ctx.lineTo(left + width, y);
      ctx.stroke();
      const labelY = i === 0 ? y - 4 * devicePixelRatio : y + 13 * devicePixelRatio;
      ctx.textAlign = "left";
      ctx.fillText(`${name} ${pct(value)}`, left + 4 * devicePixelRatio, labelY);
    });
    ctx.restore();
  };
}

function axisValues(u, splits) {
  const span = u.scales.x.max - u.scales.x.min;
  const format = span > 2 * 86400 ? axisDay : span > 600 ? axisTime : axisSeconds;
  return splits.map((s) => format.format(new Date(s * 1000)));
}

// A value with a gap on both sides draws no line, so it gets a dot.
function isolatedPoints(u, seriesIdx) {
  const ys = u.data[seriesIdx];
  const idx = [];
  for (let i = 0; i < ys.length; i++) {
    if (ys[i] != null && ys[i - 1] == null && ys[i + 1] == null) idx.push(i);
  }
  return idx;
}

function chartOptions(width, hooks, drag) {
  const axis = { stroke: color.muted, grid: { stroke: color.border, width: 1 }, ticks: { stroke: color.border, width: 1 } };
  const points = (stroke) => ({ show: true, size: 5, fill: stroke, stroke, filter: isolatedPoints });
  return {
    width,
    height: width < 480 ? 200 : 280,
    legend: { show: false },
    cursor: { drag: { x: drag, y: false, setScale: false }, points: { show: false } },
    scales: { x: { time: true }, y: { range: [0, 100] } },
    series: [
      {},
      { label: "CPU", stroke: color.cpu, width: 1.5, spanGaps: false, points: points(color.cpu) },
      { label: "RAM", stroke: color.ram, width: 1.5, spanGaps: false, points: points(color.ram) },
    ],
    axes: [
      { ...axis, values: axisValues, space: 70 },
      { ...axis, values: (u, splits) => splits.map((v) => `${v}%`), size: 44 },
    ],
    hooks,
  };
}

let timelineChart = null;

function renderTimeline(t) {
  const data = [t.ts, t.cpu, t.ram];
  const container = $("timeline");
  if (!timelineChart) {
    let downX = null;
    const hooks = {
      drawClear: [overlays(
        () => (state.timeline?.episodes ?? []).map((e) => ({
          from: Date.parse(e.start) / 1000, to: Date.parse(e.end) / 1000, fill: color.peakFill,
        })),
        () => state.timeline.thresholds,
      )],
      setSelect: [(u) => {
        if (u.select.width < 2) return;
        const from = new Date(u.posToVal(u.select.left, "x") * 1000);
        const to = new Date(u.posToVal(u.select.left + u.select.width, "x") * 1000);
        u.setSelect({ left: 0, width: 0, top: 0, height: 0 }, false);
        const back = [state.from, state.to, state.hours];
        load(from, to, () => { state.zoomed.push(back); pressRange(null); });
      }],
      ready: [(u) => {
        u.over.addEventListener("mousedown", (e) => { downX = e.clientX; });
        u.over.addEventListener("click", (e) => {
          if (downX != null && Math.abs(e.clientX - downX) > 3) return;
          const x = u.posToVal(u.cursor.left, "x");
          const slack = (u.scales.x.max - u.scales.x.min) / u.bbox.width * devicePixelRatio * 3;
          const hit = state.timeline.episodes.find((ep) =>
            Date.parse(ep.start) / 1000 - slack <= x && x <= Date.parse(ep.end) / 1000 + slack);
          if (hit) showEpisode(hit.id);
        });
      }],
    };
    timelineChart = new uPlot(chartOptions(container.clientWidth, hooks, !report), data, container);
  } else {
    timelineChart.setData(data);
  }
  renderTimelineSummary(t);
}

function renderTimelineSummary(t) {
  const cpu = t.cpu.filter((v) => v != null);
  const ram = t.ram.filter((v) => v != null);
  const step = duration(t.step_seconds);
  if (t.samples === 0) {
    $("timeline-summary").textContent = "Nenhuma amostra neste intervalo. O coletor (perfwatch run) grava os dados enquanto roda.";
  } else {
    const gaps = t.cpu.length - cpu.length;
    const episodes = t.episodes.length;
    $("timeline-summary").textContent =
      `Médias a cada ${step}. CPU: média ${pct(t.cpu_avg_percent)}, maior média de intervalo ${pct(Math.max(...cpu))}. ` +
      `RAM: média ${pct(t.ram_avg_percent)}, maior média de intervalo ${pct(Math.max(...ram))}. ` +
      (gaps ? `${integer.format(gaps)} de ${integer.format(t.cpu.length)} intervalos sem amostra. ` : "") +
      (episodes === 1 ? "1 episódio de pico." : `${integer.format(episodes)} episódios de pico.`);
  }
  const rows = t.ts.map((ts, i) => el("tr", {},
    el("td", {}, when(new Date(ts * 1000))), cell(pct(t.cpu[i]), "CPU", true), cell(pct(t.ram[i]), "RAM", true)));
  $("timeline-table").tBodies[0].replaceChildren(...rows);
}

function renderEpisodeList(episodes) {
  const list = $("episode-list");
  if (episodes.length === 0) {
    list.replaceChildren(el("li", { class: "empty" }, "Nenhum episódio de pico neste intervalo."));
    return;
  }
  list.replaceChildren(...[...episodes].reverse().map((e) => {
    const button = el("button", { type: "button", "data-episode": String(e.id), "aria-controls": "episode-detail" },
      el("span", { class: "when" }, when(new Date(e.start))),
      el("span", { class: "what" }, `${triggerText[e.trigger] ?? e.trigger}, ${duration(e.seconds)}${e.open ? ", ainda aberto" : ""}`));
    button.addEventListener("click", () => showEpisode(e.id));
    return el("li", {}, button);
  }));
}

let episodeChart = null;

async function showEpisode(id) {
  let detail;
  try {
    detail = await fresh("episode", `api/episodes/${encodeURIComponent(id)}`);
  } catch (err) {
    $("status").textContent = `Não foi possível abrir o episódio: ${err.message}`;
    return;
  }
  if (!detail) return;
  const e = detail.episode;
  state.episode = e;
  const section = $("episode-detail");
  section.hidden = false;
  $("episode-title").textContent = `Episódio de ${when(new Date(e.start))}`;
  const stats = [
    ["Duração", duration(e.seconds) + (e.open ? " (ainda aberto)" : "")],
    ["Gatilho", triggerText[e.trigger] ?? e.trigger],
    ["CPU máxima", pct(e.cpu_max_percent)],
    ["RAM máxima", pct(e.ram_max_percent)],
    ["CPU não atribuída", `${pct(e.unattributed_avg_percent)} em média`],
    ["Pré-roll", duration(e.preroll_seconds)],
  ];
  $("episode-stats").replaceChildren(...stats.map(([k, v]) => el("div", {}, el("dt", {}, k), el("dd", {}, v))));

  const s = detail.seconds;
  const data = [s.ts, s.cpu, s.ram];
  const container = $("episode-chart");
  if (episodeChart) episodeChart.destroy();
  const start = Date.parse(e.start) / 1000;
  const end = Date.parse(e.end) / 1000;
  episodeChart = new uPlot(chartOptions(container.clientWidth, {
    drawClear: [overlays(
      () => [
        { from: start - state.episode.preroll_seconds, to: start, fill: color.preRollFill },
        { from: start, to: end, fill: color.peakFill },
      ],
      () => state.timeline.thresholds,
    )],
  }, false), data, container);

  const after = s.ts.filter((t) => t > end).length;
  $("episode-summary").textContent =
    `Área cinza: os ${duration(e.preroll_seconds)} de pré-roll antes do início; área vermelha: o pico. ` +
    `CPU máxima ${pct(e.cpu_max_percent)}, RAM máxima ${pct(e.ram_max_percent)}, ` +
    `CPU não atribuída ${pct(e.unattributed_avg_percent)} em média. ` +
    (after ? `Depois do fim, ${integer.format(after)} amostras até esfriar.` : "");

  $("episode-programs").tBodies[0].replaceChildren(...e.programs.map((p) => el("tr", {},
    cell(programLabel(p), "Programa"),
    cell(pct(p.cpu_avg_percent), "CPU média", true),
    cell(pct(p.cpu_max_percent), "CPU máx.", true),
    cell(bytes(p.private_max_bytes), "RAM máx.", true))));
  $("episode-table").tBodies[0].replaceChildren(...s.ts.map((t, i) => el("tr", {},
    el("td", {}, timeOnly.format(new Date(t * 1000))),
    cell(pct(s.cpu[i]), "CPU", true), cell(pct(s.ram[i]), "RAM", true), cell(pct(s.unattributed[i]), "Não atribuída", true))));
  $("episode-title").focus();
  section.scrollIntoView({ block: "start" });
}

function programLabel(p) {
  return p.service_group ? `${p.name} (${p.service_group})` : p.name;
}

function renderRanking(r) {
  const peaks = r.peaks_only ? " dentro de episódios de pico" : "";
  // Programs are stored per minute, so the ranking can start before the range shown above.
  const widened = Date.parse(r.from) < state.from.getTime()
    ? ` O ranking começa em ${when(new Date(r.from))}: os programas são guardados por minuto.` : "";
  $("ranking-summary").textContent = (r.sampled_seconds === 0
    ? `Nenhuma amostra${peaks} neste intervalo.`
    : `${duration(r.sampled_seconds)} amostrados${peaks}; CPU da máquina ${pct(r.cpu_avg_percent)} em média.`) + widened;
  const body = $("ranking").tBodies[0];
  if (r.sampled_seconds === 0) {
    body.replaceChildren();
    return;
  }
  // Unattributed CPU and the programs not listed are rows of their own, where their CPU-seconds put them.
  const shares = [
    { name: "(CPU não atribuída)", ...r.unattributed },
    { name: "(outros programas)", ...r.other_programs },
  ].filter((s, i) => i === 0 || s.cpu_seconds > 0).sort((a, b) => b.cpu_seconds - a.cpu_seconds);
  const rows = [];
  const placeShares = (above) => {
    while (shares.length && shares[0].cpu_seconds > above) {
      const s = shares.shift();
      rows.push(el("tr", { class: "share" },
        cell(s.name, "Programa"), cell(oneDecimal.format(s.cpu_seconds), "CPU-s", true),
        cell(pct(s.cpu_avg_percent), "CPU média", true), cell("", "CPU máx.", true), cell("", "RAM máx.", true), cell("", "Instâncias", true)));
    }
  };
  for (const p of r.programs) {
    placeShares(p.cpu_seconds);
    const name = cell(programLabel(p), "Programa");
    if (p.path) name.append(el("span", { class: "path" }, p.path));
    rows.push(el("tr", {},
      name, cell(oneDecimal.format(p.cpu_seconds), "CPU-s", true), cell(pct(p.cpu_avg_percent), "CPU média", true),
      cell(pct(p.cpu_max_percent), "CPU máx.", true), cell(bytes(p.private_max_bytes), "RAM máx.", true),
      cell(integer.format(p.instances_max), "Instâncias", true)));
  }
  placeShares(-1);
  body.replaceChildren(...rows);
}

for (const button of document.querySelectorAll(".ranges [data-hours]")) {
  button.addEventListener("click", () => {
    const hours = button.dataset.hours;
    load(new Date(Date.now() - Number(hours) * 3600 * 1000), null, () => {
      state.zoomed = [];
      pressRange(hours);
    });
  });
}
$("zoom-out").addEventListener("click", () => {
  const back = state.zoomed.at(-1);
  if (back) load(back[0], back[1], () => { state.zoomed.pop(); pressRange(back[2]); });
});
$("peaks-only").addEventListener("change", () => loadRanking().catch((err) => {
  $("status").textContent = `Não foi possível carregar o ranking: ${err.message}`;
}));
$("episode-close").addEventListener("click", () => {
  $("episode-detail").hidden = true;
  // Back to the episode's entry in the list, or the list itself when the range no longer holds it.
  const opener = document.querySelector(`#episode-list [data-episode="${state.episode.id}"]`);
  (opener ?? $("episodes-title")).focus();
});

new ResizeObserver(() => {
  for (const [chart, id] of [[timelineChart, "timeline"], [episodeChart, "episode-chart"]]) {
    if (!chart) continue;
    const width = $(id).clientWidth;
    if (width > 0 && width !== chart.width) chart.setSize({ width, height: width < 480 ? 200 : 280 });
  }
}).observe(document.querySelector("main"));

if (report) {
  // The report covers the range it was exported for: no range buttons, no zoom.
  document.body.classList.add("report");
  load(null, null);
} else {
  pressRange("24");
  load(null, null);
}
