// pumasi dashboard: reads the controller's JSON every few seconds, keeps a
// world of halls, lines, machines and crates, and draws it. Read-only except
// for the priority API (expedite, mode, rules) where the controller has one.
import { View, TW, TH, TZ } from "./iso.js";
import { World, HALL_D } from "./world.js";
import { Scene } from "./draw.js";
import { store, renderChrome, renderPanel, renderList, renderTimeline, renderStale, renderMode, jobKey, atParam, subjectOf } from "./hud.js";
import { clamp, ease, rgb, ago, cpus, gb, plainReason, setLive } from "./util.js";

const $ = (id) => document.getElementById(id);
const params = new URLSearchParams(location.search);
const canvas = $("scene");
const view = new View(canvas);
const world = new World();
const scene = new Scene(view);
store.base = location.origin + location.pathname.replace(/[^/]*$/, "");

// ---------------------------------------------------------------- settings
const reduce = matchMedia("(prefers-reduced-motion: reduce)");
const ui = {
  motion: params.get("motion") ? params.get("motion") !== "off" : !reduce.matches,
  view: params.get("view") === "list" ? "list" : "map",
  prioView: params.get("view") === "priority",
  sel: null,
};
const themeNow = () => document.documentElement.dataset.theme || "dark";
if (params.get("theme")) document.documentElement.dataset.theme = params.get("theme");
scene.setTheme(themeNow() === "light" ? "light" : "dark");

// demo: ?at=<seconds> pins the demo's clock, and it runs on from there
const at0 = params.get("at") !== null ? +params.get("at") : null;
const t0 = performance.now();
window.__pumasiAt = () => (at0 === null ? null : at0 + (performance.now() - t0) / 1000);
const stress = clamp(+params.get("stress") || 0, 0, 20);

// ---------------------------------------------------------------- data
let lastOK = 0, dirty = true;
async function getJSON(p) {
  const r = await fetch(p + atParam(p.includes("?") ? "&" : "?"), { cache: "no-store" });
  if (!r.ok) { const e = new Error(p + " " + r.status); e.status = r.status; throw e; }
  return r.json();
}
let prioCheck = 0;
async function refresh() {
  try {
    const [state, jobs] = await Promise.all([getJSON("api/state"), getJSON("api/jobs")]);
    // only what the server says it answers: a missing route would be a console error on every read
    const api = state.api || [];
    const findings = api.includes("explain") ? getJSON("api/explain?format=json").then((x) => x.findings || []).catch(() => []) : Promise.resolve([]);
    let prio = null;
    // only a controller that lists the priority API is asked: a probe of one that
    // has none would be a 404 in the console (writes fall back to the CLI line)
    const hasPrio = api.includes("priority");
    if (hasPrio && (!store.prioMissing || Date.now() - prioCheck > 60000)) {
      prioCheck = Date.now();
      prio = getJSON("api/priority").then((x) => { store.prioMissing = false; return x; }).catch((e) => { if (e.status === 404 || e.status === 405) store.prioMissing = true; return null; });
    } else if (!hasPrio) store.prioMissing = true;
    store.state = stress ? multiply(state, stress) : state;
    store.jobs = jobs || [];
    store.findings = await findings;
    if (prio) store.prio = (await prio) || (store.prioMissing ? null : store.prio);
    if (api.includes("explain") && (ui.view === "list" || (ui.sel && ui.sel.kind === "explain"))) store.explainText = await fetch("api/explain" + atParam("?")).then((r) => (r.ok ? r.text() : "")).catch(() => "");
    lastOK = Date.now();
    apply();
  } catch (e) {
    /* the controller is gone or restarting: the header says so, the plant keeps the last read */
  }
  renderStale(lastOK);
}
async function refreshSeries() {
  try { store.series = await getJSON("api/series"); renderTimeline(select); if (ui.view === "list") renderList(actions); } catch (e) { /* keep the last */ }
}

