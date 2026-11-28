// Small helpers shared by the scene, the HUD and the list view.

export const GiB = 2 ** 30;
export const gib = (b) => (b / GiB).toFixed(1) + "G";
export const mib = (b) => Math.round(b / 2 ** 20) + "M";
export const cpu = (c) => (c || 0).toFixed(1) + "cpu";
export const size = (c, b) => cpu(c) + "/" + gib(b || 0);

export function dur(sec) {
  sec = Math.max(0, Math.round(sec || 0));
  if (sec < 60) return sec + "s";
  if (sec < 3600) return Math.floor(sec / 60) + "m" + String(sec % 60).padStart(2, "0") + "s";
  if (sec < 86400) return Math.floor(sec / 3600) + "h" + String(Math.floor((sec % 3600) / 60)).padStart(2, "0") + "m";
  return Math.floor(sec / 86400) + "d" + String(Math.floor((sec % 86400) / 3600)).padStart(2, "0") + "h";
}
export const clock = (iso) => new Date(iso).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false });
export const since = (iso, now = Date.now()) => (now - new Date(iso).getTime()) / 1000;

export const clamp = (v, a, b) => (v < a ? a : v > b ? b : v);
export const lerp = (a, b, t) => a + (b - a) * t;
export const ease = (t) => (t < 0.5 ? 4 * t * t * t : 1 - (-2 * t + 2) ** 3 / 2);
export const easeOut = (t) => 1 - (1 - t) ** 3;

// hash: a stable 32-bit number for a string (layout jitter, per-entity phase).
export function hash(s) {
  let h = 2166136261;
  for (let i = 0; i < s.length; i++) {
    h ^= s.charCodeAt(i);
    h = Math.imul(h, 16777619);
  }
  return h >>> 0;
}
export function rng(seed) {
  let a = seed >>> 0 || 1;
  return () => {
    a |= 0;
    a = (a + 0x6d2b79f5) | 0;
    let t = Math.imul(a ^ (a >>> 15), 1 | a);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

// Colours as [r,g,b]; shade by a factor (>1 lighter), mix two, write as css.
export function hex(h) {
  const n = parseInt(h.slice(1), 16);
  return [(n >> 16) & 255, (n >> 8) & 255, n & 255];
}
export const rgb = (c, a = 1) => (a >= 1 ? `rgb(${c[0] | 0},${c[1] | 0},${c[2] | 0})` : `rgba(${c[0] | 0},${c[1] | 0},${c[2] | 0},${a})`);
export function shade(c, f) {
  if (f >= 1) return c.map((v) => v + (255 - v) * (f - 1));
  return c.map((v) => v * f);
}
export const mix = (a, b, t) => [lerp(a[0], b[0], t), lerp(a[1], b[1], t), lerp(a[2], b[2], t)];

// The class colours: chosen apart in hue and lightness so neighbouring lines differ.
// Muted on purpose: a class only says which belt a thing belongs to, while the
// status colours (ok green, waiting amber, failed red, offline grey) must win.
export const CLASS_COLORS = ["#2fb3a8", "#e0a42e", "#8a6be0", "#e0644f", "#4aa3e8", "#8cc43f", "#d9609c", "#c9a86a", "#5fc4e0", "#b8b04a"].map(hex).map((c) => mix(c, [128, 134, 144], 0.38));

export function el(tag, props = {}, ...kids) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(props)) {
    if (v === undefined || v === null || v === false) continue;
    if (k === "class") e.className = v;
    else if (k === "text") e.textContent = v;
    else if (k === "style") e.style.cssText = v;
    else if (k.startsWith("on")) e.addEventListener(k.slice(2), v);
    else if (k in e && !k.startsWith("data") && !k.startsWith("aria")) e[k] = v;
    else e.setAttribute(k, v === true ? "" : v);
  }
  for (const k of kids.flat(Infinity)) if (k !== null && k !== undefined && k !== false) e.append(k);
  return e;
}

