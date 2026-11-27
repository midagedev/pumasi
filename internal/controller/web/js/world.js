// The world: where everything stands, worked out from /api/state, and what
// moves between two reads. Coordinates are tiles on the ground plane.
//
//   GitHub dock (front) --line belts--> trunk --> host halls (back)
//                                         \--> shipping (right end)
//
// A host is a hall whose floor is its memory: each runner claims floor in
// proportion to what placement counts it as holding; then the free floor,
// the floor kept free for the machine, others' use, and what is not lent.
import { GiB, clamp, hash, ease, parseReason, CLASS_COLORS } from "./util.js";

export const HALL_D = 8;          // hall depth in tiles
export const GAP = 3.2;           // between halls
export const LINE_LEN = 9.5;     // belt length
export const LY0 = 3.1;           // where line belts meet the trunk area
export const TRUNK = [0, 2];      // trunk y: far lane [0,1] in, near lane [1,2] out
export const BELT_Z = 0.14;

const keepMemOf = (m) => Math.min(Math.max(m / 10, GiB), 4 * GiB);

export function hallWidth(h) {
  const g = (h.memBytes || 16 * GiB) / GiB;
  return clamp(Math.round(1.5 * Math.sqrt(g)), 7, 17);
}

// classesOf: the config's classes when the controller says them, else the names seen anywhere.
export function classesOf(s) {
  let out = (s.classes || []).map((c) => ({ ...c }));
  const seen = new Set(out.map((c) => c.name));
  const add = (name) => { if (name && !seen.has(name)) { seen.add(name); out.push({ name, os: "linux", driver: "", cpus: 2, memory: 4 * GiB, guessed: true }); } };
  for (const r of s.runners || []) add(r.class);
  for (const w of s.waiting || []) add(w.class);
  for (const o of s.overflow || []) add(o.class);
  for (const o of s.overflow || []) {
    const c = out.find((x) => x.name === o.class);
    if (c) { c.overflow = true; if (o.after) c.after = o.after; }
  }
  // a class that comes after another stands right behind it: their bridge is short
  const ordered = [];
  const byName = new Map(out.map((c) => [c.name, c]));
  for (const c of out) {
    if (ordered.includes(c)) continue;
    if (c.after && byName.has(c.after) && !ordered.includes(byName.get(c.after))) continue;
    ordered.push(c);
    for (const d of out) if (d.after === c.name && !ordered.includes(d)) ordered.push(d);
  }
  for (const c of out) if (!ordered.includes(c)) ordered.push(c);
  ordered.forEach((c, i) => { c.color = CLASS_COLORS[i % CLASS_COLORS.length]; c.index = i; });
  return ordered;
}

const runnerHeld = (r) => {
  if (r.heldMem) return [r.heldCpus || r.estCpus, r.heldMem];
  const hold = r.watched ? r.estMem * 3 : r.estMem;
  return [Math.max(r.estCpus, r.cpuNow || 0), Math.max(hold, r.memBytes || 0)];
};

export class World {
  constructor() {
    this.halls = []; this.lines = []; this.machines = new Map(); this.crates = new Map();
    this.ship = []; this.sig = ""; this.first = true; this.now = 0; this.fx = [];
  }