function apply() {
  const s = store.state;
  setLive(s.hosts);
  const express = expressOf();
  world.update(s, store.jobs, now(), { instant: !ui.motion, express, mode: store.prio && store.prio.mode });
  if (!view.fitted) fit(true);
  renderChrome(select);
  renderMode();
  if (ui.sel) renderPanel(ui.sel, actions, { poll: true });
  if (ui.view === "list") renderList(actions);
  renderTimeline(select);
  dirty = true;
  if (deep) { const d = deep; deep = null; select(d, true); }
}

// expressOf: the expedited job that has room kept for it, and where.
function expressOf() {
  const p = store.prio;
  if (!p || !p.line) return null;
  const e = p.line.find((x) => x.level === "expedite");
  if (!e) return null;
  const host = store.state.hosts.find((h) => e.waitingFor && e.waitingFor.includes(h.name));
  return host ? { host: host.name, name: e.name, class: e.class } : { name: e.name, class: e.class };
}

// stress: the same plant with n times the runners and waiting jobs (?stress=n), to measure the frame
function multiply(s, n) {
  const out = JSON.parse(JSON.stringify(s));
  for (let k = 1; k <= n; k++) {
    for (const r of s.runners) out.runners.push({ ...r, name: r.name + "-x" + k, ageSeconds: r.ageSeconds - k });
    for (const j of s.waitingJobs || []) out.waitingJobs.push({ ...j, id: j.id ? j.id + "-x" + k : undefined, runId: j.runId * 10 + k, githubJobId: j.githubJobId ? j.githubJobId * 10 + k : undefined });
  }
  for (const w of out.waiting) w.jobs = out.waitingJobs.filter((j) => j.class === w.class).length;
  return out;
}

// ---------------------------------------------------------------- selection
let deep = null;
for (const k of ["host", "class", "runner", "job"]) if (params.get(k)) deep = { kind: k, id: params.get(k) };
if (params.get("panel") === "explain") deep = { kind: "explain" };
if (ui.prioView) deep = deep || { kind: "priority" };

const actions = {
  select: (s) => select(s),
  refresh: () => setTimeout(refresh, 150),
  selectJobBy: (r) => {
    // r: a row of /api/priority's line or expedited (r.job is pumasi's id), or of the state;
    // the surest name first: jobs of one name wait in one class (a matrix, several runs)
    const ws = store.state.waitingJobs || [];
    const j = r._j || (r.job && ws.find((x) => x.id === r.job)) || (r.githubJobId && ws.find((x) => x.githubJobId === r.githubJobId)) ||
      (r.runId && ws.find((x) => x.job === r.name && x.class === r.class && x.runId === r.runId)) || ws.find((x) => x.job === r.name && x.class === r.class);
    if (j) select({ kind: "job", id: jobKey(j) });
  },
};
function select(sel, fromLink) {
  // "Run next" on a waiting row: open the job and press its expedite button,
  // so the answer (or the command to type when the controller has no API) shows there
  const runNext = sel && sel.expedite;
  if (runNext) sel = { kind: sel.kind, id: sel.id };
  if (runNext) setTimeout(() => { const b = document.querySelector('#panel [data-action="expedite"]'); if (b) b.click(); }, 0);
  if (sel && sel.kind === "job") {
    // a deep link may name a job by its GitHub job id, run id or name
    const j = (store.state.waitingJobs || []).find((x) => jobKey(x) === sel.id || x.id === sel.id || String(x.githubJobId) === sel.id || String(x.runId) === sel.id || x.job === sel.id);
    if (j) sel = { kind: "job", id: jobKey(j) };
    else {
      const r = store.state.runners.find((x) => x.job === sel.id);
      const d = store.jobs.find((x) => x.job === sel.id || String(x.runId) === sel.id);
      if (r) sel = { kind: "runner", id: r.name };
      else if (d) sel = { kind: "done", id: "s:" + d.runner };
    }
  }
  if (sel && (sel.kind === "priority" || sel.kind === "mode") && !store.prio && !store.prioMissing) setTimeout(refresh, 0);
  ui.sel = sel;
  select.current = sel;
  store.keepUntil = 0;
  renderPanel(sel, actions);
  renderChrome(select);
  const q = new URLSearchParams(location.search);
  for (const k of ["host", "class", "runner", "job", "panel"]) q.delete(k);
  if (sel && ["host", "class", "runner"].includes(sel.kind)) q.set(sel.kind, sel.id);
  if (sel && sel.kind === "job") q.set("job", sel.id.replace(/^g:/, ""));
  if (sel && sel.kind === "explain") q.set("panel", "explain");
  history.replaceState(null, "", "?" + q.toString().replace(/%2C/g, ","));
  if (sel) focus(sel, fromLink);
  dirty = true;
}
select.current = null;

