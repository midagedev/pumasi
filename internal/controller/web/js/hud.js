// The HUD and the list view: every fact the plant shows, as text, with the
// command to type next to each problem. Agents read this; people click it.
import { el, gib, mib, dur, clock, since, size, osBadge, parseReason, rgb, GiB, gb, gbn, cpus, ago, hostLoad, plainReason, levelWord, kindWord, expectText, isReady, aheadOf, overflowWords } from "./util.js";
import { classesOf } from "./world.js";

const $ = (id) => document.getElementById(id);
// runCount: "3 running · 2 ready", the header chip's words for a few runners.
// aheadText: what a runner started ahead of a job is for, in a sentence.
const aheadText = (r, held) => {
  const A = aheadOf(r);
  return `Started ahead: ${A.job} needs ${A.after || "another job"} (its workflow's needs:), so GitHub asks for its runner only once that one ends. `
    + `This one was started as it was about to end, to be up when the job comes instead of after. It holds ${gb(held)}, that job's size; if the job does not come it goes at the idle timeout.`;
};
const runCount = (rs) => [`${rs.filter((r) => r.job).length} running`, ...(rs.some(isReady) ? [`${rs.filter(isReady).length} ready`] : [])].join(" · ");
export const store = { state: null, jobs: [], findings: [], explainText: "", prio: null, prioMissing: false, series: null, base: "" };

// ---------------------------------------------------------------- writes
// Writes go to the controller's priority API. A server without it answers
// 404/405 with no JSON: then the same thing is shown as a command to type.
// The controller takes a write from its own machine (JSON only), and with
// its dashboardToken from elsewhere — and from everyone when the dashboard
// listens beyond loopback and has a token. A refusal is {"ok":false,"error":…}.
const loopback = ["127.0.0.1", "localhost", "::1", "[::1]"].includes(location.hostname);
let tokenAsked = false; // a write was refused for want of the token: show the field everywhere
export function token() {
  try { return sessionStorage.getItem("pumasi.token") || ""; } catch (e) { return ""; }
}
function writer() {
  try { return localStorage.getItem("pumasi.user") || ""; } catch (e) { return ""; }
}
export async function write(method, path, body) {
  const r = await send(method, path, body);
  store.keepUntil = r.ok ? 0 : Date.now() + 30000;
  if (r.status === 401) {
    tokenAsked = true;
    for (const f of document.querySelectorAll("[data-token-field]")) f.style.display = "";
  }
  return r;
}
async function send(method, path, body) {
  // a controller that does not list the priority API has none: say so without asking
  if (!((store.state && store.state.api) || []).includes("priority")) return { ok: false, status: 404, missing: true, error: "no such API" };
  // the change is recorded as made by "web [name@]address"
  const headers = { "Content-Type": "application/json", "X-Pumasi-Client": "web" };
  if (token()) headers.Authorization = "Bearer " + token();
  if (writer()) headers["X-Pumasi-User"] = writer();
  let r;
  try {
    // a demo pinned with ?at= is asked about that same moment
    r = await fetch(path + atParam(path.includes("?") ? "&" : "?"), { method, headers, body: body ? JSON.stringify(body) : undefined });
  } catch (e) {
    return { ok: false, status: 0, error: "the controller did not answer" };
  }
  let data = null;
  try { data = await r.json(); } catch (e) { /* not JSON: a server without this API */ }
  if (r.ok) return { ok: true, status: r.status, data };
  const missing = (r.status === 404 || r.status === 405 || r.status === 501) && !(data && data.error);
  return { ok: false, status: r.status, missing, error: (data && (data.error || data.note)) || r.statusText || "refused" };
}

// ---------------------------------------------------------------- small parts
export function copyable(cmd, where, why) {
  const btn = el("button", { class: "copy", type: "button", text: "copy", "aria-label": "copy " + cmd });
  btn.addEventListener("click", () => copy(cmd, btn));
  return el("div", { class: "step" }, el("code", { text: cmd }), btn, where || why ? el("small", { text: [where, why].filter(Boolean).join(" · ") }) : null);
}
function copy(text, btn) {
  const done = () => { btn.textContent = "copied"; btn.classList.add("done"); setTimeout(() => { btn.textContent = "copy"; btn.classList.remove("done"); }, 1400); };
  if (navigator.clipboard && window.isSecureContext) navigator.clipboard.writeText(text).then(done, () => fallbackCopy(text, done));
  else fallbackCopy(text, done);
}
function fallbackCopy(text, done) {
  const t = el("textarea", { style: "position:fixed;left:-9999px" });
  t.value = text;
  document.body.append(t);
  t.select();
  try { document.execCommand("copy"); done(); } catch (e) { /* nothing to do */ }
  t.remove();
}
function findingEl(f) {
  return el("div", { class: "finding " + f.level, "data-kind": f.kind, "data-host": f.host || null, "data-class": f.class || null, "data-runner": f.runner || null },
    el("div", { class: "text", text: f.text }),
    (f.next || []).map((s) => s.run ? copyable(s.run, s.where, s.why) : s.config ? copyable(s.config, s.where || "pumasi.yaml", s.why) : el("div", { class: "note", text: [s.where, s.why].filter(Boolean).join(" · ") })));
}
const facts = (pairs) => el("dl", { class: "facts" }, pairs.filter((p) => p && p[1] !== undefined && p[1] !== null && p[1] !== "").map(([k, v]) => [el("dt", { text: k }), el("dd", {}, v)]));
const section = (title, ...kids) => el("section", {}, el("h2", { text: title }), ...kids);
const dashURL = (p) => store.base + p;
const curl = (p) => `curl -s '${dashURL(p)}'`;

function memBar(parts) {
  const total = parts.reduce((a, p) => a + Math.max(0, p[1]), 0) || 1;
  return el("div", {},
    el("div", { class: "bar2", role: "img", "aria-label": parts.map((p) => `${p[0]} ${p[3] || gib(p[1])}`).join(", ") },
      parts.filter((p) => p[1] > 0).map((p) => el("i", { style: `width:${(100 * p[1]) / total}%;background:${p[2]}`, title: `${p[0]} ${p[3] || gib(p[1])}` }))),
    el("div", { class: "legend" }, parts.filter((p) => p[1] > 0).map((p) => el("span", {}, el("i", { style: "background:" + p[2] }), `${p[0]} ${p[3] || gib(p[1])}`))));
}
const ZC = { held: "#e0a42e", kept: "#a07a30", give: "repeating-linear-gradient(-45deg,#60b0ff 0 3px,#35618c 3px 6px)", free: "#3fae74", reserve: "#9b7ef0", keep: "#8a7a3a", others: "#8d8d96", unlent: "#2a2e35" };

export function hostState(h) {
  if (h.reachable === null || h.reachable === undefined) return ["unknown", "off"];
  if (!h.reachable) return ["unreachable", "bad"];
  if (h.closed) return [h.closed, "warn"];
  return ["ok", "ok"];
}