// Reason strings from the placement ("no host with 7.5cpu/30.8G free (h1 20.3cpu/1.4G, h2 in use)").
// A job larger than any host of its class can lend has "; its size … at most): it runs alone …" after it.
const NEEDS = /^no host with ([\d.]+)cpu\/([\d.]+)G free \((.*)\)$/;
export function parseReason(reason) {
  let r = reason || "";
  const i = r.indexOf("); its size ");
  if (i >= 0) r = r.slice(0, i + 1);
  const m = NEEDS.exec(r);
  if (!m) return null;
  const needCpu = +m[1], needMem = +m[2];
  const hosts = [];
  for (const part of splitTop(m[3])) {
    const h = /^(\S+) ([\d.]+)cpu\/([\d.]+)G(?::\s*(.*))?$/.exec(part);
    if (h) hosts.push({ name: h[1], cpu: +h[2], mem: +h[3], note: h[4] || "", short: { cpu: Math.max(0, needCpu - h[2]), mem: Math.max(0, needMem - h[3]) } });
    else hosts.push({ name: part.split(" ")[0], note: part.slice(part.indexOf(" ") + 1), closed: true });
  }
  return { needCpu, needMem, hosts };
}
// splitTop splits at commas outside parentheses.
function splitTop(s) {
  const out = [];
  let depth = 0, cur = "";
  for (const ch of s) {
    if (ch === "(") depth++;
    if (ch === ")") depth--;
    if (ch === "," && depth === 0) {
      out.push(cur.trim());
      cur = "";
    } else cur += ch;
  }
  if (cur.trim()) out.push(cur.trim());
  return out;
}


export const osBadge = (os) => ({ macos: "MAC", windows: "WIN", linux: "LNX" })[os] || "LNX";

// ---------------------------------------------------------------- plain words
// The same facts in the words a newcomer reads without a glossary. The API's
// field names stay as they are; only what is shown changes.
const num = (v) => (v >= 9.95 ? String(Math.round(v)) : (+v).toFixed(1));
export const gbn = (g) => num(+g || 0) + " GB";
export const gb = (b) => gbn((b || 0) / GiB);
export const cpus = (c) => { const v = +(c || 0); return num(v) + (Math.abs(v - 1) < 0.05 ? " CPU" : " CPUs"); };
export const ago = (sec) => dur(sec).replace(/^(\d+m)00s$/, "$1").replace(/^(\d+h)00m$/, "$1");

// ready: a runner up and listening with no job yet (kept warm for its
// class's next job, or started ahead of a job that needs another to end).
export const isReady = (r) => !r.job && r.state === "idle";
// overflowWords: an overflow class's take (jobs it takes from GitHub now:
// those it holds that have room, and room for more) and what it holds.
export const overflowWords = (ov) => `takes ${ov.take ? "up to " + ov.take + (ov.take === 1 ? " job" : " jobs") : "no jobs"} now, holds ${ov.held}`;
// ahead: a runner with no job started for one expected to come, the job it
// needs about to end (state's runner.ahead: "X expected in ~Ns, when Y
// ends"). Its parts, or null.
export function aheadOf(r) {
  if (!r || r.job || !r.ahead) return null;
  const m = /^(.*) expected in ~(\d+)s, when (.*) ends$/.exec(r.ahead);
  return m ? { job: m[1], in: +m[2], after: m[3] } : { job: r.ahead, after: "" };
}