function where(sel) {
  if (!sel) return null;
  if (sel.kind === "host") { const h = world.halls.find((x) => x.name === sel.id); return h && [h.x0 + h.W / 2, h.y0 + h.D / 2, 0.9]; }
  if (sel.kind === "class") { const l = world.lines.find((x) => x.name === sel.id); return l && [l.x0 + 0.5, l.y0 + 3, 1.3]; }
  if (sel.kind === "runner") { const m = world.machines.get(sel.id); return m && [m.x, m.y, 1.6]; }
  if (sel.kind === "job") { const c = [...world.crates.values()].find((c) => c.j && c.mode === "queue" && jobKey(c.j) === sel.id); return c && [c.x, c.y, 1.6]; }
  if (sel.kind === "done") { const c = world.crates.get(sel.id); return c && [c.x, c.y, 1.4]; }
  return null;
}
let camAnim = null;
function focus(sel, instant) {
  const w = where(sel);
  if (!w) return;
  const [ix, iy] = view.iso(w[0], w[1], 0);
  // leave room for the panel on the right
  const panelW = view.w > 700 ? 200 : 0;
  const z = Math.max(view.cam.z, w[2]);
  const target = { x: ix + panelW / z, y: iy + (view.w > 700 ? 0 : view.h * 0.22 / z), z };
  if (instant || !ui.motion) { Object.assign(view.cam, target); dirty = true; return; }
  camAnim = { from: { ...view.cam }, to: target, t0: performance.now() };
}

// ---------------------------------------------------------------- camera
function fit(instant) {
  if (!world.bounds || !view.w) return;
  const b = { x0: world.trunk.x0, x1: world.shipArea.x1, y0: -1 - HALL_D - 1.5, y1: world.dock.y1 };
  const pts = [[b.x0, b.y0], [b.x1, b.y0], [b.x1, b.y1], [b.x0, b.y1]].map(([x, y]) => view.iso(x, y, 0));
  const top = view.iso(b.x0, b.y0, 4);
  let x0 = Math.min(...pts.map((p) => p[0])), x1 = Math.max(...pts.map((p) => p[0]));
  let y0 = Math.min(top[1], ...pts.map((p) => p[1])), y1 = Math.max(...pts.map((p) => p[1]));
  const wide = view.w > 900;
  // leave the left column alone (the map does not hide under what describes it),
  // and room above the halls for their gauges
  const idx = $("index");
  const padL = wide && idx.offsetParent ? idx.getBoundingClientRect().right - $("stage").getBoundingClientRect().left + 8 : 6;
  const padR = wide ? 16 : 6, padT = wide ? 150 : 96, padB = wide ? 40 : 50;
  let z = Math.min((view.w - padL - padR) / (x1 - x0), (view.h - padT - padB) / (y1 - y0));
  // a tall phone: the plant is a diagonal band, so fitting its width leaves most of
  // the screen empty. Come closer (the ends are a drag away) so the halls read.
  if (view.h > view.w * 1.3) z *= 2.1;
  z = clamp(z, 0.18, 1.6);
  const target = { x: (x0 + x1) / 2 - (padL - padR) / 2 / z, y: (y0 + y1) / 2 - (padT - padB) / 2 / z, z };
  if (instant || !ui.motion) Object.assign(view.cam, target);
  else camAnim = { from: { ...view.cam }, to: target, t0: performance.now() };
  view.fitted = true;
  dirty = true;
}
function zoomAt(f, sx, sy) {
  const c = view.cam;
  const nz = clamp(c.z * f, 0.15, 3.5);
  const wx = (sx - view.w / 2) / c.z + c.x, wy = (sy - view.h / 2) / c.z + c.y;
  c.z = nz;
  c.x = wx - (sx - view.w / 2) / nz;
  c.y = wy - (sy - view.h / 2) / nz;
  camAnim = null;
  dirty = true;
}