// ---------------------------------------------------------------- header, index, alarms
export function renderChrome(onSelect) {
  const s = store.state;
  if (!s) return;
  $("where").textContent = (s.url || "").replace(/^https?:\/\//, "");
  $("demo").hidden = !s.demo;
  const { waiting, starting } = waitCounts(s);
  const problems = s.incidents.length;
  const down = s.hosts.filter((h) => h.reachable === false).length;
  $("counts").replaceChildren(
    el("span", { class: "chip" + (down ? " bad" : "") }, el("b", { text: s.hosts.length }), " hosts" + (down ? ` · ${down} offline` : "")),
    el("span", { class: "chip" }, el("b", { text: s.runners.filter((r) => r.job).length }), " running" + (s.runners.some(isReady) ? ` · ${s.runners.filter(isReady).length} ready` : "")),
    el("span", { class: "chip" + (waiting ? " warn" : "") }, el("b", { text: waiting }), " waiting" + (starting ? ` · ${starting} ${starting === 1 ? "job" : "jobs"} starting` : "")),
    el("span", { class: "chip opt" + (problems ? " bad" : "") }, el("b", { text: problems }), problems === 1 ? " problem now" : " problems now"));
  document.querySelector("#scene").setAttribute("aria-label", `The plant: ${s.hosts.length} hosts as halls, ${s.runners.length} runners as machines, ${waiting} waiting jobs as crates on their belts, ${problems} open problems. The List view has the same as tables.`);

  const sel = onSelect.current || {};
  $("index-hosts").replaceChildren(...s.hosts.map((h) => {
    const L = hostLoad(h, s.runners);
    const gauge = (label, p, free) => el("span", { class: "g" }, el("span", { class: "gl", text: label }),
      el("span", { class: "gbar" }, p ? [el("i", { class: "mine " + (L.tone === "ready" ? "warn" : L.tone), style: `width:${((p.seg.held + p.seg.kept) * 100).toFixed(1)}%` }), el("i", { class: "give", style: `width:${(p.seg.give * 100).toFixed(1)}%` }), el("i", { class: "other", style: `width:${(p.seg.other * 100).toFixed(1)}%` }),
        el("b", { class: "used", style: `width:${(p.seg.used * 100).toFixed(1)}%` })] : null),
      el("span", { class: "gp" + (p && p.free <= 0.05 * p.lend ? (p.room <= 0.05 * p.lend ? " short" : " ready") : ""), text: p ? free + " free" : "-" }));
    const off = L.word === "offline" || L.word === "no data";
    const ready = L.giving ? `${L.giving} ready ${L.giving === 1 ? "runner gives" : "runners give"} way: ${gb(L.mem ? L.mem.give : 0)}${L.cpu ? " · " + cpus(L.cpu.give) : ""}` : "";
    const shared = L.share > 1 ? `cores shared ×${L.share}: ${h.cpus} cores count as ${short(L.cpu ? L.cpu.lend : 0)}` : "";
    return el("li", {}, el("button", {
      type: "button", "data-host": h.name, "data-state": L.word, "aria-current": sel.kind === "host" && sel.id === h.name ? "true" : "false",
      "aria-label": `host ${h.name}: ${L.word}${L.why ? " (" + L.why + ")" : ""}, ${L.cpu ? cpus(L.cpu.free) : "-"} and ${L.mem ? gb(L.mem.free) : "-"} free, ${h.runners} runners${ready ? ", " + ready : ""}`,
      onclick: () => onSelect({ kind: "host", id: h.name }),
    }, el("span", { class: "dot " + L.tone }), el("span", { class: "name", text: h.name }), el("span", { class: "word " + L.tone, text: off ? L.word : L.word + (L.why ? ": " + L.why : "") }),
      off ? el("span", { class: "sub", text: `${L.why ? L.why + " · " : ""}${h.runners} ${h.runners === 1 ? "job" : "jobs"} still running` })
        : el("span", { class: "gauges" }, gauge("CPU", L.cpu, L.cpu ? short(L.cpu.free) : ""), gauge("Mem", L.mem, L.mem ? short(L.mem.free / GiB) + "G" : "")),
      ready && !off ? el("span", { class: "sub ready", text: ready }) : null, shared && !off ? el("span", { class: "sub", text: shared }) : null));
  }));

  // the waiting jobs, in the order they get room, each with why in plain words
  const jobs = waitRows(s);
  const alert = (s.limits && s.limits.waitAlertSeconds) || 600;
  $("index-toggle").textContent = `Waiting (${waiting}) & hosts`;
  $("waiting-title").replaceChildren(...["Waiting ", el("b", { text: waiting }), starting ? el("small", { text: `+${starting} ${starting === 1 ? "job" : "jobs"} starting` }) : null, el("small", { class: "hint", text: "first gets room first" })].filter(Boolean));
  // jobs a runner is already starting for: one line, so what really waits stands out
  const going = jobs.filter((j) => j._go), held = jobs.filter((j) => !j._go);
  const show = held.slice(0, 7);
  $("index-waiting").replaceChildren(...(going.length ? [el("li", { class: "go starting" }, el("span", {}, el("b", { text: "Starting now: " }), going.map((j) => j.job || "a job").join(", ")))] : []), ...show.map((j, i) => {
    const lw = levelWord(j);
    return el("li", { class: j.waitedSeconds > alert ? "hot" : "" },
      el("button", { type: "button", class: "wj", "data-job-id": j.githubJobId || null, "data-run-id": j.runId || null, "data-class": j.class,
        "aria-current": sel.kind === "job" && sel.id === jobKey(j) ? "true" : "false", onclick: () => onSelect({ kind: "job", id: jobKey(j) }) },
      el("span", { class: "pos", text: i + 1, title: j.position ? ordinal(j.position) + " in the whole line, counting jobs already starting" : null }),
      el("span", { class: "jn" }, lw ? el("span", { class: "lvl " + (/expedited/.test(lw) ? "expedite" : lw), text: lw }) : null, " ", j.job || "(name not known yet)"),
      el("span", { class: "age", text: ago(j.waitedSeconds) }),
      el("span", { class: "why", text: whyText(j) }),
      j.expect ? el("span", { class: "eta", text: expectText(j.expect, store.state.runners, j.job) }) : null),
      // the first in line gets room first already: nothing to put it before
      j.expedited || i === 0 ? null : el("button", { type: "button", class: "runnext", "data-action": "run-next", title: "Expedite: put this job first and keep room for it", "aria-label": "run " + (j.job || "this job") + " next",
        onclick: () => onSelect({ kind: "job", id: jobKey(j), expedite: true }) }, "Run next"));
  }), ...(held.length > show.length ? [el("li", { class: "more" }, el("button", { type: "button", onclick: () => onSelect({ kind: "priority" }) }, `+${held.length - show.length} more: open Priority`))] : []),
  ...(held.length ? [] : [el("li", { class: "none", text: "nothing waits for room" })]));

  const classes = classesOf(s);
  $("index-lines").replaceChildren(...classes.map((c) => {
    const w = s.waiting.find((x) => x.class === c.name);
    return el("li", {}, el("button", {
      type: "button", "data-class": c.name, "aria-current": sel.kind === "class" && sel.id === c.name ? "true" : "false",
      "aria-label": `class ${c.name}: ${w ? w.jobs + " waiting, place " + (w.order || "-") : "nothing waiting"}`,
      title: w ? `${w.jobs} waiting · ${ordinal(w.order)} in line · oldest ${ago(w.oldestSeconds)}` : "nothing waiting",
      onclick: () => onSelect({ kind: "class", id: c.name }),
    }, el("span", { class: "swatch", style: "background:" + rgb(c.color) }), el("span", { class: "name", text: c.name }),
      el("span", { class: "n" + (w && w.oldestSeconds > alert ? " hot" : w ? " warn" : ""), text: w ? w.jobs : "0" })));
  }));

  const items = [
    ...s.incidents.map((i) => ({ cls: "problem", kind: i.kind, label: kindWord(i.kind), text: i.detail, age: "for " + ago(since(i.openedAt)), sel: subjectOf(i) })),
    ...(s.warnings || []).map((w) => ({ cls: "warning", kind: "warning", label: "warning", text: w })),
  ];
  $("alarms").hidden = items.length === 0;
  $("alarms").classList.toggle("warn-only", !s.incidents.length);
  $("alarm-title").textContent = s.incidents.length ? `Problems (${s.incidents.length})` : "Warnings";
  $("alarm-list").replaceChildren(...items.map((it) => el("li", { class: it.cls, "data-kind": it.kind }, el("button", { type: "button", onclick: () => it.sel && onSelect(it.sel) },
    el("span", { class: "k", text: it.label }), it.age ? el("span", { class: "age", text: it.age }) : null, el("div", { text: it.text })))));
}

// waitRows: the waiting jobs in line order, each marked go when a runner is
// already being started for it. Placement defers a class's jobs as a count
// (waiting[].jobs); the rest of its open jobs, the first in line, are starting.
export function waitRows(s) {
  const jobs = (s.waitingJobs || []).slice().sort((a, b) => (a.position || 1e9) - (b.position || 1e9));
  const deferred = new Map((s.waiting || []).map((w) => [w.class, w.jobs]));
  const seen = new Map();
  const total = new Map();
  for (const j of jobs) total.set(j.class, (total.get(j.class) || 0) + 1);
  return jobs.map((j) => {
    const k = (seen.get(j.class) || 0) + 1;
    seen.set(j.class, k);
    const go = k <= total.get(j.class) - (deferred.get(j.class) || 0);
    return { ...j, _go: go };
  }).map((j, _, all) => ({ ...j, _front: all.find((x) => x.class === j.class && !x._go) }));
}
// whyText: why a waiting job waits. Placement's reason is about its class's
// next job: the jobs behind that one say so instead of repeating its size.
export function whyText(j) {
  if (j._go) return "a runner is starting for it";
  if (j._front && jobKey(j._front) !== jobKey(j) && j.reason === j._front.reason) return `behind ${j._front.job || "an older job"}, the next ${j.class} job`;
  return plainReason(j.reason);
}
export function waitCounts(s) {
  const waiting = (s.waiting || []).reduce((a, w) => a + w.jobs, 0);
  return { waiting, starting: Math.max(0, (s.waitingJobs || []).length - waiting) };
}
const short = (v) => (v >= 9.95 || v < 0.05 ? String(Math.round(v)) : (+v).toFixed(1));
const ordinal = (n) => (!n ? "-" : n + (n % 100 >= 11 && n % 100 <= 13 ? "th" : ["th", "st", "nd", "rd"][n % 10] || "th"));

export function subjectOf(i) {
  const s = store.state;
  if (s.hosts.some((h) => h.name === i.subject)) return { kind: "host", id: i.subject };
  if (s.runners.some((r) => r.name === i.subject)) return { kind: "runner", id: i.subject };
  if (store.jobs.some((j) => j.runner === i.subject)) return { kind: "done", id: "s:" + i.subject };
  if (classesOf(s).some((c) => c.name === i.subject)) return { kind: "class", id: i.subject };
  return null;
}

export function renderStale(lastOK) {
  const age = (Date.now() - lastOK) / 1000;
  const fresh = lastOK && age < 10;
  $("updated").textContent = !lastOK ? "controller not answering" : fresh ? "live · " + clock(new Date(lastOK)) : "not answering for " + dur(age);
  $("updated").className = "updated" + (fresh ? "" : " stale");
  $("offline").hidden = fresh || !lastOK;
}

// ---------------------------------------------------------------- mode banner
export function renderMode() {
  const m = store.prio && store.prio.mode;
  const on = m && m.name && m.name !== "normal";
  $("mode").textContent = on ? "Mode: " + m.name : "Mode";
  $("mode").classList.toggle("on", !!on);
  const b = $("mode-banner");
  b.hidden = !on;
  if (on) {
    const left = m.until ? " · " + dur((new Date(m.until) - Date.now()) / 1000) + " left" : "";
    b.replaceChildren(el("span", { text: `${m.name.toUpperCase()} MODE · only high-priority and expedited jobs start; the rest wait${m.until ? " until " + clock(m.until) : ""}${left}` }));
  }
}

// ---------------------------------------------------------------- the panel
// renderPanel draws the details of the selection. A poll (opts.poll) leaves the
// panel alone while it is in use: a field being typed in, or the answer to a
// write that did not go through (the CLI line to copy) still being read.
export function renderPanel(sel, actions, opts = {}) {
  const panel = $("panel");
  if (!sel || !store.state) { panel.hidden = true; return; }
  if (opts.poll && !panel.hidden) {
    const a = document.activeElement;
    if (a && panel.contains(a) && /^(INPUT|SELECT|TEXTAREA)$/.test(a.tagName)) return;
    if (Date.now() < (store.keepUntil || 0)) return;
  }
  const body = build(sel, actions);
  if (!body) { panel.hidden = true; return; }
  panel.hidden = false;
  $("panel-body").replaceChildren(body);
}

function build(sel, actions) {
  const s = store.state;
  const mine = (pred) => store.findings.filter(pred).map(findingEl);
  switch (sel.kind) {
    case "host": {
      const h = s.hosts.find((x) => x.name === sel.id);
      if (!h) return null;
      const L = hostLoad(h, s.runners);
      const runners = s.runners.filter((r) => r.host === h.name);
      const mem = h.memBytes || 0;
      const M = L.mem, C = L.cpu;
      const parts = (p, f) => [["held by busy runners", p.held, ZC.held, f(p.held)], ["held by starting or kept runners", p.kept, ZC.kept, f(p.kept)], ["held by ready runners: gives way", p.give, ZC.give, f(p.give)],
        ["other programs, kept for the machine", p.other, ZC.others, f(p.other)], ["free", p.free, ZC.free, f(p.free)]];
      const readyN = runners.filter(isReady).length;
      return el("div", { "data-host": h.name },
        el("div", { class: "kind", text: "host · a hall on the map" }), el("h3", { text: h.name }),
        el("div", { class: "state " + L.tone }, el("span", { class: "dot " + L.tone }), " " + L.word + (L.why ? ": " + L.why : ""),
          C && M ? el("span", { class: "note", text: ` · a waiting job finds ${cpus(C.room)} and ${gb(M.room)}` + (M.give || C.give ? " (counting what ready runners give up)" : "") }) : null),
        M ? section(`Memory · ${gb(M.free)} free` + (M.give ? ` · ${gb(M.give)} more when ready runners give way` : ""), memBar(parts(M, gb)),
          el("div", { class: "note", text: `Its runners use ${gb(M.used)} now, of the ${gb(M.held + M.kept + M.give)} they hold. A runner holds the largest of its job's last runs plus 20%, or what it uses when that is more, so a job never runs short; a ready runner holds its class's largest recent job until it knows its own. ${gb(mem)} in the machine${h.budgetMem ? ", " + gb(h.budgetMem) + " lent to runners" : ""}.` })) : null,
        C ? section(`CPU · ${cpus(C.free)} free` + (C.give ? ` · ${cpus(C.give)} more when ready runners give way` : ""), memBar(parts(C, cpus)),
          el("div", { class: "note", text: `Its runners use ${cpus(C.used)} now.` + (L.share > 1 ? ` Cores shared: placement counts the ${h.cpus} cores ${L.share}× (${cpus(C.lend)}), as jobs that share a core only take longer; it stops at cpu pressure ${(s.limits || {}).psiCpu || 40}%. Memory is never shared.` : "") })) : null,
        readyN ? el("div", { class: "note ready-note", text: `${readyN} ready ${readyN === 1 ? "runner" : "runners"} (blue): up with no job yet, kept warm for their class's next job. Each takes a job of its own class at once${L.giving ? `; ${L.giving} step aside for a waiting job of another class that needs the room` : ""}.` }) : null,
        section(`Runners here (${runCount(runners)})`, runnerRows(runners, actions)),
        facts([["kind", h.kind], ["os", h.os || "-"], ["agent", h.agent || "-"], ["last heard", h.seenSeconds === null || h.seenSeconds === undefined ? "-" : ago(h.seenSeconds) + " ago · " + Math.round(h.rttMs) + "ms"], ["last dropped", h.lastDrop || "-"],
          ["shares", h.budgetCpus || h.budgetMem ? `at most ${h.budgetCpus ? cpus(h.budgetCpus) : "any CPU"} and ${h.budgetMem ? gb(h.budgetMem) : "any memory"}` : "whatever it has free"],
          ["pressure", ["cpu", "mem", "io"].map((k) => { const v = h[{ cpu: "cpuPressure", mem: "memPressure", io: "ioPressure" }[k]]; return v === null || v === undefined ? null : `${k} ${v.toFixed(1)}%`; }).filter(Boolean).join(" · ") || "-"],
          ["disk free", h.diskFreePct === null || h.diskFreePct === undefined ? "-" : h.diskFreePct.toFixed(0) + "%" + (h.diskPath ? " on " + h.diskPath : "")]]),
        section("What to do", ...mine((f) => f.host === h.name), copyable("pumasi status", "on the controller", "HELD, FREE and STATE per host"), copyable("pumasi doctor", "on " + h.name, "checks the way to the controller, what runners need there, and the agent")));
    }
    case "class": {
      const c = classesOf(s).find((x) => x.name === sel.id);
      if (!c) return null;
      const w = s.waiting.find((x) => x.class === c.name);
      const ov = (s.overflow || []).find((o) => o.class === c.name);
      const jobs = (s.waitingJobs || []).filter((j) => j.class === c.name);
      const runners = s.runners.filter((r) => r.class === c.name);
      const p = w && parseReason(w.reason);
      const back = (s.takenBack || []).filter((j) => j.class === c.name);
      return el("div", { "data-class": c.name },
        el("div", { class: "kind", text: "class · a belt on the map" }), el("h3", { text: c.name }),
        el("div", { class: "note", text: `Jobs with runs-on: [${(c.labels || ["self-hosted", "pumasi-" + c.name]).join(", ")}] come in on this belt and wait there until a host has room for them.` }),
        w ? section(`Waiting: ${w.jobs} · ${ordinal(w.order)} in line`,
          w.priority ? el("div", { class: "note", text: w.priority }) : null,
          el("div", { class: "why-big", text: plainReason(w.reason) }),
          p ? el("table", {}, el("thead", {}, el("tr", {}, el("th", { text: "host" }), el("th", { class: "num", text: "free at last try" }), el("th", { class: "num", text: "short by" }))),
            el("tbody", {}, p.hosts.map((hh) => el("tr", { "data-host": hh.name }, el("td", { text: hh.name }), el("td", { class: "num", text: hh.closed ? (/^(down|unreach)/.test(hh.note) ? "offline" : hh.note) : `${cpus(hh.cpu)} · ${gbn(hh.mem)}` }),
              el("td", { class: "num problem", text: hh.closed ? "-" : [hh.short.cpu > 0 ? cpus(hh.short.cpu) : "", hh.short.mem > 0 ? gbn(hh.short.mem) : ""].filter(Boolean).join(" · ") || "just short" }))))) : null) : section("Waiting", el("div", { class: "note", text: "nothing waits on this belt" })),
        jobs.length ? section("Jobs on the belt", jobRows(jobs, actions)) : null,
        section(`Runners (${runCount(runners)})`, runnerRows(runners, actions)),
        facts([["os / driver", `${c.os || "linux"} / ${c.driver || "-"}` + (c.docker ? " · docker " + c.docker : "")],
          ["first size guess", c.cpus ? `${cpus(c.cpus)} · ${gb(c.memory)}` : null], ["memory limit", c.limitMem ? gb(c.limitMem) : null], ["hosts", c.hosts && c.hosts.length ? c.hosts.join(", ") : "any of its OS"],
          ["kept ready", c.reserve ? c.reserve + " runner(s) kept room for" : null], ["overflow", ov ? `${overflowWords(ov)}${ov.after ? " · only while " + ov.after + " has no room" : ""}; GitHub gives the rest to fixed runners` : null],
          ["left to fixed runners (1h)", back.length ? String(back.length) : null], ["max duration", c.maxDurationSeconds ? dur(c.maxDurationSeconds) : null]]),
        section("What to do", ...mine((f) => f.class === c.name), copyable("pumasi priority", "on the controller", "the rules and where they put the jobs waiting now")));
    }
    case "runner": {
      const r = s.runners.find((x) => x.name === sel.id);
      if (!r) return null;
      const held = r.heldMem || r.estMem;
      const box = el("div", {});
      const out = el("div", { "data-runner": r.name },
        el("div", { class: "kind", text: "runner · a machine on the map" }), el("h3", { text: r.job || (r.state === "starting" ? "starting…" : r.name) }),
        el("div", { class: "note", text: `on ${r.host} · class ${r.class} · ${r.state === "busy" ? "running for " + ago(r.ageSeconds) : isReady(r) ? "ready for " + ago(r.ageSeconds) : r.state}` }),
        aheadOf(r) ? el("div", { class: "why-big ready", text: aheadText(r, held) }) : isReady(r) ? el("div", { class: "why-big ready", text: `Ready: up with no job yet. It holds ${gb(held)}, the largest recent ${r.class} job, so whichever comes fits; when it gets one it holds that job's size. ` + (r.givesWay ? "It steps aside for a waiting job of another class that needs its room." : "Its class keeps it up whatever waits (warmMin).") }) : null,
        facts([["CPU now / held", `${cpus(r.cpuNow || 0)} / ${cpus(r.heldCpus || r.estCpus)}`],
          ["memory now / held", `${r.memBytes ? gb(r.memBytes) : "-"} / ${gb(held)}`],
          ["size from", aheadOf(r) ? "the job it was started for" : isReady(r) ? "its class's largest recent job, until it has its own" : r.estRuns ? `its job's last ${r.estRuns} runs, largest + 20% (expects ${cpus(r.estCpus)} · ${gb(r.estMem)})` : r.watched ? "a first guess, watched: it holds 3× until its size shows" : "the class's first guess"],
          ["memory limit", r.memLimit ? gb(r.memLimit) + (r.memBytes > 0.9 * r.memLimit ? " — close" : "") : "-"],
          ["runner", r.name], ["slot", r.slot || "-"]]),
        r.memBytes > r.estMem && r.estMem ? el("div", { class: "note", text: "Using more than expected: it goes on, and its next run gets more room." }) : null,
        section("Decisions and log", box),
        section("What to do", ...mine((f) => f.runner === r.name), copyable(curl("api/job?runner=" + encodeURIComponent(r.name)), "", "its decisions and the end of its log")));
      loadHistory(r.name, box);
      return out;
    }
    case "job": {
      const j = (s.waitingJobs || []).find((x) => jobKey(x) === sel.id) || findJob(sel.id);
      if (!j) return null;
      const pl = (store.prio && store.prio.line) || [];
      const line = (j.id && pl.find((x) => x.job === j.id)) || (j.githubJobId && pl.find((x) => x.githubJobId === j.githubJobId)) || pl.find((x) => x.name === j.job && x.class === j.class && x.runId === j.runId) || null;
      const level = j.expedited ? "expedite" : (line && line.level) || j.level || "normal";
      const run = runURL(j);
      const row = waitRows(s).find((x) => jobKey(x) === jobKey(j));
      const pos = j.position || (line && line.position);
      return el("div", { "data-job-id": j.githubJobId || null, "data-run-id": j.runId || null, "data-class": j.class },
        el("div", { class: "kind", text: "waiting job · a crate on the map" }), el("h3", { text: j.job || "(name not known yet)" }),
        el("div", {}, el("span", { class: "lvl " + level, text: level === "expedite" ? (j.expedited ? "expedited" : "with expedited") : level }), " ", el("span", { class: "note", text: `${ordinal(pos)} in line · waiting ${ago(j.waitedSeconds)}` })),
        el("div", { class: "why-big", text: row && row._go ? "A runner is starting for it." : "Why it waits: " + (row && /^behind /.test(whyText(row)) ? whyText(row) : plainReason((line && line.waitingFor) || j.reason)) }),
        j.expect && !(row && row._go) ? el("div", { class: "eta-big", text: "Expected: " + expectText(j.expect, store.state.runners, j.job) }) : null,
        row && row._go ? null : section("Run it sooner", expediteBox(j, actions)),
        facts([["class", j.class], ["repo", j.repo], ["workflow", j.workflow], ["run", run ? el("a", { href: run, target: "_blank", rel: "noopener", text: String(j.runId) }) : String(j.runId || "-")],
          ["its place", j.ranked || (line && line.why) || "by its age"], ...(row && row._go ? [] : [["placement says", (line && line.waitingFor) || j.reason || "-"]])]),
        section("What to do", copyable("pumasi priority", "on the controller", "the line, and the rule that put this job where it stands")));
    }
    case "done": {
      const j = store.jobs.find((x) => "s:" + x.runner === sel.id);
      if (!j) return null;
      const box = el("div", {});
      const res = j.oomKills ? "out of memory" : j.result || j.state;
      const out = el("div", { "data-runner": j.runner },
        el("div", { class: "kind", text: "finished job · a pallet on the map" }), el("h3", { text: j.job || "(name unknown)" }),
        facts([["result", res], ["runner", j.runner], ["class", j.class], ["host", j.host], ["finished", j.finishedAt ? clock(j.finishedAt) + " (" + dur(since(j.finishedAt)) + " ago)" : "-"],
          ["waited", dur(j.waitSec)], ["ran", dur(j.runSec)], ["cpu", j.cpuSec ? j.cpuSec.toFixed(0) + " cpu·s" : "-"],
          ["memory peak / expected", (j.memWorkPeak || j.memPeak ? gb(j.memWorkPeak || j.memPeak) : "-") + " / " + (j.estMem ? gb(j.estMem) : "-")], ["exit", j.exitCode === null || j.exitCode === undefined ? "-" : j.exitCode]]),
        section("Decisions and log", box),
        section("What to do", copyable(curl("api/job?runner=" + encodeURIComponent(j.runner)), "", "the same as text")));
      loadHistory(j.runner, box);
      return out;
    }
    case "explain": {
      const pre = el("pre", { text: store.explainText || "loading…" });
      return el("div", {}, el("div", { class: "kind", text: "for agents and scripts" }), el("h3", { text: "Explain" }),
        el("div", { class: "note", text: "What an agent reads: the same facts as the map, the waiting order, and the command to type for each problem." }),
        copyable(curl("api/explain"), "", "text; ?format=json for the findings"), el("section", {}, pre),
        section("Findings", ...(store.findings.length ? store.findings.map(findingEl) : [el("div", { class: "note", text: "nothing to point at" })])));
    }
    case "priority": return priorityPanel(actions);
    case "mode": return modePanel(actions);
  }
  return null;
}

// pumasi's id first: it stays the same when GitHub's id of the job becomes known
export const jobKey = (j) => (j.id ? "i:" + j.id : j.githubJobId ? "g:" + j.githubJobId : "r:" + j.class + "|" + j.runId + "|" + j.job);
function findJob(id) {
  return (store.state.waitingJobs || []).find((x) => x.id === id || String(x.githubJobId) === id || String(x.runId) === id || x.job === id);
}
function runURL(j) {
  const u = store.state.url || "";
  return j.runId && /^https:\/\/github\.com\/[^/]+\/[^/]+$/.test(u) ? `${u}/actions/runs/${j.runId}` : "";
}

const sz = (c, b) => `${cpus(c)} · ${gb(b)}`;
function runnerRows(runners, actions) {
  if (!runners.length) return el("div", { class: "note", text: "none" });
  return el("ul", { class: "rows" }, runners.map((r) => el("li", {}, el("button", { type: "button", "data-runner": r.name, onclick: () => actions.select({ kind: "runner", id: r.name }) },
    el("span", { text: r.job || (aheadOf(r) ? "ahead: " + aheadOf(r).job : isReady(r) ? "ready" : r.state) }), el("span", { text: r.state === "starting" ? `${r.class} · starting` : isReady(r) ? `${r.class} · holds ${gb(r.heldMem || r.estMem)}${r.givesWay ? " · gives way" : ""}` : `${r.class} · uses ${gb(r.memBytes || 0)} of ${gb(r.heldMem || r.estMem)}` })))));
}
function jobRows(jobs, actions) {
  return el("ul", { class: "rows" }, jobs.map((j) => el("li", {}, el("button", { type: "button", "data-job-id": j.githubJobId || null, "data-run-id": j.runId, onclick: () => actions.select({ kind: "job", id: jobKey(j) }) },
    el("span", {}, j.expedited ? el("span", { class: "lvl expedite", text: "expedite" }) : j.level ? el("span", { class: "lvl " + j.level, text: j.level }) : null, " " + (j.job || "-")),
    el("span", { text: (j.position ? ordinal(j.position) + " · " : "") + ago(j.waitedSeconds) })))));
}

async function loadHistory(runner, box) {
  box.replaceChildren(el("div", { class: "note", text: "loading…" }));
  try {
    const d = await fetch("api/job?runner=" + encodeURIComponent(runner) + atParam("&")).then((r) => r.json());
    box.replaceChildren(
      el("ol", { class: "decisions" }, (d.decisions || []).map((x) => el("li", { text: clock(x.at) + "  " + x.action + (x.host ? " on " + x.host : "") + "  " + x.reason }))),
      el("pre", { text: d.logTail || "(no runner output kept)" }));
  } catch (e) {
    box.replaceChildren(el("div", { class: "err", text: "could not load" }));
  }
}

// ---------------------------------------------------------------- priority: expedite, mode, rules
function fallback(box, cmd, why) {
  box.replaceChildren(el("div", { class: "note", text: why }), copyable(cmd, "on the controller", ""));
}
// What names a waiting job to the controller: pumasi's id, else GitHub's job
// id, else its run's URL with its name (a run may wait with several jobs).
function jobRef(j) {
  if (j.id) return { body: { job: j.id }, query: j.id, cli: j.id };
  if (j.githubJobId) return { body: { job: j.githubJobId }, query: String(j.githubJobId), cli: String(j.githubJobId) };
  const run = runURL(j);
  if (run && j.job) return { body: { job: run, name: j.job }, query: "", cli: `'${run}' --name '${j.job}'` };
  return null;
}
// the controller's answer to the last expedite of each job, kept while the panel is drawn again
const answered = new Map();
function expediteBox(j, actions) {
  const box = el("div", {});
  const ref = jobRef(j);
  const key = jobKey(j);
  const msg = el("div", { class: "note", "data-answer": "", text: answered.get(key) || "" });
  const go = async (clear) => {
    if (!ref || (clear && !ref.query)) { msg.textContent = "this job has no id the controller knows yet: use `pumasi expedite` on the controller"; return; }
    msg.textContent = clear ? "putting it back…" : "expediting…";
    const r = clear ? await write("DELETE", "api/expedite?job=" + encodeURIComponent(ref.query)) : await write("POST", "api/expedite", ref.body);
    if (r.ok) {
      const x = r.data || {};
      const where = x.pending ? `GitHub still holds it for class ${x.class}` : x.position ? `place ${x.position} in line` : "a runner of its class is up for it";
      msg.textContent = clear ? "back to its own place by age" : `${x.already ? "already expedited" : "expedited"}: ${where}${x.note ? " — " + x.note : ""}`;
      answered.set(key, msg.textContent);
      actions.refresh();
    } else if (r.missing) {
      fallback(box, clear ? `pumasi expedite --clear ${ref.cli}` : `pumasi expedite ${ref.cli}`, "This controller has no expedite API. The same from a shell:");
    } else msg.textContent = r.error;
  };
  box.append(el("div", { class: "note", text: j.expedited ? "Expedited: it goes before every job that is not, with the older jobs of its class (GitHub gives a class its oldest job first), and the room it needs on a host is kept for it. Running jobs are never stopped for it." : "Run next (expedite) puts this job first in line and keeps the room it needs on a host. Running jobs are not stopped." }),
    el("div", { class: "act" }, j.expedited
      ? el("button", { type: "button", class: "secondary", "data-action": "unexpedite", onclick: () => go(true) }, "Put it back in line")
      : el("button", { type: "button", class: "primary", "data-action": "expedite", onclick: () => go(false) }, "Run next (expedite)")), msg, tokenField());
  return box;
}

// Who the change is recorded as made by (optional), and the controller's
// token: asked for off loopback, after a write was refused for it, or when
// one is kept already.
function tokenField() {
  const name = el("input", { type: "text", placeholder: "your name (optional, kept with the change)", value: writer(), maxlength: "32", "aria-label": "your name for the record" });
  name.addEventListener("change", () => { try { localStorage.setItem("pumasi.user", name.value.trim()); } catch (e) { /* not kept */ } });
  const inp = el("input", { type: "password", placeholder: "dashboardToken of the controller", value: token(), "aria-label": "token for writes" });
  inp.addEventListener("change", () => { try { sessionStorage.setItem("pumasi.token", inp.value); } catch (e) { /* kept for this page only */ } });
  const tok = el("div", { class: "step", "data-token-field": "" }, inp, el("small", { text: "Writes from elsewhere, or to a dashboard open beyond loopback, need the config's dashboardToken (sent as Authorization: Bearer)." }));
  if (loopback && !tokenAsked && !token()) tok.style.display = "none"; // not hidden=: .step sets display
  return el("div", {}, el("div", { class: "step" }, name), tok);
}

function modePanel(actions) {
  const m = (store.prio && store.prio.mode) || { name: "normal" };
  const msg = el("div", { class: "note" });
  const box = el("div", {});
  const set = async (name, forDur) => {
    msg.textContent = "…";
    const r = await write("POST", "api/mode", forDur ? { name, for: forDur } : { name });
    // the controller answers with the mode itself: {name, until, by, at}
    if (r.ok) { const m = (r.data && (r.data.name ? r.data : r.data.mode)) || { name }; msg.textContent = "mode: " + m.name + (m.until ? " until " + clock(m.until) : ""); actions.refresh(); }
    else if (r.missing) fallback(box, name === "normal" ? "pumasi mode normal" : `pumasi mode ${name} --for ${forDur}`, "This controller has no mode API. The same from a shell:");
    else msg.textContent = r.error;
  };
  return el("div", {}, el("div", { class: "kind", text: "operating mode" }), el("h3", { text: m.name === "normal" ? "Normal" : m.name }),
    m.until ? el("div", { class: "note", text: "until " + clock(m.until) + (m.by ? " · set by " + m.by : "") }) : null,
    el("div", { class: "note", text: "High-only: for a while, only high-priority and expedited jobs get new runners; normal and low jobs wait at their belt's gate. Nothing running is stopped." }),
    el("div", { class: "act" }, ["30m", "1h", "2h"].map((d) => el("button", { type: "button", class: "primary", "data-action": "mode-high-only", onclick: () => set("high-only", d) }, "High-only for " + d)),
      el("button", { type: "button", class: "secondary", "data-action": "mode-normal", onclick: () => set("normal") }, "normal")),
    msg, box, tokenField());
}

function priorityPanel(actions) {
  const s = store.state, p = store.prio;
  const lineRows = (p && p.line) || (s.waitingJobs || []).map((j) => ({ name: j.job, class: j.class, level: j.expedited ? "expedite" : j.level || "normal", position: j.position, waited: dur(j.waitedSeconds), why: j.ranked || j.reason || "by its age", githubJobId: j.githubJobId, _j: j }));
  lineRows.sort((a, b) => (a.position || 1e9) - (b.position || 1e9));
  // placement's reason is the class's: of its next job. One behind it in the class, short of the
  // same room, waits for that one (not one whose runner is coming up)
  const front = {};
  const whyRow = (r) => {
    const lack = parseReason(r.waitingFor);
    const f = front[r.class] || (lack ? (front[r.class] = r) : null);
    if (f && f !== r && lack && r.waitingFor === f.waitingFor) return `behind ${f.name || "an older job"}, the next ${r.class} job`;
    return r.waitingFor ? plainReason(r.waitingFor) : "";
  };
  const rules = (p && p.rules) || [];
  const exp = (p && p.expedited) || [];
  const box = el("div", {});
  return el("div", {}, el("div", { class: "kind", text: "who gets room next" }), el("h3", { text: "Priority" }),
    p ? null : el("div", { class: "note", text: store.prioMissing ? "This controller has no priority API: the line below is from /api/state, and the rules are in pumasi.yaml (`pumasi priority` shows them)." : "loading…" }),
    exp.length ? section("Expedited", el("ul", { class: "rows" }, exp.map((e) => el("li", {}, el("button", { type: "button", "data-job-id": e.githubJobId || null, onclick: () => actions.selectJobBy(e) },
      el("span", {}, el("span", { class: "lvl expedite", text: "expedite" }), " " + e.name), el("span", { text: `${e.class}${e.ahead ? " · " + e.ahead + " ahead" : ""}` })))))) : null,
    section(`Waiting order (${lineRows.length})`, lineRows.length ? el("ol", { class: "rows", style: "list-style:none;padding:0;margin:0;display:grid;gap:4px" }, lineRows.map((r) => el("li", {},
      el("button", { type: "button", "data-job-id": r.githubJobId || null, "data-class": r.class, style: "width:100%;text-align:left;border:1px solid var(--line);background:var(--code);border-radius:6px;padding:5px 8px", onclick: () => actions.selectJobBy(r) },
        el("div", { style: "display:flex;gap:6px;align-items:baseline" }, el("b", { text: "#" + (r.position || "-") }), r.level && r.level !== "normal" ? el("span", { class: "lvl " + r.level, text: r.level === "expedite" ? (/^expedited/.test(r.why || "") ? "expedited" : "with expedited") : r.level }) : null, el("span", { text: r.name || "-" }), el("span", { class: "note", style: "margin-left:auto", text: `${r.class} · ${r.waited || ""}` })),
        el("div", { class: "note", text: [r.why, whyRow(r)].filter(Boolean).join(" · ") }))))) : el("div", { class: "note", text: "nothing waits" })),
    section("Mode", el("div", { class: "act" }, el("button", { type: "button", class: "secondary", onclick: () => actions.select({ kind: "mode" }) }, "Mode: " + ((p && p.mode && p.mode.name) || "normal") + " · change"))),
    section("Rules" + (p && p.headStart ? ` · high counts as ${p.headStart} earlier, low ${p.headStart} later` : ""),
      rules.length ? el("ul", { class: "rows" }, rules.map((r) => el("li", { "data-rule": r.id }, el("div", { style: "display:flex;gap:6px;align-items:center;border:1px solid var(--line);border-radius:6px;padding:4px 8px;background:var(--code)" },
        el("span", { class: "lvl " + r.level, text: r.level }), el("span", { class: "mono", style: "font-size:12px", text: Object.entries(r.match || {}).map(([k, v]) => `${k}: ${v}`).join(", ") }),
        el("span", { class: "note", style: "margin-left:auto", text: r.source === "file" ? "pumasi.yaml 🔒" : r.expires ? "until " + clock(r.expires) : "runtime" }),
        r.source === "file" ? null : el("button", { type: "button", class: "copy", "data-action": "rule-remove", onclick: async () => {
          const res = await write("DELETE", "api/priority/rules/" + encodeURIComponent(r.id));
          if (res.ok) actions.refresh(); else if (res.missing) fallback(box, `pumasi priority rm ${r.id}`, "No rules API here. The same from a shell:"); else box.replaceChildren(el("div", { class: "err", text: res.error }));
        } }, "remove"))))) : el("div", { class: "note", text: "no rules: jobs get room oldest first" }),
      ruleForm(box, actions), box),
    tokenField());
}

function ruleForm(box, actions) {
  const classes = classesOf(store.state).map((c) => c.name);
  const f = el("form", { class: "rule", "aria-label": "add a priority rule" },
    el("label", { text: "class", for: "rf-class" }), el("select", { id: "rf-class", name: "class" }, el("option", { value: "", text: "any" }), classes.map((c) => el("option", { value: c, text: c }))),
    el("label", { text: "workflow", for: "rf-wf" }), el("input", { id: "rf-wf", name: "workflow", placeholder: "deploy-*.yml" }),
    el("label", { text: "job", for: "rf-job" }), el("input", { id: "rf-job", name: "job", placeholder: "glob" }),
    el("label", { text: "branch", for: "rf-br" }), el("input", { id: "rf-br", name: "branch", placeholder: "release/*" }),
    el("label", { text: "event", for: "rf-ev" }), el("select", { id: "rf-ev", name: "event" }, ["", "push", "pull_request", "schedule", "workflow_dispatch", "merge_group"].map((e) => el("option", { value: e, text: e || "any" }))),
    el("label", { text: "level", for: "rf-lv" }), el("select", { id: "rf-lv", name: "level" }, ["high", "normal", "low"].map((e) => el("option", { value: e, text: e }))),
    el("label", { text: "for", for: "rf-for" }), el("select", { id: "rf-for", name: "for" }, ["2h", "30m", "1h", "4h", "8h", ""].map((e) => el("option", { value: e, text: e || "until removed" }))),
    el("div", { class: "act full" }, el("button", { type: "submit", class: "primary", "data-action": "rule-add" }, "Add rule")));
  f.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    const fd = new FormData(f);
    const match = {};
    for (const k of ["class", "workflow", "job", "branch", "event"]) if (fd.get(k)) match[k] = fd.get(k);
    if (!Object.keys(match).length) { box.replaceChildren(el("div", { class: "err", text: "a rule needs at least one thing to match" })); return; }
    const body = { match, level: fd.get("level") };
    if (fd.get("for")) body.for = fd.get("for");
    const r = await write("POST", "api/priority/rules", body);
    if (r.ok) { box.replaceChildren(el("div", { class: "note", text: "rule added" })); actions.refresh(); }
    else if (r.missing) {
      const yaml = "priority:\n  - {match: {" + Object.entries(match).map(([k, v]) => `${k}: ${/[*?[]/.test(v) ? JSON.stringify(v) : v}`).join(", ") + "}, level: " + body.level + "}";
      box.replaceChildren(el("div", { class: "note", text: "This controller has no rules API. Add it to pumasi.yaml: a running `pumasi up` reads the priority rules again within 10s." }), copyable(yaml, "pumasi.yaml", "the first rule that matches counts"));
    } else box.replaceChildren(el("div", { class: "err", text: r.error }));
  });
  return f;
}

