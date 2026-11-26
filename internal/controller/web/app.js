// pumasi dashboard: reads /api/state, /api/jobs and /api/series every 5 seconds
// and fills plain tables. Read-only; no dependencies.
"use strict";

const $ = (id) => document.getElementById(id);
const el = (tag, props = {}, ...kids) => {
  const e = Object.assign(document.createElement(tag), props);
  for (const k of kids) e.append(k);
  return e;
};
const td = (text, cls = "") => el("td", { className: cls, textContent: text });

const gib = (b) => (b / 2 ** 30).toFixed(1) + "G";
const mib = (b) => Math.round(b / 2 ** 20) + "M";
function dur(sec) {
  sec = Math.max(0, Math.round(sec));
  if (sec < 60) return sec + "s";
  if (sec < 3600) return Math.floor(sec / 60) + "m" + String(sec % 60).padStart(2, "0") + "s";
  return Math.floor(sec / 3600) + "h" + String(Math.floor((sec % 3600) / 60)).padStart(2, "0") + "m";
}
const clock = (iso) => new Date(iso).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false });

// bar shows use against a limit: a thin track and the numbers beside it.
function bar(used, limit, text) {
  const pct = limit > 0 ? Math.min(100, (100 * used) / limit) : 0;
  const cell = el("td");
  const track = el("span", { className: "bar" }, el("i"));
  track.firstChild.style.width = pct + "%";
  cell.append(track, text);
  return cell;
}

function fill(tableId, rows, emptyId) {
  const table = $(tableId);
  table.tBodies[0].replaceChildren(...rows);
  if (emptyId) {
    $(emptyId).hidden = rows.length > 0;
    table.hidden = rows.length === 0;
  }
}