  // layout: recomputed only when the set of hosts, classes or a host's size changes.
  layout(s, classes) {
    const sig = JSON.stringify([s.hosts.map((h) => [h.name, hallWidth(h)]), classes.map((c) => [c.name, c.overflow, c.after])]);
    if (sig === this.sig) return false;
    this.sig = sig;
    const old = new Map(this.halls.map((h) => [h.name, h]));
    let x = 1;
    this.halls = s.hosts.map((h) => {
      const W = hallWidth(h);
      const hall = Object.assign(old.get(h.name) || { light: 1, shutter: 0, smoke: 0 }, { name: h.name, x0: x, x1: x + W, y0: -1 - HALL_D, y1: -1, W, D: HALL_D, door: x + 1.6 });
      x += W + GAP;
      return hall;
    });
    const hallsEnd = Math.max(x - GAP, 12);
    const n = Math.max(classes.length, 1);
    const span = hallsEnd - 1;
    const pitch = clamp(span / n, 3.6, 5.4);
    const start = 1 + Math.max(0, (span - pitch * n) / 2) + (pitch - 1) / 2 - 0.5;
    const oldL = new Map(this.lines.map((l) => [l.name, l]));
    this.lines = classes.map((c, i) => Object.assign(oldL.get(c.name) || {}, { name: c.name, x0: start + i * pitch, y0: LY0, y1: LY0 + LINE_LEN, cls: c }));
    const linesEnd = this.lines.length ? this.lines[this.lines.length - 1].x0 + 2.5 : hallsEnd;
    const sx0 = Math.max(hallsEnd, linesEnd) + 1.4;
    this.shipArea = { x0: sx0, x1: sx0 + 6.6, y0: -5.2, y1: 4.2 };
    this.trunk = { x0: Math.min(0, start - 1.5), x1: sx0 + 0.6 };
    this.dock = { x0: Math.min(0, start - 1.5), x1: linesEnd + 0.6, y0: LY0 + LINE_LEN, y1: LY0 + LINE_LEN + 3.0 };
    this.bounds = { x0: this.trunk.x0 - 2, x1: this.shipArea.x1 + 2, y0: -1 - HALL_D - 3, y1: this.dock.y1 + 2 };
    return true;
  }