// ---------------------------------------------------------------- list view
let showAll = false, openJob = "";
export function renderList(actions) {
  const s = store.state;
  if (!s) return;
  const fs = store.findings;
  $("l-findings").replaceChildren(...(fs.length ? fs.map(findingEl) : [el("p", { class: "empty", text: "nothing needs attention" })]));
  const fill = (id, rows, empty) => {
    $(id).tBodies[0].replaceChildren(...rows);
    if (empty) { $(empty).hidden = rows.length > 0; $(id).hidden = rows.length === 0; }
  };
  const td = (text, cls = "") => el("td", { class: cls, text: text === undefined || text === null ? "-" : String(text) });
  // one table: each job once, its class's reason said by the class's next job, the rest behind it
  const jobs = waitRows(s);
  fill("l-jobs", jobs.map((j) => el("tr", { "data-job-id": j.githubJobId || null, "data-run-id": j.runId, "data-class": j.class },
    td(j.position || "-", "num"), td(j.job), td(j.class), el("td", {}, j.expedited || (j.level && j.level !== "normal") ? el("span", { class: "lvl " + (j.expedited ? "expedite" : j.level), text: j.expedited ? "expedited" : j.level === "expedite" ? "with expedited" : j.level }) : null),
    td(ago(j.waitedSeconds), "num"), el("td", { class: "wrap" }, whyText(j), j.expect && !j._go ? el("div", { class: "eta", text: expectText(j.expect, store.state.runners, j.job) }) : null), td(j.workflow, "opt muted"))), "l-jobs-empty");
  const mbar = (parts) => el("span", { class: "mbar" }, parts.map(([v, c, tot]) => el("i", { style: `width:${tot ? (100 * v) / tot : 0}%;background:${c}` })));
  fill("l-hosts", s.hosts.map((h) => {
    const L = hostLoad(h, s.runners), M = L.mem, C = L.cpu;
    const seg = (p) => mbar([[p.held + p.kept, ZC.held, p.lend], [p.give, ZC.give, p.lend], [p.other, ZC.others, p.lend], [p.free, ZC.free, p.lend]]);
    return el("tr", { "data-host": h.name }, td(h.name), td(L.word + (L.why ? ": " + L.why : ""), L.tone === "ok" ? "muted" : L.tone === "ready" ? "ready" : L.tone === "bad" ? "problem" : "waiting"),
      td(C && M ? `${cpus(C.free)} · ${gb(M.free)}` : "-", "num"),
      el("td", {}, M ? [seg(M), `${short((M.held + M.kept) / GiB)} held · ${short(M.used / GiB)} used` + (M.give ? ` · ${short(M.give / GiB)} gives way` : "") + ` · of ${gb(M.lend)}`] : "-"),
      el("td", {}, C ? [seg(C), `${short(C.held + C.kept)} held · ${short(C.used)} used` + (C.give ? ` · ${short(C.give)} gives way` : "") + ` · of ${short(C.lend)}` + (L.share > 1 ? ` (${h.cpus}×${L.share})` : "")] : "-"),
      td(h.runners + (L.ready ? ` (${L.ready} ready)` : ""), "num"), td(h.kind + " · " + (h.os || "-"), "opt muted"),
      td(h.cpuPressure === null || h.cpuPressure === undefined ? "-" : h.cpuPressure.toFixed(1) + "%", "num opt"), td(h.diskFreePct === null || h.diskFreePct === undefined ? "-" : h.diskFreePct.toFixed(0) + "%", "num opt"),
      td(h.seenSeconds === null || h.seenSeconds === undefined ? "-" : dur(h.seenSeconds) + " ago", "num opt"), td(h.lastDrop || "-", "opt muted"));
  }));
  fill("l-runners", s.runners.map((r) => el("tr", { "data-runner": r.name, "data-class": r.class, "data-host": r.host }, td(r.name), td(r.class), td(r.host), td(aheadOf(r) ? "ahead" + (r.state === "starting" ? " · starting" : "") : isReady(r) ? "ready" + (r.givesWay ? " · gives way" : "") : r.state, isReady(r) || aheadOf(r) ? "ready" : "muted"), td(r.job || (aheadOf(r) ? "for " + aheadOf(r).job : "-")),
    td(`${(r.cpuNow || 0).toFixed(1)} / ${(r.heldCpus || r.estCpus).toFixed(1)}`, "num"),
    el("td", {}, mbar([[r.memBytes || 0, r.memBytes > r.estMem ? "#f0b429" : "#40c8be", r.heldMem || r.estMem]]), `${r.memBytes ? gb(r.memBytes) : "-"} / ${gb(r.heldMem || r.estMem)}` + (aheadOf(r) ? " (the job it was started for)" : isReady(r) ? " (its class's largest recent job)" : r.state === "starting" ? "" : r.estRuns ? "" : r.watched ? " (first guess, watched: 3×)" : " (first guess)")),
    td(dur(r.ageSeconds), "num opt"))), "l-runners-empty");
  const back = (c) => (s.takenBack || []).filter((j) => j.class === c).length;
  fill("l-overflow", (s.overflow || []).map((o) => el("tr", { "data-class": o.class }, td(o.class), td(o.take, "num"), td(o.held, "num"), td(o.after || "-"), td(back(o.class), "num"))), "l-overflow-empty");
  const done = store.jobs.filter((j) => j.job || j.startedAt);
  const rows = [];
  for (const j of showAll ? done : done.slice(0, 15)) {
    const res = j.oomKills ? "out of memory" : j.result || j.state;
    const tr = el("tr", { class: "job", "data-runner": j.runner, "data-class": j.class, "data-host": j.host }, td(j.finishedAt ? clock(j.finishedAt) : clock(j.createdAt), "muted"), td(j.job || "(name unknown)"),
      td(res, j.result === "succeeded" ? "muted" : "problem"), td(j.class, "opt"), td(j.host), td(j.startedAt && j.job ? dur(j.waitSec) : "-", "num opt"), td(j.finishedAt && j.startedAt ? dur(j.runSec) : "-", "num"),
      td(j.memWorkPeak || j.memPeak ? gb(j.memWorkPeak || j.memPeak) + (j.estMem ? " / expected " + gb(j.estMem) : "") : "-", "num"));
    tr.addEventListener("click", () => { openJob = openJob === j.runner ? "" : j.runner; renderList(actions); });
    rows.push(tr);
    if (openJob === j.runner) {
      const box = el("div", {});
      rows.push(el("tr", { class: "detail" }, el("td", { colSpan: 8 }, box)));
      loadHistory(j.runner, box);
    }
  }
  fill("l-recent", rows);
  const more = $("recent-more");
  more.hidden = done.length <= 15;
  more.textContent = showAll ? "show fewer" : "show all " + done.length;
  more.onclick = () => { showAll = !showAll; renderList(actions); };
  // a job measured on two platforms (a linux and a mac class) has a size for each: say which
  const seen = {};
  for (const j of s.sizes || []) { const k = j.repo + "/" + j.workflow + "/" + j.job; seen[k] = (seen[k] || 0) + 1; }
  const named = (j) => (seen[j.repo + "/" + j.workflow + "/" + j.job] > 1 && j.platform ? `${j.job} (${j.platform})` : j.job);
  fill("l-sizes", (s.sizes || []).map((j) => el("tr", {}, td(named(j)), td(j.workflow, "muted opt"), td(sz(j.estCpus, j.estMem) + (j.over ? ` (its limit; needs ${gb(j.over)})` : ""), "num"), td(sz(j.lastCpus, j.lastMem), "num"), td(sz(j.topCpus, j.topMem), "num opt"), td(j.runs, "num"))));
  $("l-incidents").replaceChildren(...[...s.incidents.map((i) => [i, true]), ...(s.recentIncidents || []).slice().reverse().map((i) => [i, false])].map(([i, open]) =>
    el("li", { class: open ? "open" : "", "data-kind": i.kind }, el("b", { text: i.kind + " " }), i.detail, el("span", { class: "muted", text: open ? `  open ${dur(since(i.openedAt))}` : `  ${clock(i.openedAt)}${i.resolvedAt ? " – " + clock(i.resolvedAt) : ""}` }))));
  if (!s.incidents.length && !(s.recentIncidents || []).length) $("l-incidents").replaceChildren(el("li", { class: "muted", text: "none" }));
  $("l-explain").textContent = store.explainText || "";
  renderCIList();
  if (store.series) renderCharts(store.series, s);
}