// hostLoad: how full a host is, as a waiting job sees it. Of what the host
// lends runners (its budget, else the machine; cores counted cpuShare times
// where runners share them), each part is one of:
//   held   — what its busy runners hold (their size, or more when they use more)
//   give   — what its ready runners hold that they give up for a waiting job
//   kept   — what ready runners hold that they keep (warmMin), and starting ones
//   other  — other programs, and memory kept free for the machine
//   free   — what a new runner could take now
// used is measured: what the runners use now, inside what they hold.
// The word counts give as room: a host full of ready runners is not FULL.
export function hostLoad(h, runners = []) {
  const share = h.cpuShare > 1 ? h.cpuShare : 1;
  const lendMem = h.budgetMem || h.memBytes || 0;
  const lendCpu = h.budgetCpus ? h.budgetCpus : (h.cpus || 0) * share;
  const mine = runners.filter((r) => r.host === h.name);
  const ready = mine.filter(isReady);
  const sum = (rs, f) => rs.reduce((a, r) => a + (f(r) || 0), 0);
  const giving = ready.filter((r) => r.givesWay);
  const part = (k) => {
    const heldOf = (r) => (k === "mem" ? r.heldMem || r.estMem : r.heldCpus || r.estCpus);
    const lend = k === "mem" ? lendMem : lendCpu;
    if (!lend) return null;
    const free = (k === "mem" ? h.freeMem : h.freeCpus) || 0;
    const busy = mine.filter((r) => r.job);
    const held = sum(busy, heldOf), give = sum(giving, heldOf), kept = sum(mine.filter((r) => !r.job && !r.givesWay), heldOf);
    const used = sum(mine, (r) => (k === "mem" ? r.memBytes : r.cpuNow));
    let other = Math.max(0, lend - held - give - kept - free);
    const tot = held + give + kept + other + free;
    const f = tot > lend ? lend / tot : 1; // held over what is lent: fit the bar
    const room = free + give;
    const taken = clamp(1 - room / lend, 0, 1), rawTaken = clamp(1 - free / lend, 0, 1);
    // jobless: how taken it would be without the runners that hold room and
    // run nothing yet (ready ones that keep it, and those starting)
    const jobless = clamp(1 - (room + kept) / lend, 0, 1);
    return { lend, held, give, kept, other, free, used, room, f, taken, rawTaken, jobless, pct: Math.round(taken * 100),
      // the bar, as shares of what is lent
      seg: { held: (held * f) / lend, give: (give * f) / lend, kept: (kept * f) / lend, other: (other * f) / lend, used: clamp(used / lend, 0, 1) } };
  };
  const mem = part("mem"), cpu = part("cpu");
  let word, tone, why = "";
  const top = (k) => Math.max(mem ? mem[k] : 0, cpu ? cpu[k] : 0);
  if (h.reachable === null || h.reachable === undefined) { word = "no data"; tone = "off"; }
  else if (!h.reachable) { word = "offline"; tone = "bad"; why = h.seenSeconds ? "no signal for " + ago(h.seenSeconds) : "no signal"; }
  else if (h.closed) { word = "closed"; tone = "warn"; why = h.closed; }
  else {
    // FULL: 90% or more taken even counting what ready runners give way.
    // N READY: it would be FULL by its free room alone, and ready runners
    // give way the rest: a waiting job of another class still gets in. N is
    // all its ready runners, as the header and the hosts list count them.
    const t = top("taken");
    if (t >= 0.9) { word = "full"; tone = "bad"; }
    else if (top("rawTaken") >= 0.9 && giving.length) { word = ready.length + " ready"; tone = "ready"; }
    else if (t >= 0.7) { word = "busy"; tone = "warn"; }
    else if (!h.runners) { word = "idle"; tone = "ok"; }
    else { word = "ok"; tone = "ok"; }
    // HELD: as full as that only by runners that run no job yet (ready ones
    // keeping their room, starting ones): nothing runs there, it is not busy
    if ((word === "full" || word === "busy") && !mine.some((r) => r.job) && top("jobless") < 0.7) { word = "held"; tone = "warn"; }
  }
  return { mem, cpu, word, tone, why, lendMem, lendCpu, share, ready: ready.length, giving: giving.length };
}