  // update joins a new read with what is drawn: entities keep their place and
  // move to their new one. t is the scene clock in seconds.
  update(s, jobs, t, opts = {}) {
    this.state = s;
    this.express = opts.express || null;
    this.mode = opts.mode || null;
    const classes = classesOf(s);
    this.classes = classes;
    const relaid = this.layout(s, classes);
    const instant = this.first || opts.instant;
    const hallBy = new Map(this.halls.map((h) => [h.name, h]));
    const lineBy = new Map(this.lines.map((l) => [l.name, l]));

    // Halls: their host's facts and the floor plan.
    const runnersBy = new Map();
    for (const r of s.runners) {
      if (!runnersBy.has(r.host)) runnersBy.set(r.host, []);
      runnersBy.get(r.host).push(r);
    }
    for (const h of s.hosts) {
      const hall = hallBy.get(h.name);
      hall.h = h;
      hall.status = h.reachable === null || h.reachable === undefined ? "unknown" : !h.reachable ? "down" : h.closed ? "closed" : "ok";
      this.floor(hall, h, (runnersBy.get(h.name) || []).slice().sort((a, b) => b.ageSeconds - a.ageSeconds || (a.name < b.name ? -1 : 1)), classes);
    }
    // Runners whose host is not a hall (left the config): no floor to stand on, so not drawn.

    // Machines
    const live = new Set();
    for (const hall of this.halls) {
      for (const c of hall.claims || []) {
        const r = c.r;
        live.add(r.name);
        let m = this.machines.get(r.name);
        if (!m) {
          m = { name: r.name, born: instant ? t - 10 : t, x: c.cx, y: c.cy, phase: (hash(r.name) % 1000) / 160, gear: 0, puff: 0 };
          this.machines.set(r.name, m);
        }
        if (m.dying) m.dying = 0;
        m.r = r; m.hall = hall; m.cls = classes.find((k) => k.name === r.class) || classes[0];
        if (instant || m.tx === undefined) { m.x = c.cx; m.y = c.cy; }
        else if (m.tx !== c.cx || m.ty !== c.cy) { m.fx = m.x; m.fy = m.y; m.mt = t; }
        m.tx = c.cx; m.ty = c.cy; m.s = c.s; m.claim = c;
      }
    }
    const finished = new Map((jobs || []).map((j) => [j.runner, j]));
    for (const [name, m] of this.machines) {
      if (live.has(name) || m.dying) continue;
      m.dying = t;
      const j = finished.get(name);
      if (j && !instant && (j.job || j.startedAt)) this.shipCrate(j, m, t);
    }

    // Waiting jobs: crates on their line, oldest at the front.
    const sizes = new Map((s.sizes || []).map((z) => [z.workflow + "\u0000" + z.job, z]));
    const reasonOf = new Map((s.waiting || []).map((w) => [w.class, w]));
    const want = new Map();
    const byLine = new Map();
    for (const j of s.waitingJobs || []) {
      const key = "w:" + j.class + "|" + j.runId + "|" + j.job;
      if (want.has(key)) continue;
      want.set(key, j);
      if (!byLine.has(j.class)) byLine.set(j.class, []);
      byLine.get(j.class).push(key);
    }
    // Classes that wait without per-job rows (an older controller): one crate per job.
    for (const w of s.waiting || []) {
      if (byLine.has(w.class)) continue;
      const keys = [];
      for (let i = 0; i < w.jobs; i++) {
        const key = "w:" + w.class + "|n" + i;
        want.set(key, { class: w.class, job: "", runId: 0, waitedSeconds: w.oldestSeconds, reason: w.reason });
        keys.push(key);
      }
      byLine.set(w.class, keys);
    }
    for (const [cls, keys] of byLine) {
      // the controller's place in line rules (an expedited job's older classmates
      // still go first: GitHub hands a label's jobs out oldest first); without one,
      // expedited first
      const rank = (k) => { const j = want.get(k); return j.position ? [0, j.position] : [j.expedited ? 0 : 1, 1e9]; };
      const idx = new Map(keys.map((k, i) => [k, i]));
      keys.sort((a, b) => { const ra = rank(a), rb = rank(b); return ra[0] - rb[0] || ra[1] - rb[1] || idx.get(a) - idx.get(b); });
    }
    for (const l of this.lines) {
      l.w = reasonOf.get(l.name) || null;
      l.ov = (s.overflow || []).find((o) => o.class === l.name) || null;
      l.keys = byLine.get(l.name) || [];
      l.extra = Math.max(0, l.keys.length - 9);
      l.need = l.w ? parseReason(l.w.reason) : null;
    }
    const taken = new Set();
    for (const [key, j] of want) {
      const line = lineBy.get(j.class);
      if (!line) continue;
      const idx = line.keys.indexOf(key);
      const slot = Math.min(idx, 9);
      const tx = line.x0 + 0.5, ty = line.y0 + 0.65 + slot * 1.0;
      let c = this.crates.get(key);
      const cls = line.cls;
      const est = sizes.get(j.workflow + "\u0000" + j.job);
      let mem = est ? est.estMem : cls.memory || 4 * GiB;
      if (idx === 0 && line.need) mem = line.need.needMem * GiB;
      if (!c) {
        c = { key, mode: "queue", born: instant ? t - 10 : t, x: tx, y: instant ? ty : line.y1 + 0.6, z: BELT_Z };
        if (!instant) this.move(c, [[tx, line.y1 + 0.6], [tx, ty]], t, 0.9);
        this.crates.set(key, c);
      } else if (c.mode === "queue" && (c.tx !== tx || c.ty !== ty)) {
        // a crate that jumps the queue (expedited) is lifted over the others
        const jump = c.slot !== undefined && slot < c.slot - 1;
        this.move(c, [[c.x, c.y], [tx, ty]], t, jump ? 1.4 : 0.7);
        c.hop = jump;
      }
      Object.assign(c, { j, line, cls, tx, ty, slot, idx, mem, level: j.level || "", hidden: idx > 9 });
    }
    // Crates no longer waiting: to the runner that took them, back to GitHub, or gone.
    const back = new Map((s.takenBack || []).map((j) => [j.class + "|" + j.runId + "|" + j.job, j]));
    for (const [key, c] of this.crates) {
      if (c.mode !== "queue" || want.has(key)) continue;
      const j = c.j;
      const m = instant ? null : [...this.machines.values()].find((m) => !m.dying && !taken.has(m.name) && m.r.class === j.class && m.r.job && m.r.job === j.job);
      if (m) {
        taken.add(m.name);
        c.mode = "in"; c.target = m;
        const hall = m.hall, lx = c.line.x0 + 0.5;
        this.move(c, [[c.x, c.y], [lx, LY0 - 0.4], [lx, 0.5], [hall.door + 0.5, 0.5], [hall.door + 0.5, -1.6], [m.tx, m.ty]], t, null, true);
      } else if (!instant && back.has(j.class + "|" + j.runId + "|" + j.job)) {
        c.mode = "back";
        const px = c.line.x0 + 1.55;
        this.move(c, [[c.x, c.y], [px, c.y], [px, c.line.y1 + 0.8]], t, null, true, 0.55);
      } else {
        c.mode = "gone"; c.t0 = t;
      }
    }
    // Shipping: the newest finished jobs stand on the pallets.
    this.shipping(jobs || [], t, instant);
    this.first = false;
    return relaid;
  }