const pointers = new Map();
let drag = null, pinch = null;
canvas.addEventListener("pointerdown", (e) => {
  canvas.setPointerCapture(e.pointerId);
  pointers.set(e.pointerId, [e.offsetX, e.offsetY]);
  if (pointers.size === 1) drag = { x: e.offsetX, y: e.offsetY, moved: 0 };
  if (pointers.size === 2) { const [a, b] = [...pointers.values()]; pinch = { d: Math.hypot(a[0] - b[0], a[1] - b[1]), z: view.cam.z }; drag = null; }
});
canvas.addEventListener("pointermove", (e) => {
  if (pointers.has(e.pointerId)) pointers.set(e.pointerId, [e.offsetX, e.offsetY]);
  if (pinch && pointers.size === 2) {
    const [a, b] = [...pointers.values()];
    const d = Math.hypot(a[0] - b[0], a[1] - b[1]);
    zoomAt((pinch.z * d) / pinch.d / view.cam.z, (a[0] + b[0]) / 2, (a[1] + b[1]) / 2);
    return;
  }
  if (drag) {
    const dx = e.offsetX - drag.x, dy = e.offsetY - drag.y;
    drag.moved += Math.abs(dx) + Math.abs(dy);
    if (drag.moved > 4) {
      canvas.classList.add("drag");
      view.cam.x -= dx / view.cam.z; view.cam.y -= dy / view.cam.z;
      camAnim = null;
      dirty = true;
    }
    drag.x = e.offsetX; drag.y = e.offsetY;
    return;
  }
  hover(e.offsetX, e.offsetY);
});
const up = (e) => {
  pointers.delete(e.pointerId);
  if (pointers.size < 2) pinch = null;
  canvas.classList.remove("drag");
  if (drag && drag.moved <= 4 && e.type === "pointerup") {
    const h = scene.pick(e.offsetX, e.offsetY);
    if (h) select({ kind: h.kind, id: h.id });
    else if (ui.sel) { ui.sel = null; select.current = null; renderPanel(null); dirty = true; }
  }
  drag = null;
};
canvas.addEventListener("pointerup", up);
canvas.addEventListener("pointercancel", up);
canvas.addEventListener("wheel", (e) => {
  e.preventDefault();
  zoomAt(Math.exp(-e.deltaY * (e.ctrlKey ? 0.01 : 0.0015)), e.offsetX, e.offsetY);
}, { passive: false });
canvas.addEventListener("dblclick", (e) => zoomAt(1.6, e.offsetX, e.offsetY));