function renderState(s) {
  $("url").textContent = s.url.replace(/^https?:\/\//, "");

  $("problems").hidden = s.incidents.length === 0;
  $("problem-list").replaceChildren(...s.incidents.map((i) =>
    el("li", {}, i.detail, el("span", { className: "age", textContent: "open " + dur((Date.now() - new Date(i.openedAt)) / 1000) }))));

  fill("hosts", s.hosts.map((h) => {
    const state = h.reachable === null ? "unknown" : h.reachable ? (h.closed || "ok") : "unreachable";
    return el("tr", {},
      td(h.name), td(h.kind, "muted opt"), td(h.os || "-", "muted opt"),
      bar(h.usedCpus, h.budgetCpus, h.usedCpus + "/" + h.budgetCpus),
      bar(h.usedMem, h.budgetMem, gib(h.usedMem) + "/" + gib(h.budgetMem)),
      td(h.cpuPressure === null ? "-" : h.cpuPressure.toFixed(1) + "%", "num opt"),
      td(h.diskFreePct === null ? "-" : h.diskFreePct.toFixed(0) + "%", "num opt"),
      td(String(h.runners), "num"),
      td(state, state === "ok" ? "muted" : "problem"),
      td(h.seenSeconds === null ? "-" : dur(h.seenSeconds) + " ago", "num opt"),
      td(h.seenSeconds === null ? "-" : Math.round(h.rttMs) + "ms", "num opt"),
      td(h.agent || "-", "muted opt"), td(h.lastDrop || "-", "muted opt"));
  }));

  fill("runners", s.runners.map((r) => el("tr", {},
    td(r.name), td(r.class, "opt"), td(r.host), td(r.state, "muted"), td(r.job || "-"),
    td(dur(r.ageSeconds), "num opt"),
    td(r.cpuSec ? r.cpuSec.toFixed(0) + "s" : "-", "num opt"),
    r.memBytes ? bar(r.memBytes, r.memLimit, mib(r.memBytes) + "/" + gib(r.memLimit)) : td("-"))), "runners-empty");

  fill("waiting", s.waiting.map((w) => el("tr", {},
    td(w.class, "waiting"), td(String(w.jobs), "num"), td(dur(w.oldestSeconds), "num"), td(w.reason || "-", "wrap"))), "waiting-empty");
}

// Results: GitHub's word for a job; a runner that never got one shows why it ended.
function resultCell(j) {
  if (j.result === "succeeded") return td("succeeded", "muted");
  if (j.oomKills) return td((j.result || j.state) + " · out of memory", "problem");
  if (j.result) return td(j.result, "problem");
  // No result from GitHub. A runner taken away by a person is not a failure.
  if (j.reason === "down") return td("unknown: removed by pumasi down", "muted");
  if (j.reason === "controller stopped") return td("unknown: removed when the controller stopped", "muted");
  if (j.state === "failed" || j.state === "lost") return td(j.state + (j.reason ? ": " + j.reason : ""), "problem wrap");
  return td("unknown", "muted");
}

// Recent jobs lists jobs: a runner that never took one is not a row here.
const SHORT = 15;
let openJob = "", showAll = false, lastJobs = [];
function renderJobs(all) {
  lastJobs = all;
  const jobs = all.filter((j) => j.job || j.startedAt);
  const rows = [];
  for (const j of showAll ? jobs : jobs.slice(0, SHORT)) {
    const tr = el("tr", { className: "job" },
      td(j.finishedAt ? clock(j.finishedAt) : clock(j.createdAt), "muted"),
      td(j.job || "(name unknown)"), resultCell(j), td(j.class, "opt"), td(j.host),
      td(j.startedAt && j.job ? dur(j.waitSec) : "-", "num opt"),
      td(j.finishedAt && j.startedAt ? dur(j.runSec) : "-", "num"),
      td(j.cpuSec ? j.cpuSec.toFixed(0) + "s" : "-", "num opt"),
      td(j.memPeak ? mib(j.memPeak) : "-", "num"));
    tr.onclick = () => { openJob = openJob === j.runner ? "" : j.runner; renderJobs(all); };
    rows.push(tr);
    if (openJob === j.runner) rows.push(detailRow(j));
  }
  fill("jobs", rows, "jobs-empty");
  const more = $("jobs-more");
  more.hidden = jobs.length <= SHORT;
  more.textContent = showAll ? "show fewer" : "show all " + jobs.length;
}
$("jobs-more").onclick = () => { showAll = !showAll; renderJobs(lastJobs); };

function detailRow(j) {
  const cell = el("td", { colSpan: 9 }, el("div", { className: "muted", textContent: j.runner + (j.exitCode === null ? "" : " · exit code " + j.exitCode) + (j.provisionMs ? " · ready in " + (j.provisionMs / 1000).toFixed(1) + "s" : "") }));
  const list = el("ol");
  const log = el("pre", { textContent: "loading…" });
  cell.append(list, log);
  fetch("api/job?runner=" + encodeURIComponent(j.runner)).then((r) => r.json()).then((d) => {
    list.replaceChildren(...d.decisions.map((x) => el("li", {}, clock(x.at) + "  " + x.action + (x.host ? " on " + x.host : "") + "  " + x.reason)));
    log.textContent = d.logTail || "(no runner output kept)";
  }).catch(() => { log.textContent = "(could not load)"; });
  return el("tr", { className: "detail" }, cell);
}

// One small line chart: lines = [{name, values, dash, cls}], shared y scale from zero.
function chart(title, t, lines, unit, limit) {
  const W = 360, H = 120, L = 30, B = 16, T = 6;
  let max = limit || 0;
  for (const l of lines) for (const v of l.values) if (v > max) max = v;
  if (max <= 0) max = 1;
  const x = (i) => L + ((W - L) * i) / (t.length - 1);
  const y = (v) => T + (H - T - B) * (1 - v / max);
  const ns = "http://www.w3.org/2000/svg";
  const s = (tag, attrs) => { const e = document.createElementNS(ns, tag); for (const k in attrs) e.setAttribute(k, attrs[k]); return e; };
  const svg = s("svg", { viewBox: `0 0 ${W} ${H}`, preserveAspectRatio: "none", role: "img", "aria-label": title });
  svg.append(s("line", { x1: L, x2: W, y1: y(0), y2: y(0), stroke: "#1c1c1c", "stroke-width": 1 }),
    s("line", { x1: L, x2: W, y1: y(max), y2: y(max), stroke: "#dedede", "stroke-width": 1 }));
  const label = (txt, xx, yy, anchor) => { const e = s("text", { x: xx, y: yy, "text-anchor": anchor, "font-size": 10, fill: "#6a6a6a" }); e.textContent = txt; return e; };
  svg.append(label(unit(max), L - 4, y(max) + 3, "end"), label("0", L - 4, y(0) + 3, "end"),
    label("24h ago", L, H - 3, "start"), label("now", W, H - 3, "end"));
  const legend = el("div", { className: "legend" });
  for (const l of lines) {
    let d = "", pen = false;
    l.values.forEach((v, i) => {
      if (v < 0) { pen = false; return; } // a gap: nothing happened in that step
      d += (pen ? "L" : "M") + x(i).toFixed(1) + " " + y(v).toFixed(1);
      pen = true;
    });
    const stroke = l.color || "#3a3a3a";
    svg.append(s("path", { d, fill: "none", stroke, "stroke-width": 1.5, "stroke-dasharray": l.dash || "", "vector-effect": "non-scaling-stroke" }));
    if (l.dots) l.values.forEach((v, i) => { if (v >= 0) svg.append(s("circle", { cx: x(i), cy: y(v), r: 1.6, fill: stroke })); });
    const key = s("svg", { viewBox: "0 0 22 8" });
    key.append(s("line", { x1: 0, x2: 22, y1: 4, y2: 4, stroke, "stroke-width": 1.5, "stroke-dasharray": l.dash || "" }));
    legend.append(el("span", {}, key, l.name));
  }
  return el("div", { className: "chart" }, el("h3", { textContent: title }), svg, legend);
}

function renderSeries(se, state) {
  const dashes = ["", "5 3", "2 2", "8 2 2 2"];
  const hosts = Object.keys(se.hostCpus).sort();
  const budget = Math.max(0, ...state.hosts.map((h) => h.budgetCpus));
  $("charts").replaceChildren(
    chart("CPUs held by runners, per host", se.t, hosts.map((h, i) => ({ name: h, values: se.hostCpus[h], dash: dashes[i % dashes.length] })), (v) => String(Math.round(v)), budget),
    chart("Jobs waiting", se.t, [{ name: "waiting", values: se.waiting, color: "#8a5a00" }], (v) => String(Math.round(v))),
    chart("Wait before a runner took the job (median)", se.t, [{ name: "median wait", values: se.waitMedianSec, color: "#8a5a00", dots: true }], dur));
}

let lastOK = 0, lastState = null;
async function refresh() {
  try {
    const [state, jobs] = await Promise.all([fetch("api/state").then((r) => r.json()), fetch("api/jobs").then((r) => r.json())]);
    lastState = state;
    renderState(state);
    renderJobs(jobs);
    lastOK = Date.now();
  } catch (e) { /* the controller is gone or restarting; the header says so */ }
  const age = (Date.now() - lastOK) / 1000;
  $("updated").textContent = lastOK ? (age < 8 ? "updated " + clock(new Date(lastOK)) : "controller not answering for " + dur(age)) : "controller not answering";
  $("updated").className = age < 8 && lastOK ? "" : "stale";
}
async function refreshSeries() {
  try {
    const se = await fetch("api/series").then((r) => r.json());
    if (lastState) renderSeries(se, lastState);
  } catch (e) { /* keep the last charts */ }
}

refresh().then(refreshSeries);
setInterval(refresh, 5000);
setInterval(refreshSeries, 60000);