  floor(hall, h, runners, classes) {
    const T = hall.W * hall.D;
    const mem = h.memBytes || 0;
    hall.claims = [];
    hall.tiles = new Array(T);
    if (!mem) {
      // Nothing measured: runners stand in a row, the rest is unknown floor.
      let i = 0;
      for (const r of runners) {
        const n = 2;
        hall.claims.push(this.claim(hall, r, i, n));
        i += n;
      }
      for (; i < T; i++) hall.tiles[i] = { zone: "unknown" };
      hall.zones = null;
      return;
    }
    const held = runners.map((r) => runnerHeld(r));
    const sumHeld = held.reduce((a, b) => a + b[1], 0);
    const keep = h.keepMem || keepMemOf(mem);
    const others = h.otherMem !== undefined ? h.otherMem : null;
    const free = h.freeMem || 0;
    // reserve: room kept for a class's runners, shown on the host that has the most free memory and can take it
    let reserve = null;
    for (const c of classes) {
      if (!c.reserve) continue;
      const have = runners.filter((r) => r.class === c.name).length + (this.state.runners.filter((r) => r.class === c.name && r.host !== h.name).length);
      const need = c.reserve - have;
      if (need <= 0) continue;
      const best = this.state.hosts.filter((x) => x.reachable && (c.os || "linux") === (x.os || "linux") && (!c.hosts || !c.hosts.length || c.hosts.includes(x.name))).sort((a, b) => b.freeMem - a.freeMem)[0];
      if (best && best.name === h.name) reserve = { cls: c, mem: Math.min(free, need * (c.memory || 4 * GiB)), n: need };
    }
    let fr = { free: free / mem, keep: keep / mem, others: others === null ? 0 : others / mem };
    let rest = 1 - sumHeld / mem - fr.free - fr.keep - fr.others;
    if (others === null) { fr.others = Math.max(0, rest); rest = 0; }
    if (rest < 0) {
      // over-committed (held is an estimate, the rest is measured): shrink the measured shares to fit
      const k = Math.max(0, 1 - sumHeld / mem) / Math.max(1e-9, fr.free + fr.keep + fr.others);
      fr.free *= k; fr.keep *= k; fr.others *= k; rest = 0;
    }
    fr.unlent = rest;
    hall.zones = { held: sumHeld, free, keep, others: others === null ? mem - sumHeld - free - keep : others, unlent: rest * mem, mem, reserve };
    // tiles: runners first, each at least one tile
    let i = 0;
    runners.forEach((r, k) => {
      const n = Math.max(1, Math.round((held[k][1] / mem) * T));
      if (i + n > T) { hall.claims.push(this.claim(hall, r, Math.max(0, T - 1), 1, true)); return; }
      hall.claims.push(this.claim(hall, r, i, n));
      i += n;
    });
    const left = T - i;
    const parts = [["free", fr.free], ["keep", fr.keep], ["others", fr.others], ["unlent", fr.unlent]];
    const tot = parts.reduce((a, p) => a + p[1], 0) || 1;
    let counts = parts.map(([z, f]) => [z, (f / tot) * left]);
    let ints = counts.map(([z, f]) => [z, Math.floor(f)]);
    let rem = left - ints.reduce((a, p) => a + p[1], 0);
    counts.map((p, k) => [k, p[1] - Math.floor(p[1])]).sort((a, b) => b[1] - a[1]).slice(0, rem).forEach(([k]) => ints[k][1]++);
    const resTiles = reserve ? Math.min(ints[0][1], Math.max(1, Math.round((reserve.mem / mem) * T))) : 0;
    // an expedited job's room is kept on its host: that much of the free floor, after the reserve
    const ex = this.express && this.express.host === h.name ? this.express : null;
    let exTiles = 0;
    if (ex) {
      const line = this.lines.find((l) => l.name === ex.class);
      const need = line && line.need ? line.need.needMem * GiB : (classes.find((c) => c.name === ex.class) || {}).memory || 4 * GiB;
      exTiles = Math.max(1, Math.min(ints[0][1] - resTiles, Math.round((need / mem) * T)));
    }
    for (const [z, n] of ints) {
      for (let k = 0; k < n; k++, i++) {
        hall.tiles[i] = { zone: z === "free" && k < resTiles ? "reserve" : z === "free" && k < resTiles + exTiles ? "express" : z };
      }
    }
    hall.expressTiles = exTiles ? { n: exTiles, name: ex.name } : null;
    hall.reserve = reserve && resTiles ? { cls: reserve.cls, n: reserve.n, tiles: resTiles } : null;
  }

