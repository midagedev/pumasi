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

// hostLoad: how full a host is, of what it shares with runners (its budget,
// else the whole machine). Taken is all a new runner cannot have: what runners
// reserve, other programs, and what is kept for the machine itself.
export function hostLoad(h) {
  const lendMem = h.budgetMem || h.memBytes || 0, lendCpu = h.budgetCpus || h.cpus || 0;
  const part = (used, free, lend) => {
    if (!lend) return null;
    const taken = clamp((lend - (free || 0)) / lend, 0, 1);
    const mine = Math.min(taken, Math.max(0, (used || 0) / lend));
    return { pct: Math.round(taken * 100), taken, mine, other: taken - mine };
  };
  const mem = part(h.usedMem, h.freeMem, lendMem), cpu = part(h.usedCpus, h.freeCpus, lendCpu);
  let word, tone, why = "";
  if (h.reachable === null || h.reachable === undefined) { word = "no data"; tone = "off"; }
  else if (!h.reachable) { word = "offline"; tone = "bad"; why = h.seenSeconds ? "no signal for " + ago(h.seenSeconds) : "no signal"; }
  else if (h.closed) { word = "closed"; tone = "warn"; why = h.closed; }
  else {
    const top = Math.max(mem ? mem.pct : 0, cpu ? cpu.pct : 0);
    if (top >= 90) { word = "full"; tone = "bad"; }
    else if (top >= 70) { word = "busy"; tone = "warn"; }
    else if (!h.runners) { word = "idle"; tone = "ok"; }
    else { word = "ok"; tone = "ok"; }
  }
  return { mem, cpu, word, tone, why, lendMem, lendCpu };
}

// plainReason: why a class's next job waits, in one sentence. Placement says
// "no host with 3.8cpu/19.2G free (h1 0.8cpu/2.8G, h2 down)"; this says which
// of the two is short and the most there is anywhere.
// The free room the reason quotes is from the last placement round; the cards
// show the host now. Hosts it names that are up are read from the latest state
// (setLive), so the sentence and the card never disagree.
let live = new Map();
export const setLive = (hosts) => { live = new Map((hosts || []).filter((h) => h.reachable && !h.closed && h.memBytes).map((h) => [h.name, h])); };
export function plainReason(reason) {
  const r = reason || "";
  if (!r) return "a runner is starting for it";
  if (/^(high-only mode|mode high-only)/.test(r)) return "held: high-only mode is on, only high and expedited jobs start";
  const p = parseReason(r);
  if (!p) return r.replace(/(\d+(?:\.\d+)?)G\b/g, (_, g) => gbn(+g)).replace(/(\d+(?:\.\d+)?)cpu\b/g, (_, c) => cpus(+c));
  const open = p.hosts.filter((h) => !h.closed);
  for (const h of p.hosts) {
    const now = !h.closed && live.get(h.name);
    if (now) { h.cpu = +(now.freeCpus || 0).toFixed(1); h.mem = +((now.freeMem || 0) / GiB).toFixed(1); }
  }
  const shut = p.hosts.filter((h) => h.closed).map((h) => `${h.name} ${plainNote(h.note)}`);
  // a host that is closed or reserved is often the whole story: always said
  const tail = shut.length ? `; ${shut.join("; ")}` : "";
  if (!open.length) return `needs ${cpus(p.needCpu)} and ${gbn(p.needMem)}; no host takes jobs now${tail}`;
  const memOK = open.filter((h) => h.mem >= p.needMem);
  if (!memOK.length) {
    const best = open.slice().sort((a, b) => b.mem - a.mem)[0];
    return `needs ${gbn(p.needMem)} memory, the most free${shut.length ? " elsewhere" : ""} is ${gbn(best.mem)} (${best.name})${tail}`;
  }
  const best = memOK.slice().sort((a, b) => b.cpu - a.cpu)[0];
  if (best.cpu < p.needCpu) return `needs ${cpus(p.needCpu)}, ${best.name} has the memory but only ${cpus(best.cpu)} free${tail}`;
  return `needs ${cpus(p.needCpu)} and ${gbn(p.needMem)}; room is opening on ${best.name}${tail}`;
}
// expectText: when a waiting job is expected to start (state's expect), in a few words.
export function expectText(e) {
  if (!e) return "";
  const s = e.inSeconds || 0;
  const inw = s < 60 ? "under a minute" : s < 5400 ? "~" + Math.round(s / 60) + "m" : "~" + Math.floor(s / 3600) + "h" + String(Math.floor((s % 3600) / 60)).padStart(2, "0") + "m";
  return `starts in ${inw} on ${e.host}` + (e.after ? `, when ${e.after} ends` : "");
}
const plainNote = (n) => (/^(down|unreach)/.test(n) ? "offline" : n);

export const levelWord = (j) => (j.expedited || j.level === "expedite" ? "expedited" : j.level === "high" ? "high" : j.level === "low" ? "low" : "");

// Problem kinds as headings.
export const kindWord = (k) => ({ "host-unreachable": "host offline", "job-waiting": "waiting too long", "out-of-memory": "out of memory", "runner-died": "runner died", "disk-low": "disk almost full", "agent-restarted": "agent restarted" })[k] || String(k || "").replace(/-/g, " ");