// readyLine: a host's ready runners, as many as the header counts there, and
// the part of them that gives way to another class's waiting job.
export function readyLine(L) {
  if (!L.ready) return "";
  const give = `${gb(L.mem ? L.mem.give : 0)}${L.cpu ? " · " + cpus(L.cpu.give) : ""}`;
  if (L.giving === L.ready) return `${L.ready} ready ${L.ready === 1 ? "runner gives" : "runners give"} way: ${give}`;
  if (L.giving) return `${L.ready} ready; ${L.giving} ${L.giving === 1 ? "gives" : "give"} way: ${give}`;
  return `${L.ready} ready ${L.ready === 1 ? "runner keeps its" : "runners keep their"} room`;
}
// expressRow: the line's row of the job expedited by hand (the first of them
// still waiting), not one of those ahead of it it pulls along, which are
// "expedite" in the line too.
export function expressRow(p) {
  const own = (p.expedited || []).filter((x) => x.position > 0);
  const is = (x, e) => (e.job && x.job === e.job) || (e.githubJobId && x.githubJobId === e.githubJobId);
  for (const e of own) {
    const row = (p.line || []).find((x) => x.level === "expedite" && is(x, e));
    if (row) return row;
  }
  return null;
}
// shown: a CI value as the panel writes it (memory to a tenth of a GB, time
// to the second, to the minute past an hour); shownRatio: after against
// before as the two read, so that the percent beside them is what they make.
// The CLI does the same (watch.Shown).
export function shown(metric, v) {
  if (metric === "memory") return Math.round((Math.max(0, v || 0) / GiB) * 10) / 10;
  const n = Math.round(v || 0);
  return n >= 3600 ? Math.floor(n / 60) * 60 : n;
}
export const shownRatio = (metric, before, after) => (shown(metric, before) > 0 ? shown(metric, after) / shown(metric, before) - 1 : NaN);
// stepOf: the step that moved most the way the job did (slower: the one that
// grew most; faster: the one that shrank most), or none.
export function stepOf(c) {
  const way = c.ratio >= 0 ? 1 : -1;
  const moved = (c.steps || []).map((x) => ({ x, d: way * (x.after - x.before) })).filter((m) => m.d > 0).sort((a, b) => b.d - a.d);
  return moved.length ? moved[0].x : null;
}
// plainReason: why a class's next job waits, in one sentence. Placement says
// "no host with 3.8cpu/19.2G free (h1 0.8cpu/2.8G, h2 down)"; this says which
// of the two is short and the most there is anywhere.
// The free room the reason quotes is from the last placement round; the cards
// show the host now. Hosts it names that are up are read from the latest state
// (setLive), so the sentence and the card never disagree.
let live = new Map();
// With runners, each host also knows the memory its ready runners give way.
export const setLive = (hosts, runners = []) => {
  live = new Map((hosts || []).filter((h) => h.reachable && !h.closed && h.memBytes).map((h) => [h.name, { h, give: ((hostLoad(h, runners).mem || {}).give || 0) / GiB }]));
};
export function plainReason(reason) {
  const r = reason || "";
  if (!r) return "a runner is starting for it";
  if (/^(high-only mode|mode high-only)/.test(r)) return "held: high-only mode is on, only high and expedited jobs start";
  const p = parseReason(r);
  if (!p) return r.replace(/(\d+(?:\.\d+)?)G\b/g, (_, g) => gbn(+g)).replace(/(\d+(?:\.\d+)?)cpu\b/g, (_, c) => cpus(+c));
  const open = p.hosts.filter((h) => !h.closed);
  for (const h of p.hosts) {
    const now = !h.closed && live.get(h.name);
    if (now) { h.cpu = now.h.freeCpus || 0; h.mem = (now.h.freeMem || 0) / GiB; h.give = now.give; }
  }
  const shut = p.hosts.filter((h) => h.closed).map((h) => `${h.name} ${plainNote(h.note)}`);
  // a host that is closed or reserved is often the whole story: always said
  const tail = shut.length ? `; ${shut.join("; ")}` : "";
  if (!open.length) return `needs ${cpus(p.needCpu)} and ${gbn(p.needMem)}; no host takes jobs now${tail}`;
  // the need is rounded to 0.1 by placement: a host within that of it may
  // still be short, and two numbers that read the same say "just short"
  const memOK = open.filter((h) => h.mem >= p.needMem + 0.05);
  if (!memOK.length) {
    const best = open.slice().sort((a, b) => b.mem - a.mem)[0];
    const [need, have] = apart(p.needMem, best.mem, gbn);
    // a host whose ready runners give way makes more than its free memory
    const most = open.slice().sort((a, b) => b.mem + (b.give || 0) - a.mem - (a.give || 0))[0];
    const more = most.give > 0.05 && most.mem + most.give > best.mem + 0.05 ? `; ${most.name} makes ${gbn(most.mem + most.give)} with its ready runners giving way` : "";
    if (need === have) return `needs ${need} memory, ${best.name} is just short of it${more}${tail}`;
    // one host takes the class: "the most free" would hint at others
    if (open.length === 1) return `needs ${need} memory, ${best.name} has ${have} free${more}${tail}`;
    return `needs ${need} memory, the most free${shut.length ? " elsewhere" : ""} is ${have} (${best.name})${more}${tail}`;
  }
  const best = memOK.slice().sort((a, b) => b.cpu - a.cpu)[0];
  if (best.cpu < p.needCpu + 0.05) {
    const [need, have] = apart(p.needCpu, best.cpu, cpus);
    if (need === have) return `needs ${need}, ${best.name} has the memory but is just short of the CPU${tail}`;
    return `needs ${need}, ${best.name} has the memory but only ${have} free${tail}`;
  }
  return `needs ${cpus(p.needCpu)} and ${gbn(p.needMem)}; room is opening on ${best.name}${tail}`;
}
// apart writes a need and what a host has so that they read differently when
// they differ: one more decimal, as far as two.
function apart(need, have, fmt) {
  let a = fmt(need), b = fmt(have);
  for (const d of [1, 2]) {
    if (a !== b) break;
    const unit = fmt(need).replace(/^[\d.]+/, "");
    const u = unit === " CPU" && Math.abs(have - 1) >= 0.05 ? " CPUs" : unit;
    a = need.toFixed(d) + unit; b = have.toFixed(d) + u;
  }
  return a === b ? [fmt(need), fmt(need)] : [a, b];
}
// expectText: when a waiting job is expected to start (state's expect), in a few words.
// The controller says the same (JobExpect.Text in explain.go). self is the
// waiting job's name: one of its name it waits for is "another". A job it
// waits for that is still waiting itself is said to be: "after the e2e waiting
// ahead of it". With the waiting jobs too, a running job whose name more than
// one job has in sight says which one: where it runs, and how long it has,
// when its host runs more than one. When that job's end alone does not make
// the room, it says that others ending there before it do too.
export function expectText(e, runners = [], self = "", waiting = []) {
  if (!e) return "";
  const t = `starts in ${inText(e.inSeconds)} on ${e.host}`;
  if (!e.after) return t;
  if (!e.afterRunner) return `${t}, after the ${e.after} waiting ahead of it`;
  const n = e.afterWith || 0;
  const with_ = n ? `, once ${n} ${n === 1 ? "other" : "others"} there ${n === 1 ? "has" : "have"} ended too` : "";
  const at = runners.find((r) => r.name === e.afterRunner);
  const same = runners.filter((r) => r.job === e.after).length + waiting.filter((j) => j.job === e.after).length;
  const where = at && at.host !== e.host ? ` on ${at.host}` : "";
  if (!at || same < 2) return `${t}, when ${e.after === self ? "another " + e.after : e.after}${where} ends${with_}`;
  const on = where || " there";
  const mins = (r) => Math.floor((r.ageSeconds || 0) / 60);
  const twins = runners.filter((r) => r.job === e.after && r.host === at.host);
  if (twins.length < 2) return `${t}, when the ${e.after} running${on} ends${with_}`;
  if (twins.filter((r) => mins(r) === mins(at)).length > 1) return `${t}, when one of the ${twins.length} ${e.after} running${on} ends${with_}`;
  return `${t}, when the ${e.after} running${on} for ${mins(at) < 1 ? "under a minute" : mins(at) + "m"} ends${with_}`;
}
const inText = (s = 0) => (s < 60 ? "under a minute" : s < 5400 ? "~" + Math.round(s / 60) + "m" : "~" + Math.floor(s / 3600) + "h" + String(Math.floor((s % 3600) / 60)).padStart(2, "0") + "m");
const plainNote = (n) => (/^(down|unreach)/.test(n) ? "offline" : n);

// One job is expedited; the older jobs of its class go first with it (GitHub gives a
// class its oldest job first) and say so, not that they were expedited.
export const levelWord = (j) => (j.expedited ? "expedited" : j.level === "expedite" ? "with expedited" : j.level === "high" ? "high" : j.level === "low" ? "low" : "");

// Problem kinds as headings.
export const kindWord = (k) => ({ "host-unreachable": "host offline", "job-waiting": "waiting too long", "out-of-memory": "out of memory", "runner-died": "runner died", "disk-low": "disk almost full", "agent-restarted": "agent restarted" })[k] || String(k || "").replace(/-/g, " ");