  // tile i of a hall: columns left to right, the rows snaking so a claim stays in one piece
  tileXY(hall, i) {
    const col = Math.floor(i / hall.D), row = i % hall.D;
    const r = col % 2 === 0 ? row : hall.D - 1 - row;
    return [hall.x0 + col, hall.y0 + r];
  }

  claim(hall, r, i0, n, squeezed = false) {
    let sx = 0, sy = 0;
    const tiles = [];
    for (let k = 0; k < n; k++) {
      const [x, y] = this.tileXY(hall, i0 + k);
      hall.tiles[i0 + k] = { zone: "runner", runner: r.name };
      tiles.push([x, y]);
      sx += x + 0.5; sy += y + 0.5;
    }
    let cx = sx / n, cy = sy / n;
    // the machine stands on its own floor: move the centre to the nearest tile of the claim if it fell outside
    if (!tiles.some(([x, y]) => cx >= x && cx < x + 1 && cy >= y && cy < y + 1)) {
      const best = tiles.reduce((a, b) => (Math.hypot(b[0] + 0.5 - cx, b[1] + 0.5 - cy) < Math.hypot(a[0] + 0.5 - cx, a[1] + 0.5 - cy) ? b : a));
      cx = (cx + best[0] + 0.5) / 2; cy = (cy + best[1] + 0.5) / 2;
    }
    const s = clamp(0.66 * Math.sqrt(n), 0.72, 2.7);
    return { r, i0, n, tiles, cx, cy, s, squeezed };
  }

  // move sends an entity along a path of ground points; dur in seconds or by speed.
  move(e, path, t, dur, keepZ, speed = 1) {
    let len = 0;
    for (let i = 1; i < path.length; i++) len += Math.hypot(path[i][0] - path[i - 1][0], path[i][1] - path[i - 1][1]);
    e.path = path; e.t0 = t; e.len = len;
    e.dur = dur || clamp(len / (5.5 * speed), 0.8, 3.6);
    if (!keepZ) e.z = BELT_Z;
  }

  shipCrate(j, m, t) {
    const key = "s:" + j.runner;
    if (this.crates.has(key)) return;
    const c = { key, mode: "out", j, x: m.x, y: m.y, z: BELT_Z, born: t, result: resultOf(j), cls: m.cls };
    const hall = m.hall;
    this.move(c, [[m.x, m.y], [hall.door + 1.3, -1.6], [hall.door + 1.3, 1.5], [this.shipArea.x0 + 0.6, 1.5]], t, null, true);
    this.crates.set(key, c);
  }