// One small line chart, shared y from zero.
function chart(title, t, lines, unit, limit) {
  const W = 360, H = 120, L = 30, B = 16, T = 6;
  let max = limit || 0;
  for (const l of lines) for (const v of l.values) if (v > max) max = v;
  if (max <= 0) max = 1;
  const x = (i) => L + ((W - L) * i) / Math.max(1, t.length - 1);
  const y = (v) => T + (H - T - B) * (1 - v / max);
  const ns = "http://www.w3.org/2000/svg";
  const s = (tag, attrs) => { const e = document.createElementNS(ns, tag); for (const k in attrs) e.setAttribute(k, attrs[k]); return e; };
  const svg = s("svg", { viewBox: `0 0 ${W} ${H}`, preserveAspectRatio: "none", role: "img", "aria-label": title });
  const ink = getComputedStyle(document.documentElement).getPropertyValue("--muted").trim() || "#888";
  svg.append(s("line", { x1: L, x2: W, y1: y(0), y2: y(0), stroke: ink, "stroke-width": 1 }));
  const label = (txt, xx, yy, anchor) => { const e = s("text", { x: xx, y: yy, "text-anchor": anchor, "font-size": 10, fill: ink }); e.textContent = txt; return e; };
  svg.append(label(unit(max), L - 4, y(max) + 3, "end"), label("0", L - 4, y(0) + 3, "end"), label("24h ago", L, H - 3, "start"), label("now", W, H - 3, "end"));
  const legend = el("div", { class: "legend" });
  const colors = ["#e0a42e", "#2fb3a8", "#8a6be0", "#e0644f", "#4aa3e8", "#8cc43f"];
  lines.forEach((l, k) => {
    let d = "", pen = false;
    l.values.forEach((v, i) => {
      if (v < 0) { pen = false; return; }
      d += (pen ? "L" : "M") + x(i).toFixed(1) + " " + y(v).toFixed(1);
      pen = true;
    });
    const stroke = l.color || colors[k % colors.length];
    svg.append(s("path", { d, fill: "none", stroke, "stroke-width": 1.5, "vector-effect": "non-scaling-stroke" }));
    legend.append(el("span", {}, el("i", { style: "background:" + stroke }), l.name));
  });
  return el("div", { class: "chart" }, el("h3", { text: title }), svg, legend);
}
function renderCharts(se, state) {
  const hosts = Object.keys(se.hostCpus || {}).sort();
  $("charts").replaceChildren(
    chart("CPUs held by runners, per host", se.t, hosts.map((h) => ({ name: h, values: se.hostCpus[h] })), (v) => String(Math.round(v))),
    chart("Jobs waiting", se.t, [{ name: "waiting", values: se.waiting, color: "#f0b429" }], (v) => String(Math.round(v))),
    chart("Wait before a runner took the job (median)", se.t, [{ name: "median wait", values: se.waitMedianSec, color: "#f0b429" }], dur));
}