const tip = $("tip");
function hover(x, y) {
  const h = scene.pick(x, y);
  canvas.classList.toggle("over", !!h);
  const text = h && tipText(h);
  if (!text) { tip.hidden = true; return; }
  tip.hidden = false;
  tip.replaceChildren(Object.assign(document.createElement("b"), { textContent: text[0] }), Object.assign(document.createElement("span"), { textContent: text[1] }));
  tip.style.left = Math.min(x + 14, view.w - 300) + "px";
  tip.style.top = y + 16 + "px";
}
function tipText(h) {
  const s = store.state;
  if (h.kind === "runner") { const r = s.runners.find((x) => x.name === h.id); return r && [r.job || (r.state === "starting" ? "a runner starting" : r.name), r.state === "starting" ? `class ${r.class} on ${r.host}` : `running ${ago(r.ageSeconds)} on ${r.host} · using ${cpus(r.cpuNow || 0)} and ${gb(r.memBytes || 0)} of ${gb(r.heldMem || r.estMem)} reserved`]; }
  if (h.kind === "job") { const c = world.crates.get(h.id); return c && c.j && [c.j.job || "a job", `waiting ${ago(c.j.waitedSeconds)}${c.j.position ? " · " + c.j.position + ". in line" : ""}${c.j.expedited ? " · expedited" : c.j.level ? " · " + c.j.level : ""} — ${plainReason(c.j.reason)}`]; }
  if (h.kind === "done") { const c = world.crates.get(h.id); return c && c.j && [c.j.job, `${{ ok: "finished ok", fail: "failed", oom: "out of memory", cancel: "cancelled" }[c.result] || c.result} · on ${c.j.host}`]; }
  if (h.kind === "host") return [h.id, "a host: click for how full it is and what runs there"];
  if (h.kind === "class") return [h.id, "a class of runner: click for its waiting jobs and why they wait"];
  return null;
}

addEventListener("keydown", (e) => {
  if (e.target.closest && e.target.closest("input,select,textarea")) return;
  const k = e.key, step = 60 / view.cam.z;
  if (k === "Escape") { ui.sel = null; select.current = null; renderPanel(null); dirty = true; }
  else if (k === "ArrowLeft") view.cam.x -= step;
  else if (k === "ArrowRight") view.cam.x += step;
  else if (k === "ArrowUp") view.cam.y -= step;
  else if (k === "ArrowDown") view.cam.y += step;
  else if (k === "+" || k === "=") zoomAt(1.25, view.w / 2, view.h / 2);
  else if (k === "-") zoomAt(0.8, view.w / 2, view.h / 2);
  else if (k === "0" || k === "f") fit();
  else if (k === "l") setView(ui.view === "list" ? "map" : "list");
  else if (k === "p") togglePrio();
  else return;
  dirty = true;
});

// ---------------------------------------------------------------- toolbar
function setView(v) {
  ui.view = v;
  $("list").hidden = v !== "list";
  $("stage").hidden = v === "list";
  document.body.classList.toggle("listing", v === "list");
  $("view-map").setAttribute("aria-pressed", String(v === "map"));
  $("view-list").setAttribute("aria-pressed", String(v === "list"));
  const q = new URLSearchParams(location.search);
  if (v === "list") q.set("view", "list"); else q.delete("view");
  history.replaceState(null, "", "?" + q.toString());
  if (v === "list") refresh();
  else { view.resize(); dirty = true; }
}
function togglePrio() {
  ui.prioView = !ui.prioView;
  $("prio").setAttribute("aria-pressed", String(ui.prioView));
  if (ui.prioView) select({ kind: "priority" });
  else if (ui.sel && ui.sel.kind === "priority") { ui.sel = null; select.current = null; renderPanel(null); }
  dirty = true;
}
$("view-map").onclick = () => setView("map");
$("view-list").onclick = () => setView("list");
$("prio").onclick = togglePrio;
$("prio").setAttribute("aria-pressed", String(ui.prioView));
$("mode").onclick = () => select({ kind: "mode" });
$("explain").onclick = async () => {
  select({ kind: "explain" });
  const has = store.state && (store.state.api || []).includes("explain");
  store.explainText = has ? await fetch("api/explain" + atParam("?")).then((r) => (r.ok ? r.text() : "")).catch(() => "") : "This controller has no api/explain (an older pumasi): `pumasi status` says the same in a terminal.";
  renderPanel(ui.sel, actions);
};
$("motion").onclick = () => {
  ui.motion = !ui.motion;
  $("motion").setAttribute("aria-pressed", String(ui.motion));
  dirty = true;
  if (ui.motion) loop();
};
$("motion").setAttribute("aria-pressed", String(ui.motion));
$("theme").onclick = () => {
  const t = themeNow() === "dark" ? "light" : "dark";
  document.documentElement.dataset.theme = t;
  try { localStorage.setItem("pumasi.theme", t); } catch (e) { /* this page only */ }
  scene.setTheme(t);
  drawMinimap.bg = null;
  dirty = true;
};
$("panel-close").onclick = () => { if (ui.sel && ui.sel.kind === "priority") togglePrio(); ui.sel = null; select.current = null; renderPanel(null); dirty = true; };
$("index-toggle").onclick = () => {
  const n = $("index");
  const c = n.dataset.collapsed !== "true";
  n.dataset.collapsed = String(c);
  $("index-toggle").setAttribute("aria-expanded", String(!c));
};
if (innerWidth < 640) { $("index").dataset.collapsed = "true"; $("index-toggle").setAttribute("aria-expanded", "false"); }