  shipping(jobs, t, instant) {
    const done = jobs.filter((j) => (j.job || j.startedAt) && j.finishedAt).slice(0, 20);
    const a = this.shipArea;
    const keep = new Set();
    done.forEach((j, i) => {
      const key = "s:" + j.runner;
      keep.add(key);
      const col = i % 4, row = Math.floor(i / 4);
      const tx = a.x0 + 1.4 + col * 1.25, ty = a.y0 + 1.6 + row * 1.6;
      let c = this.crates.get(key);
      if (!c) {
        c = { key, mode: "ship", j, x: tx, y: ty, z: 0.18, born: instant ? t - 10 : t, result: resultOf(j), cls: this.classes.find((k) => k.name === j.class) };
        this.crates.set(key, c);
      } else if (c.mode === "out" || c.mode === "ship") {
        if (c.mode === "out" && c.path) {
          // continue its trip from the trunk end to its pallet
          const p = c.path;
          if (p[p.length - 1][0] !== tx || p[p.length - 1][1] !== ty) p.push([a.x0 + 0.6, ty], [tx, ty]), (c.dur += 0.9);
        } else if (c.tx !== tx || c.ty !== ty) this.move(c, [[c.x, c.y], [tx, ty]], t, 0.8, true);
        c.mode = c.mode === "out" ? "out" : "ship";
      }
      c.j = j; c.tx = tx; c.ty = ty; c.slot = i;
    });
    for (const [key, c] of this.crates) {
      if (key.startsWith("s:") && !keep.has(key) && c.mode !== "out" && c.mode !== "gone") { c.mode = "gone"; c.t0 = t; }
    }
  }

  // step advances moving things to time t; returns the entities to drop.
  step(t) {
    for (const [key, c] of this.crates) {
      if (c.path) {
        const k = clamp((t - c.t0) / c.dur, 0, 1);
        const [x, y] = along(c.path, c.len, ease(k));
        c.x = x; c.y = y;
        if (c.hop) c.lift = Math.sin(Math.PI * k) * 1.5;
        if (k >= 1) {
          c.path = null; c.hop = false; c.lift = 0;
          if (c.mode === "in") { c.mode = "gone"; c.t0 = t; c.absorbed = true; }
          else if (c.mode === "back") { c.mode = "gone"; c.t0 = t; }
          else if (c.mode === "out") { c.mode = "ship"; c.z = 0.18; if (c.tx === undefined) { c.mode = "gone"; c.t0 = t; } else { c.x = c.tx; c.y = c.ty; } }
        }
      }
      if (c.mode === "gone" && t - c.t0 > 0.6) this.crates.delete(key);
    }
    for (const [name, m] of this.machines) {
      if (m.mt !== undefined) {
        const k = clamp((t - m.mt) / 0.9, 0, 1);
        m.x = m.fx + (m.tx - m.fx) * ease(k);
        m.y = m.fy + (m.ty - m.fy) * ease(k);
        if (k >= 1) m.mt = undefined;
      }
      if (m.dying && t - m.dying > 1.2) this.machines.delete(name);
    }
  }
}

export function resultOf(j) {
  if (j.oomKills) return "oom";
  if (j.result === "succeeded") return "ok";
  if (j.result === "canceled" || j.result === "cancelled" || j.result === "skipped") return "cancel";
  if (j.result) return "fail";
  if (j.state === "failed" || j.state === "lost") return "fail";
  return "unknown";
}

function along(path, len, k) {
  let d = k * len;
  for (let i = 1; i < path.length; i++) {
    const [ax, ay] = path[i - 1], [bx, by] = path[i];
    const seg = Math.hypot(bx - ax, by - ay);
    if (d <= seg || i === path.length - 1) {
      const u = seg ? Math.min(1, d / seg) : 1;
      return [ax + (bx - ax) * u, ay + (by - ay) * u];
    }
    d -= seg;
  }
  return path[path.length - 1];
}