// ---------------------------------------------------------------- timeline strip
export function renderTimeline(onSelect) {
  const se = store.series, s = store.state;
  const svg = $("tl-svg");
  if (!se || !s) return;
  const ns = "http://www.w3.org/2000/svg";
  const mk = (tag, a) => { const e = document.createElementNS(ns, tag); for (const k in a) e.setAttribute(k, a[k]); return e; };
  const W = 480, H = 64, n = se.waiting.length;
  const max = Math.max(1, ...se.waiting);
  let d = `M0 ${H}`;
  se.waiting.forEach((v, i) => { d += ` L${((i / (n - 1)) * W).toFixed(1)} ${(H - 6 - (v / max) * (H - 22)).toFixed(1)}`; });
  d += ` L${W} ${H} Z`;
  const kids = [mk("path", { d, fill: "rgba(240,180,41,0.35)", stroke: "#f0b429", "stroke-width": 1, "vector-effect": "non-scaling-stroke" })];
  const t0 = se.t[0] * 1000, t1 = se.t[n - 1] * 1000;
  const all = [...(s.recentIncidents || []).map((i) => [i, false]), ...s.incidents.map((i) => [i, true])];
  for (const [i, open] of all) {
    const at = new Date(i.openedAt).getTime();
    if (at < t0) continue;
    const x = Math.min(W - 2, ((at - t0) / (t1 - t0)) * W);
    const col = open ? "#ff5252" : /restart|recovered/.test(i.kind) ? "#8f9cab" : i.kind === "job-waiting" ? "#f0b429" : "#ff8a65";
    const g = mk("g", { style: "cursor:pointer" });
    const tt = mk("title", {}); tt.textContent = `${clock(i.openedAt)} ${i.kind}: ${i.detail}`;
    g.append(tt, mk("rect", { x: x - 1.2, y: 2, width: 2.4, height: 12, fill: col }));
    g.addEventListener("click", () => { const sub = subjectOf(i); if (sub) onSelect(sub); });
    kids.push(g);
  }
  svg.replaceChildren(...kids);
  $("tl-legend").textContent = `waiting peak ${max} · ${all.length} ${all.length === 1 ? "problem" : "problems"} in 24h`;
}