// the legend: shown until someone says they have it, then a "?" away
const legendKey = "pumasi.legend";
const legendSeen = () => { try { return localStorage.getItem(legendKey) === "seen"; } catch (e) { return false; } };
const legend = (on) => {
  $("legend").hidden = !on;
  $("legend-open").setAttribute("aria-pressed", String(on));
};
legend(params.get("legend") === "on" || (params.get("legend") !== "off" && !legendSeen()));
$("legend-close").onclick = () => { legend(false); try { localStorage.setItem(legendKey, "seen"); } catch (e) { /* this page only */ } };
$("legend-open").onclick = () => legend($("legend").hidden);
for (const b of document.querySelectorAll("#zoom button")) {
  b.onclick = () => { const z = b.dataset.zoom; if (z === "fit") fit(); else zoomAt(z === "in" ? 1.3 : 0.77, view.w / 2, view.h / 2); };
}

// ---------------------------------------------------------------- minimap
const mini = $("minimap").querySelector("canvas");
const mctx = mini.getContext("2d");
function miniXform() {
  const b = world.bounds;
  const pts = [[b.x0, b.y0], [b.x1, b.y0], [b.x1, b.y1], [b.x0, b.y1]].map(([x, y]) => view.iso(x, y));
  const x0 = Math.min(...pts.map((p) => p[0])), x1 = Math.max(...pts.map((p) => p[0])), y0 = Math.min(...pts.map((p) => p[1])), y1 = Math.max(...pts.map((p) => p[1]));
  const k = Math.min((mini.width - 8) / (x1 - x0), (mini.height - 8) / (y1 - y0));
  return { k, ox: (mini.width - (x1 - x0) * k) / 2 - x0 * k, oy: (mini.height - (y1 - y0) * k) / 2 - y0 * k };
}
function drawMinimap() {
  if (!world.bounds || $("minimap").offsetParent === null) return;
  const m = miniXform(), th = scene.th;
  const P = (x, y) => { const [ix, iy] = view.iso(x, y); return [ix * m.k + m.ox, iy * m.k + m.oy]; };
  const quad = (x0, y0, x1, y1, fill) => {
    const a = P(x0, y0), b = P(x1, y0), c = P(x1, y1), d = P(x0, y1);
    mctx.beginPath(); mctx.moveTo(...a); mctx.lineTo(...b); mctx.lineTo(...c); mctx.lineTo(...d); mctx.closePath();
    mctx.fillStyle = fill; mctx.fill();
  };
  mctx.clearRect(0, 0, mini.width, mini.height);
  const b = world.bounds;
  quad(b.x0, b.y0, b.x1, b.y1, rgb(th.yard, 0.9));
  quad(world.trunk.x0, 0, world.trunk.x1, 2, rgb(th.dark));
  for (const h of world.halls) {
    const col = h.status === "down" ? th.bad : h.status === "closed" ? th.warn : h.status === "ok" ? th.floor : th.off;
    quad(h.x0, h.y0, h.x1, h.y1, rgb(col, h.status === "ok" ? 1 : 0.8));
  }
  for (const l of world.lines) quad(l.x0, l.y0, l.x0 + 1, l.y1, rgb(l.cls.color, 0.9));
  const s = world.shipArea;
  quad(s.x0, s.y0, s.x1, s.y1, rgb(th.floor, 0.7));
  for (const mm of world.machines.values()) { const [x, y] = P(mm.x, mm.y); mctx.fillStyle = rgb(mm.cls.color); mctx.fillRect(x - 1.5, y - 1.5, 3, 3); }
  for (const c of world.crates.values()) { if (c.mode !== "queue") continue; const [x, y] = P(c.x, c.y); mctx.fillStyle = rgb(th.warn); mctx.fillRect(x - 1, y - 1, 2, 2); }
  // the view's corners on the ground
  const corners = [[0, 0], [view.w, 0], [view.w, view.h], [0, view.h]].map(([x, y]) => view.ground(x, y)).map(([x, y]) => P(x, y));
  mctx.beginPath(); corners.forEach((p, i) => (i ? mctx.lineTo(...p) : mctx.moveTo(...p))); mctx.closePath();
  mctx.strokeStyle = rgb(th.paint); mctx.lineWidth = 1.5; mctx.stroke();
}
mini.addEventListener("pointerdown", (e) => {
  const go = (ev) => {
    const r = mini.getBoundingClientRect();
    const m = miniXform();
    const ix = ((ev.clientX - r.left) * (mini.width / r.width) - m.ox) / m.k, iy = ((ev.clientY - r.top) * (mini.height / r.height) - m.oy) / m.k;
    view.cam.x = ix; view.cam.y = iy; camAnim = null; dirty = true;
  };
  go(e);
  mini.setPointerCapture(e.pointerId);
  mini.onpointermove = go;
  mini.onpointerup = () => { mini.onpointermove = null; };
});