export function atParam(sep) {
  const at = window.__pumasiAt ? window.__pumasiAt() : null;
  return at === null ? "" : sep + "at=" + at.toFixed(0);
}
export { osBadge };

// ---------------------------------------------------------------- CI watch
// From /api/trend and /api/forecast, and the ci-* problems: what got slower
// or faster and since which commit, what grows slowly, the limits crossed,
// and when CI's growth fills the cores. A summary line; the rest on a click.
const ciOpen = { v: false };
try { ciOpen.v = localStorage.getItem("pumasi.ci") === "open"; } catch (e) { /* closed */ }
export function ciModel() {
  const t = store.trend, f = store.forecast, s = store.state;
  if (!t && !f) return null;
  // the same job and commit: one row, time first, then memory; the rest said along
  const groups = new Map();
  const rank = (c) => ({ time: 0, memory: 1, cpu: 2 })[c.metric] ?? 3;
  for (const c of (t && t.changes) || []) {
    if (c.holds === false) continue;
    const k = c.repo + "|" + c.workflow + "|" + c.job + "|" + ((c.first && c.first.sha) || c.pr || "");
    if (!groups.has(k)) groups.set(k, []);
    groups.get(k).push(c);
  }
  const rows = [...groups.values()].map((g) => { g.sort((a, b) => rank(a) - rank(b)); return { c: g[0], also: g.slice(1) }; });
  const items = rows.map(({ c, also }) => {
    const up = c.ratio > 0;
    const kind = c.metric === "memory" ? (up ? "bigger" : "smaller") : up ? "slower" : "faster";
    const fmt = c.metric === "memory" ? (v) => gb(v) : c.metric === "cpu" ? (v) => Math.round(v) + " cpu·s" : (v) => ago(v);
    const repo = "https://github.com/" + c.repo;
    const sha = (c.first && c.first.sha) || "";
    // a pull request it is known to come from; else the one merged in between, likely
    const pr = c.pr || (c.first && c.first.pr) || 0;
    const likely = !pr && (c.suspects || []).length === 1 ? c.suspects[0] : 0;
    const step = (c.steps || []).find((x) => x.after > x.before) || (c.steps || [])[0];
    return { kind, job: c.job, metric: c.metric, ratio: c.ratio, from: fmt(c.before), to: fmt(c.after), pr, likely, likelyURL: likely ? `${repo}/pull/${likely}` : "", sha, prURL: pr ? `${repo}/pull/${pr}` : "", shaURL: sha ? `${repo}/commit/${sha}` : "",
      step: step && c.metric === "time" ? `${step.name} ${ago(step.before)} → ${ago(step.after)}` : "", also: also.map((a) => `${a.metric} ${pctText(a.ratio)}`), at: c.first && c.first.at };
  });
  for (const g of (t && t.creeping) || []) items.push({ kind: "growing", job: g.job, metric: "time", ratio: g.ratio, from: ago(g.from), to: ago(g.to), days: g.days });
  const order = { slower: 0, bigger: 1, growing: 2, faster: 3, smaller: 4 };
  items.sort((a, b) => order[a.kind] - order[b.kind] || Math.abs(b.ratio) - Math.abs(a.ratio));
  const alarms = ((s && s.incidents) || []).filter((i) => /^ci-/.test(i.kind));
  let fc = "";
  if (f && f.fullIn > 0 && isFinite(f.fullIn)) fc = `at this rate the cores fill in ~${Math.round(f.fullIn)} weeks` + (f.fullSoon && f.fullLate ? ` (${Math.round(f.fullSoon)}–${Math.round(f.fullLate)})` : "");
  else if (f && f.growth <= 0) fc = "CI is not growing: the cores do not fill at this rate";
  const growth = f && f.fitted ? `CI ${f.growth >= 0 ? "+" : ""}${Math.round(f.growth * 100)}% a week` + (f.cores ? ` · busiest hours use ${Math.round(f.load * f.cores)} of ${Math.round(f.cores)} cores` : "") : "";
  return { items, alarms, fc, growth };
}
const pctText = (r) => (r >= 0 ? "+" : "−") + Math.round(Math.abs(r) * 100) + "%";
const KW = { slower: "slower", faster: "faster", bigger: "more memory", smaller: "less memory", growing: "growing" };
function ciItem(it) {
  const head = el("div", { class: "ci-l1" }, el("span", { class: "ci-tag " + it.kind, text: KW[it.kind] }), el("b", { text: it.job }),
    el("span", { class: "ci-n " + it.kind, text: `${it.metric === "memory" ? "memory" : "time"} ${pctText(it.ratio)}` }), el("span", { class: "muted", text: `${it.from} → ${it.to}` }));
  const since = it.kind === "growing" ? el("div", { class: "ci-l2 muted", text: `slowly, over ${it.days} days, no one commit` })
    : el("div", { class: "ci-l2" }, "since ", it.pr ? el("a", { href: it.prURL, target: "_blank", rel: "noopener", text: "PR #" + it.pr }) : null, it.pr && it.sha ? " · " : "",
      it.sha ? el("a", { href: it.shaURL, target: "_blank", rel: "noopener", class: "mono", text: it.sha.slice(0, 7) }) : null,
      it.likely ? el("span", {}, " (likely ", el("a", { href: it.likelyURL, target: "_blank", rel: "noopener", text: "PR #" + it.likely }), ")") : null,
      it.at ? el("span", { class: "muted", text: " · " + new Date(it.at).toLocaleDateString([], { month: "short", day: "numeric" }) }) : null,
      it.step ? el("span", { class: "muted", text: " · step " + it.step }) : null, it.also.length ? el("span", { class: "muted", text: " · " + it.also.join(", ") }) : null);
  return el("li", { "data-ci": it.kind, "data-job": it.job }, head, since);
}
function ciBody(m, max) {
  return [
    m.fc || m.growth ? el("div", { class: "ci-fc" }, m.fc ? el("b", { text: m.fc[0].toUpperCase() + m.fc.slice(1) }) : null, m.growth ? el("span", { class: "muted", text: (m.fc ? " · " : "") + m.growth }) : null) : null,
    el("h4", { text: "Changes" }), m.items.length ? el("ul", { class: "ci-list" }, m.items.slice(0, max).map(ciItem)) : el("div", { class: "note", text: "no job changed in the last two weeks" }),
    m.items.length > max ? el("div", { class: "note", text: `+${m.items.length - max} more: pumasi trend` }) : null,
    el("h4", { text: "Limits" }), m.alarms.length ? el("ul", { class: "ci-list" }, m.alarms.map((a) => el("li", { "data-kind": a.kind }, el("div", { class: "ci-l1" }, el("span", { class: "ci-tag slower", text: kindWord(a.kind).replace(/^ci /, "") }), el("span", { text: a.detail })))))
      : el("div", { class: "note", text: "no limit crossed (job time, memory, wait, deadline and cpu-hours, set under watch: in pumasi.yaml)" }),
  ];
}
export function renderCI() {
  const box = $("ci");
  const m = ciModel();
  if (!m) { box.hidden = true; return; }
  box.hidden = false;
  const top = m.items.find((x) => x.kind === "slower" || x.kind === "bigger") || m.items[0];
  const bits = [];
  if (m.alarms.length) bits.push(el("span", { class: "problem", text: `${m.alarms.length} ${m.alarms.length === 1 ? "limit" : "limits"} crossed` }));
  if (top) bits.push(el("span", {}, el("b", { text: top.job }), ` ${top.metric === "memory" ? "memory" : ""} ${pctText(top.ratio)}` + (top.pr ? ` since #${top.pr}` : top.sha ? ` since ${top.sha.slice(0, 7)}` : "")));
  if (m.fc) bits.push(el("span", { class: "muted", text: m.fc.replace("at this rate the cores fill in ", "cores full in ") }));
  $("ci-head").replaceChildren(...bits.flatMap((b, i) => (i ? [" · ", b] : [b])));
  $("ci-toggle").setAttribute("aria-expanded", String(ciOpen.v));
  $("ci-toggle").onclick = () => { ciOpen.v = !ciOpen.v; try { localStorage.setItem("pumasi.ci", ciOpen.v ? "open" : "closed"); } catch (e) { /* this page only */ } renderCI(); };
  box.classList.toggle("open", ciOpen.v);
  $("ci-body").hidden = !ciOpen.v;
  if (ciOpen.v) $("ci-body").replaceChildren(...ciBody(m, 6).filter(Boolean));
}
export function renderCIList() {
  const m = ciModel();
  $("l-ci").hidden = !m;
  if (m) $("l-ci-body").replaceChildren(...ciBody(m, 12).filter(Boolean));
}