// ---------------------------------------------------------------- frame loop
const now = () => performance.now() / 1000;
let looping = false, frames = 0, lastMini = 0;
const perf = (window.__pumasiPerf = { draw: [], n: 0 });
function render() {
  const t = now();
  if (camAnim) {
    const k = clamp((performance.now() - camAnim.t0) / 650, 0, 1), e = ease(k);
    for (const key of ["x", "y", "z"]) view.cam[key] = camAnim.from[key] + (camAnim.to[key] - camAnim.from[key]) * e;
    if (k >= 1) camAnim = null;
  }
  world.step(t);
  const a = performance.now();
  scene.frame(world, t, { motion: ui.motion, selected: ui.sel, prioView: ui.prioView, mode: store.prio && store.prio.mode });
  const d = performance.now() - a;
  perf.draw.push(d); if (perf.draw.length > 600) perf.draw.shift(); perf.n++;
  if (performance.now() - lastMini > 250) { drawMinimap(); lastMini = performance.now(); }
}
function loop() {
  if (looping) return;
  looping = true;
  const tick = () => {
    if (ui.view === "map" && (ui.motion || dirty || camAnim)) { render(); dirty = false; }
    if (!ui.motion && !camAnim && !dirty) { looping = false; return; }
    requestAnimationFrame(tick);
  };
  requestAnimationFrame(tick);
}
// without motion the frame is drawn when something changed
setInterval(() => { if (dirty && !looping) loop(); }, 100);

addEventListener("resize", () => { view.resize(); dirty = true; loop(); });
reduce.addEventListener("change", () => { if (!params.get("motion")) { ui.motion = !reduce.matches; $("motion").setAttribute("aria-pressed", String(ui.motion)); dirty = true; loop(); } });

// ---------------------------------------------------------------- start
view.resize();
const barH = () => { document.documentElement.style.setProperty("--bar-h", $("bar").offsetHeight + "px"); view.resize(); };
barH();
if (window.ResizeObserver) new ResizeObserver(barH).observe($("bar"));
else addEventListener("resize", barH);
if (ui.view === "list") setView("list");
refresh().then(refreshSeries).then(() => loop());
setInterval(refresh, 3000);
setInterval(refreshSeries, 60000);
setInterval(() => renderStale(lastOK), 1000);
loop();
