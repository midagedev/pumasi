// The scene: everything the world holds, drawn each frame. Flat things first
// (ground, floors, belts, shadows), then the back walls, then every solid
// sorted back to front, then light, smoke and the labels.
import { TW, TH, TZ, box, cylinder, poly, noiseTile } from "./iso.js";
import { HALL_D, LY0, BELT_Z } from "./world.js";
import { GiB, rgb, shade, mix, hex, clamp, hash, rng, gib, dur, shortReason, osBadge, since } from "./util.js";

const WALL_H = 2.3;

export const THEMES = {
  dark: {
    night: true,
    bg: ["#05080c", "#0b121a"],
    yard: [22, 28, 35], yardAmp: 18,
    floor: [78, 84, 92], floorAmp: 12, seam: "rgba(0,0,0,0.32)",
    wall: [72, 80, 92], steel: [86, 94, 106], dark: [34, 38, 44],
    belt: [24, 27, 31], rail: [120, 128, 140],
    paint: [232, 190, 64], hazard: [222, 170, 40],
    free: [70, 150, 110], keep: [110, 96, 40], others: [96, 96, 104], unlent: [14, 16, 20], unknown: [44, 48, 54],
    crate: [196, 154, 98], hot: [236, 70, 50], warm: [240, 150, 50],
    ok: [72, 208, 128], warn: [240, 180, 41], bad: [255, 77, 77], off: [100, 108, 118],
    text: "#eef3f8", dim: "#93a1ae", label: "rgba(10,14,20,0.82)", labelLine: "rgba(255,255,255,0.12)",
    lamp: [255, 196, 120], glass: [120, 190, 220],
  },
  light: {
    night: false,
    bg: ["#cfd8d4", "#e7ece6"],
    yard: [150, 158, 150], yardAmp: 14,
    floor: [196, 198, 194], floorAmp: 10, seam: "rgba(0,0,0,0.14)",
    wall: [176, 182, 190], steel: [150, 158, 168], dark: [92, 98, 106],
    belt: [58, 62, 68], rail: [170, 176, 184],
    paint: [168, 118, 6], hazard: [220, 168, 30],
    free: [150, 196, 168], keep: [210, 190, 120], others: [160, 160, 166], unlent: [92, 96, 102], unknown: [170, 174, 178],
    crate: [204, 162, 104], hot: [220, 60, 40], warm: [232, 140, 40],
    ok: [30, 160, 90], warn: [210, 140, 10], bad: [220, 50, 50], off: [130, 136, 144],
    text: "#14191f", dim: "#56616d", label: "rgba(255,255,255,0.9)", labelLine: "rgba(0,0,0,0.12)",
    lamp: [255, 220, 160], glass: [110, 170, 200],
  },
};

const RESULT = { ok: "ok", fail: "bad", oom: "bad", cancel: "off", unknown: "off" };

export class Scene {
  constructor(view) {
    this.v = view;
    this.setTheme("dark");
    this.hits = [];
    this.fx = [];
    this.lastFx = 0;
    this.bursts = new Set();
  }

  setTheme(name) {
    this.themeName = name;
    const th = (this.th = THEMES[name]);
    const ctx = this.v.ctx;
    const mk = (canvas, scale) => {
      const p = ctx.createPattern(canvas, "repeat");
      p.scale = scale;
      return p;
    };
    this.pat = {
      yard: mk(noiseTile(128, th.yard, th.yardAmp, 7, { blots: 26 }), 1 / 24),
      floor: mk(noiseTile(64, th.floor, th.floorAmp, 11, { seams: th.seam, blots: 3 }), 1 / 64),
      steel: mk(noiseTile(64, th.steel, 10, 5), 1 / 48),
      hatch: mk(stripes(th.hazard, [30, 30, 30], 16), 1 / 64),
      unlent: mk(stripes(shade(th.unlent, 1.6), th.unlent, 12), 1 / 48),
      reserve: mk(stripes([180, 140, 255], shade(th.floor, 0.9), 14), 1 / 64),
      express: mk(stripes([255, 122, 217], shade(th.floor, 0.8), 14), 1 / 64),
      chev: mk(chevrons(th.belt), 1 / 64),
      rollers: mk(rollers(th.belt), 1 / 64),
    };
  }

  // ---------------------------------------------------------------- frame
  frame(world, t, opts) {
    const v = this.v, ctx = v.ctx, th = this.th;
    this.dt = this.t === undefined ? 0.016 : clamp(t - this.t, 0, 0.1);
    this.t = t; this.world = world; this.opts = opts;
    this.hits = []; this.glows = []; this.pools = [];
    v.base();
    const g = ctx.createLinearGradient(0, 0, 0, v.h);
    g.addColorStop(0, th.bg[0]);
    g.addColorStop(1, th.bg[1]);
    ctx.fillStyle = g;
    ctx.fillRect(0, 0, v.w, v.h);
    if (!world.bounds) return;
    this.ground(world);
    for (const hall of world.halls) this.backWalls(hall);
    const items = [];
    this.collect(world, items);
    items.sort((a, b) => a.d - b.d);
    for (const it of items) it.f();
    if (th.night) this.lights(world);
    this.effects(world, t);
    if (opts.prioView) {
      // the line on its own: everything else steps back, the crates and gates come forward
      v.base();
      ctx.fillStyle = th.night ? "rgba(4,6,10,0.62)" : "rgba(235,238,232,0.66)";
      ctx.fillRect(0, 0, v.w, v.h);
      const fore = [];
      for (const l of world.lines) fore.push({ d: l.x0 + l.y0, f: () => this.gate(l) });
      for (const c of world.crates.values()) if (c.mode === "queue" && !c.hidden) fore.push({ d: c.x + c.y, f: () => this.crate(c) });
      fore.sort((a, b) => a.d - b.d);
      for (const it of fore) it.f();
    }
    this.labels(world);
    v.base();
  }

  pattern(p, ox = 0, oy = 0) {
    p.setTransform(new DOMMatrix([p.scale, 0, 0, p.scale, ox, oy]));
    return p;
  }

  // ---------------------------------------------------------------- ground
  ground(w) {
    const v = this.v, ctx = v.ctx, th = this.th, b = w.bounds, t = this.t;
    v.onGround();
    ctx.fillStyle = this.pattern(this.pat.yard);
    ctx.fillRect(b.x0 - 30, b.y0 - 30, b.x1 - b.x0 + 60, b.y1 - b.y0 + 60);
    // the plant's lot: a slab a shade lighter, with a painted border
    ctx.fillStyle = rgb(shade(th.yard, th.night ? 1.18 : 1.06), 0.55);
    ctx.fillRect(b.x0, b.y0, b.x1 - b.x0, b.y1 - b.y0);
    ctx.strokeStyle = rgb(th.paint, 0.35);
    ctx.lineWidth = 0.06;
    ctx.setLineDash([0.6, 0.4]);
    ctx.strokeRect(b.x0 + 0.3, b.y0 + 0.3, b.x1 - b.x0 - 0.6, b.y1 - b.y0 - 0.6);
    ctx.setLineDash([]);

    for (const hall of w.halls) this.hallFloor(hall);

    // trunk: far lane rollers (either way), near lane a belt to shipping
    const tr = w.trunk;
    ctx.fillStyle = rgb(th.dark);
    ctx.fillRect(tr.x0, -0.15, tr.x1 - tr.x0, 2.3);
    ctx.fillStyle = this.pattern(this.pat.rollers);
    ctx.fillRect(tr.x0, 0, tr.x1 - tr.x0, 1);
    ctx.fillStyle = this.pattern(this.pat.chev, this.opts.motion ? (t * 0.9) % 1 : 0, 1.05);
    ctx.fillRect(tr.x0, 1.05, tr.x1 - tr.x0, 0.95);
    ctx.fillStyle = rgb(th.rail);
    for (const y of [-0.08, 0.99, 2.0]) ctx.fillRect(tr.x0, y, tr.x1 - tr.x0, 0.07);
    // spurs into each hall
    for (const hall of w.halls) {
      ctx.fillStyle = rgb(th.dark);
      ctx.fillRect(hall.door - 0.15, -1.9, 1.9, 1.85);
      ctx.fillStyle = this.pattern(this.pat.rollers);
      ctx.save(); ctx.translate(hall.door, -1.8); ctx.rotate(Math.PI / 2);
      ctx.fillRect(0, -0.95, 1.8, 0.9);
      ctx.restore();
      // out of the hall: a belt from the floor to the near lane
      ctx.save(); ctx.translate(hall.door + 1.65, -1.85); ctx.rotate(Math.PI / 2);
      ctx.fillStyle = this.pattern(this.pat.chev, this.opts.motion ? (t * 0.9) % 1 : 0, 0);
      ctx.fillRect(0, 0, 3.0, 0.7);
      ctx.restore();
    }
    // line belts toward the trunk
    for (const l of w.lines) this.lineGround(l);
    // painted words on the ground
    this.groundText("TRUNK  ▸  SHIPPING", tr.x0 + 1, 2.75, 0.55, rgb(th.paint, 0.45), 0);
    // shipping yard
    const s = w.shipArea;
    ctx.fillStyle = rgb(shade(th.floor, 0.92));
    ctx.fillRect(s.x0, s.y0, s.x1 - s.x0, s.y1 - s.y0);
    ctx.fillStyle = this.pattern(this.pat.floor);
    ctx.globalAlpha = 0.6;
    ctx.fillRect(s.x0, s.y0, s.x1 - s.x0, s.y1 - s.y0);
    ctx.globalAlpha = 1;
    ctx.strokeStyle = rgb(th.paint, 0.7);
    ctx.lineWidth = 0.08;
    ctx.strokeRect(s.x0 + 0.2, s.y0 + 0.2, s.x1 - s.x0 - 0.4, s.y1 - s.y0 - 0.4);
    for (let i = 0; i < 20; i++) {
      const col = i % 4, row = Math.floor(i / 4);
      const x = s.x0 + 1.4 + col * 1.25 - 0.5, y = s.y0 + 1.6 + row * 1.6 - 0.5;
      ctx.fillStyle = rgb(shade(th.floor, 0.78), 0.8);
      ctx.fillRect(x, y, 1, 1);
    }
    this.groundText("SHIPPED", s.x0 + 0.6, s.y1 - 0.5, 0.85, rgb(th.paint, 0.55), 0);
    // GitHub dock apron
    const d = w.dock;
    ctx.fillStyle = rgb(shade(th.floor, 0.85));
    ctx.fillRect(d.x0, d.y0 - 0.6, d.x1 - d.x0, d.y1 - d.y0 + 0.6);
    // shadows of solids
    this.shadows(w);
    v.base();
  }

  groundText(text, x, y, size, color, angle, font = "800") {
    const ctx = this.v.ctx;
    ctx.save();
    ctx.translate(x, y);
    if (angle) ctx.rotate(angle);
    ctx.scale(size / 32, size / 32);
    ctx.font = `${font} 32px ui-sans-serif, system-ui, sans-serif`;
    ctx.fillStyle = color;
    ctx.textBaseline = "middle";
    ctx.fillText(text, 0, 0);
    ctx.restore();
  }

  hallFloor(hall) {
    const v = this.v, ctx = v.ctx, th = this.th;
    const { x0, y0, W, D } = hall;
    // foundation, a little larger than the floor
    ctx.fillStyle = rgb(shade(th.floor, 0.7));
    ctx.fillRect(x0 - 0.45, y0 - 0.45, W + 0.9, D + 0.9);
    ctx.fillStyle = this.pattern(this.pat.floor);
    ctx.fillRect(x0, y0, W, D);
    this.aoHall = [x0, y0, W, D];
    // zones, merged into runs down each column
    const col = { free: th.free, keep: th.keep, others: th.others, unlent: th.unlent, unknown: th.unknown, reserve: th.free, express: [255, 122, 217] };
    const alpha = { free: 0.32, keep: 0.5, others: 0.75, unlent: 0.92, unknown: 0.5, reserve: 0.3, express: 0.3 };
    const tiles = hall.tiles || [];
    const runs = [];
    for (let i = 0; i < tiles.length; i++) {
      const tl = tiles[i];
      if (!tl || tl.zone === "runner") continue;
      const [x, y] = this.world.tileXY(hall, i);
      runs.push([tl.zone, x, y]);
    }
    for (const [z, x, y] of runs) {
      ctx.fillStyle = rgb(col[z], alpha[z]);
      ctx.fillRect(x, y, 1, 1);
    }
    for (const [z, x, y] of runs) {
      if (z === "keep") { ctx.fillStyle = this.pattern(this.pat.hatch); ctx.globalAlpha = 0.35; ctx.fillRect(x, y, 1, 1); ctx.globalAlpha = 1; }
      if (z === "unlent") { ctx.fillStyle = this.pattern(this.pat.unlent); ctx.globalAlpha = 0.6; ctx.fillRect(x, y, 1, 1); ctx.globalAlpha = 1; }
      if (z === "reserve") { ctx.fillStyle = this.pattern(this.pat.reserve); ctx.globalAlpha = 0.55; ctx.fillRect(x, y, 1, 1); ctx.globalAlpha = 1; }
      if (z === "express") { ctx.fillStyle = this.pattern(this.pat.express); ctx.globalAlpha = 0.55 + 0.2 * Math.sin(this.t * 3); ctx.fillRect(x, y, 1, 1); ctx.globalAlpha = 1; }
    }
    if (hall.expressTiles) {
      const i = tiles.findIndex((tl) => tl && tl.zone === "express");
      const [ex, ey] = this.world.tileXY(hall, i);
      this.groundText("KEPT FOR " + hall.expressTiles.name.toUpperCase().slice(0, 18), ex + 0.1, ey + 0.5, 0.26, "rgba(255,150,225,0.95)", 0, "900");
    }
    if (hall.reserve) {
      const i = tiles.findIndex((tl) => tl && tl.zone === "reserve");
      const [rx, ry] = this.world.tileXY(hall, i);
      this.groundText("RESERVE · " + hall.reserve.cls.name.toUpperCase(), rx + 0.1, ry + 0.5, 0.24, "rgba(200,180,255,0.95)", 0, "900");
    }
    // claims: each runner's floor painted in its class colour, with an outline
    for (const c of hall.claims || []) {
      const m = this.world.machines.get(c.r.name);
      const color = m ? m.cls.color : th.steel;
      ctx.fillStyle = rgb(color, 0.26);
      for (const [x, y] of c.tiles) ctx.fillRect(x + 0.04, y + 0.04, 0.92, 0.92);
      const own = new Set(c.tiles.map(([x, y]) => x + "," + y));
      ctx.strokeStyle = rgb(color, 0.85);
      ctx.lineWidth = 0.05;
      ctx.beginPath();
      for (const [x, y] of c.tiles) {
        if (!own.has(x - 1 + "," + y)) { ctx.moveTo(x + 0.04, y + 0.04); ctx.lineTo(x + 0.04, y + 0.96); }
        if (!own.has(x + 1 + "," + y)) { ctx.moveTo(x + 0.96, y + 0.04); ctx.lineTo(x + 0.96, y + 0.96); }
        if (!own.has(x + "," + (y - 1))) { ctx.moveTo(x + 0.04, y + 0.04); ctx.lineTo(x + 0.96, y + 0.04); }
        if (!own.has(x + "," + (y + 1))) { ctx.moveTo(x + 0.04, y + 0.96); ctx.lineTo(x + 0.96, y + 0.96); }
      }
      ctx.stroke();
      if (m && m.r.watched) {
        // a watched job holds three times its guess: the extra floor is dashed
        ctx.setLineDash([0.15, 0.12]);
        ctx.strokeStyle = rgb(th.warn, 0.9);
        ctx.stroke();
        ctx.setLineDash([]);
      }
    }
    // the free floor's edge: a painted safety line between ours and theirs
    const z = hall.zones;
    if (z) {
      const firstOther = tiles.findIndex((tl) => tl && (tl.zone === "others" || tl.zone === "keep" || tl.zone === "unlent"));
      if (firstOther > 0) {
        const colIdx = Math.floor(firstOther / D);
        ctx.strokeStyle = rgb(th.paint, 0.85);
        ctx.lineWidth = 0.09;
        ctx.setLineDash([0.35, 0.2]);
        ctx.beginPath();
        // stepped edge: the boundary between tile firstOther-1 and firstOther
        const xa = x0 + colIdx;
        const row = firstOther % D;
        const down = colIdx % 2 === 0;
        if (row === 0) { ctx.moveTo(xa, y0); ctx.lineTo(xa, y0 + D); }
        else if (down) { ctx.moveTo(xa, y0 + D); ctx.lineTo(xa, y0 + row); ctx.lineTo(xa + 1, y0 + row); ctx.lineTo(xa + 1, y0); }
        else { ctx.moveTo(xa, y0); ctx.lineTo(xa, y0 + D - row); ctx.lineTo(xa + 1, y0 + D - row); ctx.lineTo(xa + 1, y0 + D); }
        ctx.stroke();
        ctx.setLineDash([]);
      }
      // the floor's own legend, painted near the front
      const free = hall.h.freeMem;
      this.groundText(`${gib(free)} FREE`, x0 + 0.35, y0 + D - 0.45, 0.42, rgb(th.ok, 0.75), 0, "800");
    }
    // where floor meets wall the light does not reach
    let g = ctx.createLinearGradient(0, y0, 0, y0 + 1.4);
    g.addColorStop(0, "rgba(0,0,0,0.42)"); g.addColorStop(1, "rgba(0,0,0,0)");
    ctx.fillStyle = g; ctx.fillRect(x0, y0, W, 1.4);
    g = ctx.createLinearGradient(x0, 0, x0 + 1.4, 0);
    g.addColorStop(0, "rgba(0,0,0,0.36)"); g.addColorStop(1, "rgba(0,0,0,0)");
    ctx.fillStyle = g; ctx.fillRect(x0, y0, 1.4, D);
    // the host's name painted big on the floor
    const h = hall.h || {};
    this.groundText(hall.name.toUpperCase(), x0 + 0.35, y0 + D + 0.25, Math.min(0.62, (W - 0.6) / Math.max(6, hall.name.length) * 1.55), rgb(th.paint, 0.8), 0, "900");
    if (hall.status === "down" || hall.status === "unknown") {
      ctx.fillStyle = hall.status === "down" ? "rgba(0,0,0,0.55)" : "rgba(0,0,0,0.25)";
      ctx.fillRect(x0 - 0.45, y0 - 0.45, W + 0.9, D + 0.9);
    }
    // heat: a host over a pressure line glows under its machines
    const heat = this.heat(h);
    if (heat > 0.05) {
      const g = ctx.createRadialGradient(x0 + W / 2, y0 + D / 2, 0.5, x0 + W / 2, y0 + D / 2, Math.max(W, D) * 0.7);
      g.addColorStop(0, `rgba(255,110,40,${0.35 * heat})`);
      g.addColorStop(1, "rgba(255,110,40,0)");
      ctx.fillStyle = g;
      ctx.fillRect(x0, y0, W, D);
    }
  }

  heat(h) {
    const L = (this.world.state && this.world.state.limits) || {};
    const r = (p, lim) => (p === null || p === undefined ? 0 : p / (lim || 50));
    return clamp(Math.max(r(h.cpuPressure, L.psiCpu || 40), r(h.memPressure, L.psiMemory || 10), r(h.ioPressure, L.psiIo || 50)), 0, 1.5);
  }

  lineGround(l) {
    const v = this.v, ctx = v.ctx, th = this.th, t = this.t;
    const c = l.cls;
    // a lane of the class's colour, the belt on it
    ctx.fillStyle = rgb(c.color, th.night ? 0.12 : 0.18);
    ctx.fillRect(l.x0 - 0.5, l.y0 - 0.5, 2.0 + (c.overflow ? 0.6 : 0), l.y1 - l.y0 + 0.5);
    ctx.fillStyle = rgb(th.dark);
    ctx.fillRect(l.x0 - 0.1, l.y0 - 0.1, 1.2, l.y1 - l.y0 + 0.2);
    // belt chevrons run toward the trunk (-y)
    ctx.save();
    ctx.translate(l.x0, l.y1);
    ctx.rotate(-Math.PI / 2);
    ctx.fillStyle = this.pattern(this.pat.chev, this.opts.motion && l.keys.length ? (t * 0.7) % 1 : 0, 0);
    ctx.fillRect(0, 0.05, l.y1 - l.y0, 0.9);
    ctx.restore();
    ctx.fillStyle = rgb(c.color, 0.9);
    ctx.fillRect(l.x0 - 0.1, l.y0 - 0.1, 0.06, l.y1 - l.y0 + 0.2);
    ctx.fillRect(l.x0 + 1.04, l.y0 - 0.1, 0.06, l.y1 - l.y0 + 0.2);
    // the class name painted along the lane, reading toward the trunk
    ctx.save();
    ctx.translate(l.x0 - 0.42, l.y1 - 0.2);
    ctx.rotate(-Math.PI / 2);
    ctx.font = "900 32px ui-sans-serif, system-ui, sans-serif";
    ctx.scale(0.4 / 32, 0.4 / 32);
    ctx.fillStyle = rgb(shade(c.color, 1.2), 0.85);
    ctx.textBaseline = "middle";
    ctx.fillText(l.name.toUpperCase() + "  ·  " + osBadge(c.os) + (c.driver ? " " + c.driver.toUpperCase() : ""), 0, 0);
    ctx.restore();
    // a stop line where the belt meets the trunk
    ctx.fillStyle = rgb(th.paint, 0.9);
    for (let i = 0; i < 4; i++) ctx.fillRect(l.x0 - 0.1 + i * 0.32, l.y0 - 0.32, 0.18, 0.12);
    if (c.reserve) {
      // a reserve: bays kept empty for the class, hatched
      ctx.fillStyle = this.pattern(this.pat.reserve);
      ctx.globalAlpha = 0.6;
      ctx.fillRect(l.x0 + 1.25, l.y0 + 0.2, 0.8, 1.6);
      ctx.globalAlpha = 1;
    }
  }

  shadows(w) {
    const ctx = this.v.ctx;
    ctx.fillStyle = this.th.night ? "rgba(0,0,0,0.38)" : "rgba(30,40,30,0.22)";
    // the sun is up and to the left: shadows fall down-right (+x, a little -y)
    const sh = (x, y, wd, dp, h) => {
      const dx = h * 0.55, dy = -h * 0.18;
      ctx.beginPath();
      ctx.moveTo(x, y + dp); ctx.lineTo(x + wd, y + dp); ctx.lineTo(x + wd, y);
      ctx.lineTo(x + wd + dx, y + dy); ctx.lineTo(x + wd + dx, y + dp + dy); ctx.lineTo(x + dx, y + dp + dy);
      ctx.closePath(); ctx.fill();
    };
    for (const m of w.machines.values()) {
      const s = m.s || 1;
      sh(m.x - s / 2, m.y - s / 2, s, s, 0.9 + 0.3 * s);
    }
    for (const hall of w.halls) {
      sh(hall.x0 - 0.35, hall.y0 - 0.35, hall.W + 0.35, 0.35, WALL_H);
      sh(hall.x1 + 0.4, hall.y0 + 0.8, 1.2, 1.2, 2.3);
    }
    const d = w.dock;
    sh(d.x0, d.y0 + 0.4, d.x1 - d.x0, d.y1 - d.y0 - 0.4, 0.9);
  }

  // ---------------------------------------------------------------- halls
  backWalls(hall) {
    const v = this.v, ctx = v.ctx, th = this.th, t = this.t;
    const { x0, y0, W, D } = hall;
    const h = hall.h || {};
    const lit = hall.status !== "down";
    const wall = th.wall;
    // chimneys behind the back wall
    for (const k of [0, 1]) cylinder(v, x0 + 1.2 + k * 1.1, y0 - 1.1, 0, 0.32, 3.6 + k * 0.4, th.steel, { top: rgb(th.dark) });
    box(v, x0 - 0.4, y0 - 0.4, 0, 0.4, D + 0.4, WALL_H, wall);
    box(v, x0, y0 - 0.4, 0, W, 0.4, WALL_H, wall);
    // the inner faces carry the host's signs: name, OS, cores
    v.onFaceY(y0, WALL_H);
    // windows high on the back wall
    for (let i = 0; i < Math.floor(W / 1.5); i++) {
      const wx = x0 + 0.4 + i * 1.5;
      ctx.fillStyle = lit && th.night ? rgb(th.lamp, 0.55 + 0.1 * Math.sin(t * 0.7 + i)) : rgb(shade(wall, 0.55));
      ctx.fillRect(wx, 0.25, 1.0, 0.32);
    }
    // name plate
    ctx.fillStyle = rgb(th.dark, 0.92);
    ctx.fillRect(x0 + 0.3, 0.75, Math.min(W - 0.6, 6.2), 0.62);
    ctx.font = "800 32px ui-sans-serif, system-ui, sans-serif";
    ctx.save();
    ctx.translate(x0 + 0.45, 1.08);
    ctx.scale(0.36 / 32, 0.36 / 32);
    ctx.textBaseline = "middle";
    ctx.fillStyle = lit ? th.text : rgb(th.off);
    ctx.fillText(hall.name, 0, 0);
    ctx.restore();
    // core lamps: one per core (or per few), lit amber for runners, grey for others, green when free
    const cores = h.cpus || 0;
    if (cores) {
      const n = Math.min(cores, Math.floor((W - 0.6) / 0.22));
      const per = cores / n;
      const used = h.usedCpus || 0, other = h.otherCpus || 0, free = h.freeCpus || 0;
      for (let i = 0; i < n; i++) {
        const c0 = i * per;
        let col = th.off, a = 0.35;
        if (c0 < used) { col = th.warn; a = 1; }
        else if (c0 < used + free) { col = th.ok; a = 0.75; }
        else if (c0 < used + free + other) { col = [200, 200, 210]; a = 0.6; }
        if (!lit) { col = th.off; a = 0.2; }
        ctx.fillStyle = rgb(col, a);
        ctx.fillRect(x0 + 0.3 + i * 0.22, 1.55, 0.15, 0.2);
      }
      ctx.save();
      ctx.translate(x0 + 0.3, 1.95);
      ctx.scale(0.2 / 32, 0.2 / 32);
      ctx.font = "700 32px ui-sans-serif, system-ui, sans-serif";
      ctx.textBaseline = "middle";
      ctx.fillStyle = th.dim;
      ctx.fillText(`CPU ${used.toFixed(1)} held · ${free.toFixed(1)} free · ${cores} cores`, 0, 0);
      ctx.restore();
    }
    // the left wall: what the floor is
    v.onFaceX(x0, WALL_H);
    ctx.fillStyle = rgb(th.dark, 0.9);
    ctx.fillRect(-(y0 + D) + 0.3, 0.8, Math.min(D - 0.6, 6), 0.6);
    ctx.save();
    ctx.translate(-(y0 + D) + 0.45, 1.1);
    ctx.scale(0.26 / 32, 0.26 / 32);
    ctx.font = "800 32px ui-sans-serif, system-ui, sans-serif";
    ctx.textBaseline = "middle";
    ctx.fillStyle = lit ? th.text : rgb(th.off);
    const os = h.os ? osBadge(h.os) : "?";
    ctx.fillText(`${os} · ${h.kind || ""} · ${h.memBytes ? gib(h.memBytes) + " RAM" : "no data"}`, 0, 0);
    ctx.restore();
    v.base();
  }

  // ---------------------------------------------------------------- solids
  collect(w, items) {
    const v = this.v, t = this.t;
    const add = (d, f) => items.push({ d, f });
    for (const hall of w.halls) {
      // front edges of the hall: curb, pillars, beam (in pieces so they sort with what stands near them)
      const { x0, x1, y0, y1 } = hall;
      for (let x = x0; x < x1; x += 1) {
        if (x + 1 > hall.door - 0.2 && x < hall.door + 1.8) continue;
        add(x + 0.5 + y1 + 0.1, () => box(v, x, y1, 0, 1, 0.22, 0.22, this.th.hazard, { edge: false }));
      }
      for (let y = y0; y < y1; y += 1) add(x1 + 0.1 + y + 0.5, () => box(v, x1, y, 0, 0.22, 1, 0.22, this.th.hazard, { edge: false }));
      add(x1 + y1 + 0.3, () => this.pillar(hall, x1, y1, true));
      add(x0 + y1 + 0.3, () => this.pillar(hall, x0 - 0.3, y1, false));
      add(x1 + y0 + 0.2, () => this.pillar(hall, x1, y0 - 0.3, false));
      add(hall.door + 0.9 + y1 + 0.2, () => this.door(hall));
      add(x1 + 1.0 + y0 + 1.4, () => this.silo(hall));
      // others' corner: their gear, grey, as many as their share of cpu
      if (hall.zones) {
        const tiles = hall.tiles;
        const r = rng(hash(hall.name));
        const cpuShare = (hall.h.otherCpus || 0) / Math.max(1, hall.h.cpus || 1);
        tiles.forEach((tl, i) => {
          if (!tl || tl.zone !== "others") return;
          if (r() > 0.28 + cpuShare) return;
          const [x, y] = w.tileXY(hall, i);
          const s = 0.4 + r() * 0.35, hh = 0.3 + r() * 0.7;
          const ox = x + 0.5 - s / 2 + (r() - 0.5) * 0.2, oy = y + 0.5 - s / 2 + (r() - 0.5) * 0.2;
          add(ox + oy + s, () => box(v, ox, oy, 0, s, s, hh, this.th.others, { rim: "rgba(255,255,255,0.08)" }));
        });
      }
    }
    for (const m of w.machines.values()) {
      if (!v.visible(m.x, m.y, 1, 200)) continue;
      add(m.x + m.y, () => this.machine(m));
    }
    for (const c of w.crates.values()) {
      if (c.hidden) continue;
      if (!v.visible(c.x, c.y, 0, 100)) continue;
      add(c.x + c.y + 0.01, () => this.crate(c));
    }
    for (const l of w.lines) {
      add(l.x0 + 0.5 + l.y0 - 0.25, () => this.gate(l));
      if (l.cls.overflow) add(l.x0 + 1.6 + l.y0 + 0.2, () => this.chute(l, w));
      if (l.cls.after) {
        const a = w.lines.find((k) => k.name === l.cls.after);
        if (a) add((a.x0 + l.x0) / 2 + 1 + l.y0 + 4.3, () => this.bridge(a, l));
      }
      if (l.extra > 0) add(l.x0 + 0.5 + l.y1 + 0.2, () => this.pile(l));
    }
    // lamps along the trunk
    const tr = w.trunk;
    for (let x = tr.x0 + 2; x < tr.x1 - 1; x += 7) add(x + 2.6, () => this.lamp(x, 2.6));
    add(w.dock.x0 + w.dock.y0 + 100, () => this.dockBuilding(w));
    add(w.shipArea.x0 + w.shipArea.y1, () => this.shipSign(w));
  }

  pillar(hall, x, y, beacon) {
    const v = this.v, th = this.th, t = this.t;
    box(v, x, y, 0, 0.3, 0.3, WALL_H + 0.1, th.wall);
    if (!beacon) return;
    const st = hall.status;
    const col = st === "ok" ? th.ok : st === "closed" ? th.warn : st === "down" ? th.bad : th.off;
    const blink = st === "down" ? (Math.sin(t * 8) > 0 ? 1 : 0.25) : st === "closed" ? 0.75 + 0.25 * Math.sin(t * 3) : 1;
    cylinder(v, x + 0.15, y + 0.15, WALL_H + 0.1, 0.16, 0.28, col.map((c) => c * blink), { top: rgb(shade(col, 1.3 * blink)) });
    this.glows.push([x + 0.15, y + 0.15, WALL_H + 0.3, col, 1.6 * blink]);
  }

  door(hall) {
    const v = this.v, ctx = v.ctx, th = this.th;
    const x = hall.door - 0.15, y = hall.y1, w = 1.9;
    // door frame
    box(v, x - 0.15, y, 0, 0.15, 0.25, 1.7, th.steel);
    box(v, x + w, y, 0, 0.15, 0.25, 1.7, th.steel);
    box(v, x - 0.15, y, 1.55, w + 0.3, 0.25, 0.22, th.steel);
    const target = hall.status === "closed" ? 1 : 0;
    hall.shutter = hall.shutter === undefined ? target : hall.shutter + (target - hall.shutter) * 0.06;
    if (!this.opts.motion) hall.shutter = target;
    if (hall.shutter > 0.02) {
      // a rolled shutter, striped, saying why
      const hh = 1.5 * hall.shutter;
      const A = v.P(x, y + 0.2, 1.55), B = v.P(x + w, y + 0.2, 1.55), C = v.P(x + w, y + 0.2, 1.55 - hh), D = v.P(x, y + 0.2, 1.55 - hh);
      poly(ctx, [A, B, C, D]);
      ctx.fillStyle = rgb(shade(th.steel, 0.85));
      ctx.fill();
      v.onFaceY(y + 0.2, 1.55);
      ctx.save();
      ctx.beginPath(); ctx.rect(x, 0, w, hh); ctx.clip();
      for (let i = 0; i < 14; i++) {
        ctx.fillStyle = i % 2 ? rgb(th.hazard, 0.9) : "rgba(20,20,20,0.9)";
        ctx.beginPath();
        ctx.moveTo(x + i * 0.3 - 0.6, hh); ctx.lineTo(x + i * 0.3 - 0.3, hh); ctx.lineTo(x + i * 0.3 + 0.2, hh - 0.35); ctx.lineTo(x + i * 0.3 - 0.1, hh - 0.35);
        ctx.fill();
      }
      for (let k = 0.12; k < hh - 0.4; k += 0.12) { ctx.fillStyle = "rgba(0,0,0,0.18)"; ctx.fillRect(x, k, w, 0.025); }
      ctx.restore();
      v.base();
    }
  }

  silo(hall) {
    const v = this.v, th = this.th, h = hall.h || {};
    const x = hall.x1 + 1.0, y = hall.y0 + 1.4;
    // a pipe from the hall: this tank is the hall's disk
    box(v, hall.x1 + 0.2, y - 0.1, 1.55, 0.5, 0.2, 0.2, th.steel);
    const pct = h.diskFreePct;
    const L = (this.world.state && this.world.state.limits) || {};
    const low = pct !== null && pct !== undefined && pct < (L.diskMinFree || 10);
    const used = pct === null || pct === undefined ? 0 : 1 - pct / 100;
    cylinder(v, x, y, 0, 0.62, 2.3, th.steel, { fill: used, liquid: low ? rgb(th.bad, 0.9) : rgb(shade(th.glass, 0.8), 0.85) });
    if (low) {
      const k = 0.5 + 0.5 * Math.sin(this.t * 5);
      cylinder(v, x, y, 2.3, 0.14, 0.22, th.bad.map((c) => c * (0.5 + k / 2)));
      this.glows.push([x, y, 2.5, th.bad, 1.4 * k]);
    }
    hall.siloAt = [x, y, 2.4];
  }

  lamp(x, y) {
    const v = this.v, th = this.th;
    box(v, x - 0.06, y - 0.06, 0, 0.12, 0.12, 2.6, th.dark, { edge: false });
    box(v, x - 0.25, y - 0.12, 2.6, 0.5, 0.24, 0.1, th.dark);
    this.pools.push([x, y - 0.6, th.lamp, 3.4, 0.33]);
  }

  // ---------------------------------------------------------------- machines
  machine(m) {
    const v = this.v, ctx = v.ctx, th = this.th, t = this.t;
    const r = m.r, s = m.s || 1;
    const born = clamp((t - m.born) / 0.8, 0, 1);
    const dying = m.dying ? clamp(1 - (t - m.dying) / 1.0, 0, 1) : 1;
    const k = Math.min(born, dying);
    if (k <= 0) return;
    const hall = m.hall;
    const down = hall.status === "down" || hall.status === "unknown";
    const x = m.x - s / 2, y = m.y - s / 2;
    const cls = m.cls ? m.cls.color : th.steel;
    const body = mix(th.steel, cls, 0.55);
    const starting = r.state === "starting";
    const busy = r.state === "busy";
    box(v, x, y, 0, s, s, 0.12, th.dark);
    const bw = s * 0.8, bx = x + s * 0.05, by = y + s * 0.05;
    const bh = (0.5 + 0.34 * s) * k;
    if (starting) {
      // a frame going up: scaffolding, and the body rising inside it
      const f = 0.5 + 0.5 * Math.sin(t * 2 + m.phase);
      box(v, bx, by, 0.12, bw, bw, bh * (0.35 + 0.25 * f), body, { tk: 1.05 });
      ctx.strokeStyle = rgb(th.warn, 0.9);
      ctx.lineWidth = Math.max(1, v.cam.z * 1.2);
      for (const [px, py] of [[bx, by + bw], [bx + bw, by + bw], [bx + bw, by]]) {
        const a = v.P(px, py, 0.12), b = v.P(px, py, 0.12 + bh);
        ctx.beginPath(); ctx.moveTo(a[0], a[1]); ctx.lineTo(b[0], b[1]); ctx.stroke();
      }
    } else {
      box(v, bx, by, 0.12, bw, bw, bh, down ? shade(body, 0.6) : body);
      // class band and plate on the lit face
      v.onFaceY(by + bw, 0.12 + bh);
      ctx.fillStyle = rgb(shade(cls, down ? 0.5 : 1.05));
      ctx.fillRect(bx, bh * 0.12, bw, bh * 0.14);
      ctx.fillStyle = "rgba(0,0,0,0.25)";
      ctx.fillRect(bx + bw * 0.12, bh * 0.42, bw * 0.5, bh * 0.36);
      if (v.cam.z * s > 0.9) {
        ctx.save();
        ctx.translate(bx + bw * 0.15, bh * 0.6);
        ctx.scale(0.17 * Math.min(1.4, s) / 32, 0.17 * Math.min(1.4, s) / 32);
        ctx.font = "800 32px ui-monospace, SFMono-Regular, Menlo, monospace";
        ctx.textBaseline = "middle";
        ctx.fillStyle = rgb(shade(cls, 1.35));
        ctx.fillText((r.slot ? "#" + r.slot + " " : "") + r.class.slice(0, 10), 0, 0);
        ctx.restore();
      }
      // the right face: vents
      v.onFaceX(bx + bw, 0.12 + bh);
      ctx.fillStyle = "rgba(0,0,0,0.3)";
      for (let i = 0; i < 3; i++) ctx.fillRect(-(by + bw) + bw * 0.15, bh * (0.25 + i * 0.2), bw * 0.7, bh * 0.08);
      v.base();
      // gears on top, turning with the job's cpu
      const top = 0.12 + bh;
      const speed = down ? 0 : busy ? clamp(r.cpuNow || 0, 0, 10) * 1.3 + 0.15 : 0;
      if (this.opts.motion) m.gear += speed * (this.dt || 0.016);
      v.onGround(top);
      const gx = bx + bw * 0.42, gy = by + bw * 0.42, gr = bw * 0.27;
      gear(ctx, gx, gy, gr, 10, m.gear, down ? th.off : mix(th.steel, [210, 180, 110], 0.55));
      gear(ctx, gx + gr * 1.62, gy - gr * 0.05, gr * 0.62, 7, -m.gear * 1.6 + 0.3, down ? th.off : mix(th.steel, [200, 200, 210], 0.4));
      v.base();
    }
    // the tank: memory now against what it holds
    if (!starting) {
      const held = r.heldMem || r.estMem || 1;
      const lv = r.memBytes ? r.memBytes / held : 0;
      const over = r.memBytes > r.estMem && r.estMem > 0;
      const near = r.memLimit && r.memBytes > 0.9 * r.memLimit;
      const liquid = near ? th.bad : over ? th.warn : [64, 200, 190];
      cylinder(v, x + s * 0.88, y + s * 0.88, 0.12, 0.13 + 0.06 * s, (0.45 + 0.3 * s) * k, mix(th.steel, [200, 220, 230], 0.3), { fill: lv, liquid: rgb(liquid, down ? 0.35 : 0.9) });
      if (over && !down && this.opts.motion && Math.random() < 0.05) this.spawn("steam", x + s * 0.88, y + s * 0.88, 0.6 + 0.3 * s);
    }
    // status lamp
    const lampCol = down ? th.off : starting ? th.warn : busy ? th.ok : [90, 180, 240];
    const blink = starting ? (Math.sin(t * 6 + m.phase) > 0 ? 1 : 0.3) : 1;
    const lz = 0.12 + bh + 0.02;
    cylinder(v, x + s * 0.12, y + s * 0.12, lz, 0.07, 0.12, lampCol.map((c) => c * blink));
    if (!down) this.glows.push([x + s * 0.12, y + s * 0.12, lz + 0.12, lampCol, 0.9 * blink]);
    // exhaust: puffs as fast as the job burns cpu
    if (busy && !down && this.opts.motion) {
      m.puff += (r.cpuNow || 0) * (this.dt || 0.016) * 0.8;
      if (m.puff > 1) { m.puff = 0; this.spawn("smoke", x + s * 0.2, y + s * 0.55, 0.12 + bh + 0.1); }
    }
    this.hit("runner", r.name, x, y, s, s, 0.12 + bh + 0.3);
    m.top = [m.x, m.y, 0.12 + bh + 0.35];
  }

  // ---------------------------------------------------------------- crates
  crate(c) {
    const v = this.v, ctx = v.ctx, th = this.th, t = this.t;
    let a = 1;
    if (c.mode === "gone") a = clamp(1 - (t - c.t0) / 0.5, 0, 1);
    const born = clamp((t - c.born) / 0.4, 0, 1);
    a *= born;
    if (a <= 0) return;
    const memG = (c.mem || (c.j && c.j.estMem) || 4 * GiB) / GiB;
    let s = clamp(0.34 + 0.1 * Math.log2(1 + memG), 0.32, 0.84);
    if (c.absorbed) s *= a;
    let col;
    if (c.result) {
      col = th[RESULT[c.result]] || th.off;
      col = mix(th.crate, col, 0.72);
    } else {
      const L = (this.world.state && this.world.state.limits) || {};
      const alert = L.waitAlertSeconds || 600;
      const waited = (c.j && c.j.waitedSeconds) || 0;
      const k = clamp(waited / alert, 0, 1.4);
      col = k < 0.5 ? mix(th.crate, th.warm, k * 2 * 0.6) : mix(mix(th.crate, th.warm, 0.6), th.hot, clamp((k - 0.5) * 2, 0, 1));
      if (k >= 1) {
        const p = 0.6 + 0.4 * Math.sin(t * 4 + hash(c.key) % 7);
        this.glows.push([c.x, c.y, 0.4, th.hot, 0.9 * p]);
      }
    }
    const x = c.x - s / 2, y = c.y - s / 2;
    const z = (c.z || BELT_Z) + (c.mode === "back" ? 0.62 : 0) + (c.lift || 0);
    const exp = c.j && c.j.expedited;
    if (exp) {
      const p = 0.7 + 0.3 * Math.sin(t * 5);
      this.glows.push([c.x, c.y, z + s * 0.5, [255, 122, 217], 1.3 * p]);
      if (c.lift) this.glows.push([c.x, c.y, 0.05, [255, 122, 217], 0.8]);
    }
    ctx.globalAlpha = a;
    box(v, x, y, z, s, s, s * 0.85, col, { rim: "rgba(255,255,255,0.22)" });
    // tape across the top, and the priority band
    v.onGround(z + s * 0.85);
    ctx.fillStyle = "rgba(0,0,0,0.16)";
    ctx.fillRect(x + s * 0.42, y, s * 0.16, s);
    if (exp) {
      ctx.fillStyle = "rgba(255,122,217,0.95)";
      ctx.fillRect(x, y, s, s);
      ctx.fillStyle = "rgba(255,255,255,0.9)";
      for (let k = 0; k < 3; k++) {
        const cy = y + s * (0.22 + k * 0.28), q = s * 0.16;
        ctx.beginPath(); ctx.moveTo(x + s * 0.5 - q * 1.6, cy + q); ctx.lineTo(x + s * 0.5, cy - q * 0.4); ctx.lineTo(x + s * 0.5 + q * 1.6, cy + q); ctx.lineTo(x + s * 0.5 + q * 1.6, cy + q * 1.5); ctx.lineTo(x + s * 0.5, cy + q * 0.1); ctx.lineTo(x + s * 0.5 - q * 1.6, cy + q * 1.5); ctx.fill();
      }
    } else if (c.level === "high" || c.level === "low") {
      ctx.fillStyle = c.level === "high" ? "rgba(255,214,64,0.95)" : "rgba(110,140,200,0.95)";
      ctx.fillRect(x, y + s * 0.38, s, s * 0.24);
      ctx.fillStyle = "rgba(0,0,0,0.55)";
      ctx.beginPath();
      const cx = x + s * 0.5, cy = y + s * 0.5, q = s * 0.16;
      if (c.level === "high") { ctx.moveTo(cx - q * 1.4, cy + q); ctx.lineTo(cx, cy - q); ctx.lineTo(cx + q * 1.4, cy + q); }
      else { ctx.moveTo(cx - q * 1.4, cy - q); ctx.lineTo(cx, cy + q); ctx.lineTo(cx + q * 1.4, cy - q); }
      ctx.fill();
    }
    if (c.result === "oom" || c.result === "fail") {
      ctx.strokeStyle = "rgba(0,0,0,0.6)";
      ctx.lineWidth = 0.04;
      ctx.beginPath(); ctx.moveTo(x + s * 0.2, y + s * 0.2); ctx.lineTo(x + s * 0.8, y + s * 0.8); ctx.moveTo(x + s * 0.8, y + s * 0.2); ctx.lineTo(x + s * 0.2, y + s * 0.8); ctx.stroke();
    }
    v.base();
    ctx.globalAlpha = 1;
    if (c.mode === "queue" || c.mode === "ship") this.hit(c.mode === "ship" ? "done" : "job", c.key, x, y, s, s, z + s);
    c.top = [c.x, c.y, z + s * 0.9];
  }

  pile(l) {
    // more jobs than the belt shows: a stack at the dock door, with its count
    const v = this.v, th = this.th;
    const x = l.x0 + 0.1, y = l.y1 - 0.3;
    const n = Math.min(4, Math.ceil(l.extra / 8));
    for (let i = 0; i < n; i++) box(v, x + (i % 2) * 0.42, y - 0.1, Math.floor(i / 2) * 0.4, 0.4, 0.4, 0.38, mix(th.crate, th.warm, 0.3));
    l.pileAt = [x + 0.4, y, 1.0];
  }

  gate(l) {
    const v = this.v, th = this.th, t = this.t;
    const c = l.cls, x = l.x0, y = l.y0 - 0.25;
    const w = l.w;
    box(v, x - 0.25, y, 0, 0.16, 0.16, 1.55, th.steel);
    box(v, x + 1.09, y, 0, 0.16, 0.16, 1.55, th.steel);
    box(v, x - 0.25, y, 1.45, 1.5, 0.16, 0.24, c.color);
    // the order lamp: lit with the class's place in line
    const col = !w ? th.ok : w.priority && /high/.test(w.priority) ? [255, 214, 64] : w.priority && /low/.test(w.priority) ? [110, 140, 200] : th.warn;
    const blink = w && w.order === 1 ? 0.75 + 0.25 * Math.sin(t * 4) : 1;
    cylinder(v, x + 0.5, y + 0.08, 1.69, 0.13, 0.16, col.map((q) => q * blink));
    this.glows.push([x + 0.5, y + 0.08, 1.85, col, 1.1 * blink]);
    // high-only mode: a boom comes down on every line whose next job is not high or expedited
    const mode = this.opts.mode;
    if (mode && mode.name === "high-only") {
      const front = [...this.world.crates.values()].find((k) => k.mode === "queue" && k.line === l && k.idx === 0);
      const pass = front && front.j && (front.j.expedited || front.j.level === "high" || front.j.level === "expedite");
      const ang = pass ? 1.25 : 0;
      const ctx = v.ctx;
      const a = v.P(x - 0.45, y + 0.35, 0.75);
      const len = 1.9;
      const b = pass ? v.P(x - 0.45, y + 0.35, 0.75 + len) : v.P(x - 0.45 + len, y + 0.35, 0.75);
      ctx.lineCap = "round";
      ctx.lineWidth = Math.max(2, 7 * v.cam.z);
      ctx.strokeStyle = "#f4f4f4";
      ctx.beginPath(); ctx.moveTo(a[0], a[1]); ctx.lineTo(b[0], b[1]); ctx.stroke();
      ctx.setLineDash([Math.max(2, 9 * v.cam.z), Math.max(2, 9 * v.cam.z)]);
      ctx.strokeStyle = "#e0302a";
      ctx.stroke();
      ctx.setLineDash([]);
      ctx.lineCap = "butt";
      box(v, x - 0.62, y + 0.2, 0, 0.3, 0.3, 0.8, th.warn);
      const rot = 0.6 + 0.4 * Math.sin(t * 6 + l.x0);
      cylinder(v, x - 0.47, y + 0.35, 0.8, 0.12, 0.16, th.warn.map((q) => q * rot));
      this.glows.push([x - 0.47, y + 0.35, 1.0, th.warn, 1.4 * rot]);
      void ang;
    }
    l.gateAt = [x + 0.5, y, 2.0];
    this.hit("class", l.name, x - 0.25, y - 0.2, 1.5, 1.2, 1.7);
  }

  chute(l, w) {
    // overflow: the line hands back what it cannot place, to other runners of its labels (GitHub)
    const v = this.v, th = this.th, ctx = v.ctx, t = this.t;
    const x = l.x0 + 1.38, y0 = l.y0 + 0.1, y1 = l.y1 + 0.6;
    for (let y = y0 + 0.3; y < y1; y += 2.6) box(v, x + 0.1, y, 0, 0.12, 0.12, 0.5, th.dark, { edge: false });
    box(v, x, y0, 0.5, 0.36, y1 - y0, 0.18, [130, 110, 200]);
    v.onGround(0.68);
    ctx.fillStyle = "rgba(255,255,255,0.5)";
    const off = this.opts.motion ? (t * 1.2) % 1.2 : 0;
    for (let y = y0 + off; y < y1 - 0.4; y += 1.2) {
      ctx.beginPath(); ctx.moveTo(x + 0.08, y + 0.1); ctx.lineTo(x + 0.18, y + 0.3); ctx.lineTo(x + 0.28, y + 0.1); ctx.lineTo(x + 0.18, y + 0.2); ctx.fill();
    }
    v.base();
    l.chuteAt = [x + 0.18, y0 + 1.2, 0.9];
  }

  bridge(a, b) {
    // after: b takes jobs only while a has no room. A raised belt from a to b, running when it does.
    const v = this.v, ctx = v.ctx, th = this.th, t = this.t;
    const y = a.y0 + 4.2, xa = a.x0 + 1.15, xb = b.x0 - 0.15;
    const on = b.ov && b.ov.take > 0;
    for (const px of [xa + 0.2, xb - 0.3]) box(v, px, y + 0.1, 0, 0.1, 0.1, 0.6, th.dark, { edge: false });
    box(v, xa, y, 0.6, xb - xa, 0.42, 0.1, on ? mix(th.steel, b.cls.color, 0.5) : th.steel);
    v.onGround(0.7);
    ctx.fillStyle = on ? "rgba(255,255,255,0.7)" : "rgba(255,255,255,0.18)";
    const off = on && this.opts.motion ? (t * 1.0) % 0.8 : 0;
    for (let x = xa + 0.2 + off; x < xb - 0.3; x += 0.8) {
      ctx.beginPath(); ctx.moveTo(x, y + 0.08); ctx.lineTo(x + 0.2, y + 0.21); ctx.lineTo(x, y + 0.34); ctx.lineTo(x + 0.1, y + 0.21); ctx.fill();
    }
    v.base();
    b.bridgeAt = [(xa + xb) / 2, y + 0.2, 0.9];
  }

  dockBuilding(w) {
    const v = this.v, ctx = v.ctx, th = this.th, t = this.t;
    const d = w.dock;
    const col = mix(th.wall, [70, 84, 110], 0.35);
    box(v, d.x0, d.y0 + 0.4, 0, d.x1 - d.x0, d.y1 - d.y0 - 0.4, 0.95, col);
    // the roof: seams, the painted name
    v.onGround(0.95);
    ctx.fillStyle = "rgba(0,0,0,0.16)";
    for (let x = d.x0 + 0.7; x < d.x1; x += 1.4) ctx.fillRect(x, d.y0 + 0.5, 0.05, d.y1 - d.y0 - 0.6);
    const size = Math.min(1.0, (d.x1 - d.x0) / 15);
    this.groundText("GITHUB  ·  JOBS IN", d.x0 + 1.0, (d.y0 + d.y1) / 2 + 0.35, size, rgb(th.paint, 0.92), 0, "900");
    v.base();
    // roof vents, turning slowly
    for (let x = d.x0 + 1.2; x < d.x1 - 1; x += 3.1) {
      cylinder(v, x, d.y0 + 1.1, 0.95, 0.22, 0.3, th.steel, { top: rgb(th.dark) });
      v.onGround(1.27);
      ctx.save(); ctx.translate(x, d.y0 + 1.1); ctx.rotate(this.opts.motion ? t * 2 + x : x);
      ctx.fillStyle = rgb(shade(th.steel, 1.2));
      for (let k = 0; k < 4; k++) { ctx.rotate(Math.PI / 2); ctx.fillRect(-0.03, 0, 0.06, 0.17); }
      ctx.restore();
      v.base();
    }
    // where each line comes in: a lit door in the class's colour on the dock's back
    v.onGround(0.02);
    for (const l of w.lines) {
      ctx.fillStyle = rgb(l.cls.color, 0.85);
      ctx.fillRect(l.x0 - 0.1, d.y0 + 0.28, 1.2, 0.14);
    }
    v.base();
    for (const l of w.lines) this.glows.push([l.x0 + 0.5, d.y0 + 0.3, 0.4, l.cls.color, 0.9]);
  }

  shipSign(w) {
    const v = this.v, th = this.th;
    const s = w.shipArea;
    box(v, s.x1 - 0.5, s.y0 + 0.3, 0, 0.2, 0.2, 1.8, th.steel);
    box(v, s.x1 - 0.5, s.y0 + 0.3, 1.8, 0.2, 2.6, 0.7, th.dark);
    w.shipSignAt = [s.x1 - 0.4, s.y0 + 1.6, 2.6];
  }

  // ---------------------------------------------------------------- light
  lights(w) {
    const v = this.v, ctx = v.ctx;
    ctx.save();
    ctx.globalCompositeOperation = "lighter";
    v.onGround();
    for (const hall of w.halls) {
      if (hall.status === "down") continue;
      const dim = hall.status === "unknown" ? 0.4 : 1;
      for (let i = 0; i < Math.ceil(hall.W / 4.5); i++) {
        const x = hall.x0 + 2.2 + i * 4.5, y = hall.y0 + hall.D / 2;
        const g = ctx.createRadialGradient(x, y, 0, x, y, 3.6);
        g.addColorStop(0, `rgba(255,214,160,${0.17 * dim})`);
        g.addColorStop(1, "rgba(255,214,160,0)");
        ctx.fillStyle = g;
        ctx.fillRect(x - 3.6, y - 3.6, 7.2, 7.2);
      }
    }
    for (const [x, y, col, r, a] of this.pools) {
      const g = ctx.createRadialGradient(x, y, 0, x, y, r);
      g.addColorStop(0, rgb(col, a));
      g.addColorStop(1, rgb(col, 0));
      ctx.fillStyle = g;
      ctx.fillRect(x - r, y - r, 2 * r, 2 * r);
    }
    v.base();
    // small glows in screen space: lamps, hot crates, beacons
    const k = v.cam.z;
    for (const [x, y, z, col, a] of this.glows) {
      const [sx, sy] = v.P(x, y, z);
      const r = 26 * k * Math.max(0.6, a);
      const g = ctx.createRadialGradient(sx, sy, 0, sx, sy, r);
      g.addColorStop(0, rgb(col, 0.45 * Math.min(1, a)));
      g.addColorStop(1, rgb(col, 0));
      ctx.fillStyle = g;
      ctx.fillRect(sx - r, sy - r, 2 * r, 2 * r);
    }
    ctx.restore();
  }

  // ---------------------------------------------------------------- smoke, sparks
  spawn(kind, x, y, z, n = 1) {
    if (this.fx.length > 500) return;
    for (let i = 0; i < n; i++) {
      const a = Math.random() * Math.PI * 2;
      const sp = kind === "spark" ? 1.5 + Math.random() * 2.5 : 0.1;
      this.fx.push({ kind, x, y, z, vx: Math.cos(a) * sp * 0.3, vy: Math.sin(a) * sp * 0.3, vz: kind === "spark" ? 1 + Math.random() * 2 : 0.5 + Math.random() * 0.3, t: this.t, life: kind === "spark" ? 0.8 + Math.random() * 0.5 : kind === "steam" ? 1.2 : 2.6, r: kind === "smoke" || kind === "steam" ? 0.12 : 0.04 });
    }
  }

  effects(w, t) {
    const v = this.v, ctx = v.ctx, th = this.th, dt = this.dt || 0.016;
    // chimneys of hosts under pressure, and bursts for out-of-memory kills
    if (this.opts.motion) {
      for (const hall of w.halls) {
        const heat = this.heat(hall.h || {});
        if (heat > 0.15 && Math.random() < heat * 0.25) this.spawn("smoke", hall.x0 + 1.2 + (Math.random() < 0.5 ? 0 : 1.1), hall.y0 - 1.1, 4.0);
      }
    }
    const inc = (w.state && w.state.incidents) || [];
    const recent = ((w.state && w.state.recentIncidents) || []).filter((i) => since(i.openedAt) < 90);
    for (const i of [...inc, ...recent]) {
      if (i.kind !== "out-of-memory" && i.kind !== "runner-died") continue;
      const key = i.key + i.openedAt;
      const m = w.machines.get(i.subject);
      const c = w.crates.get("s:" + i.subject);
      const at = m ? [m.x, m.y, 1.2] : c ? [c.x, c.y, 0.6] : null;
      if (!at) continue;
      if (!this.bursts.has(key)) {
        this.bursts.add(key);
        if (this.opts.motion) { this.spawn("spark", at[0], at[1], at[2], 40); this.spawn("smoke", at[0], at[1], at[2], 8); }
      }
      if (this.opts.motion && Math.random() < 0.15) this.spawn("smoke", at[0], at[1], at[2]);
    }
    this.fx = this.fx.filter((p) => t - p.t < p.life);
    for (const p of this.fx) {
      const age = (t - p.t) / p.life;
      if (this.opts.motion) {
        p.x += p.vx * dt; p.y += p.vy * dt; p.z += p.vz * dt;
        if (p.kind === "spark") p.vz -= 4 * dt;
        else { p.x += 0.25 * dt; p.y -= 0.1 * dt; }
      }
      const [sx, sy] = v.P(p.x, p.y, p.z);
      const k = v.cam.z;
      if (p.kind === "spark") {
        ctx.fillStyle = `rgba(255,${180 - age * 120 | 0},60,${1 - age})`;
        ctx.fillRect(sx - 1.5 * k, sy - 1.5 * k, 3 * k, 3 * k);
      } else {
        const r = (p.r + age * (p.kind === "steam" ? 0.25 : 0.5)) * TW * k;
        const base = p.kind === "steam" ? "220,230,240" : th.night ? "150,155,165" : "120,120,125";
        ctx.fillStyle = `rgba(${base},${(1 - age) * (p.kind === "steam" ? 0.25 : 0.32)})`;
        ctx.beginPath(); ctx.arc(sx, sy, r, 0, Math.PI * 2); ctx.fill();
      }
    }
  }

  // ---------------------------------------------------------------- labels
  labels(w) {
    const v = this.v, ctx = v.ctx, th = this.th, z = v.cam.z;
    const sel = this.opts.selected || {};
    const tags = [];
    const isSel = (k, id) => sel.kind === k && sel.id === id;
    const narrow = v.w < 640; // a phone: hosts say only what is free, and idle lines keep quiet
    for (const hall of w.halls) {
      const h = hall.h || {};
      const [sx, sy] = v.P(hall.x0 + hall.W / 2, hall.y0 - 0.4, WALL_H + 1.4);
      const st = hall.status === "ok" ? "" : hall.status === "closed" ? h.closed : hall.status === "down" ? "no signal" + (h.seenSeconds ? " · " + dur(h.seenSeconds) : "") : "no data";
      const col = hall.status === "ok" ? th.ok : hall.status === "closed" ? th.warn : hall.status === "down" ? th.bad : th.off;
      const line2 = h.memBytes ? `${(h.usedCpus || 0).toFixed(1)}/${h.cpus} cpu · ${gib(h.usedMem || 0)} held · ${gib(h.freeMem || 0)} free` : "";
      const sub = st ? st + (line2 ? " · " + gib(h.freeMem || 0) + " free" : "") : narrow && line2 ? `${gib(h.freeMem || 0)} free` : line2;
      tags.push({ x: sx, y: sy, title: hall.name, sub, dot: col, big: true, kind: "host", id: hall.name, sel: isSel("host", hall.name), prio: isSel("host", hall.name) ? 0 : 1 });
      if (hall.siloAt && h.diskFreePct !== null && h.diskFreePct !== undefined && h.diskFreePct < (((w.state || {}).limits || {}).diskMinFree || 10) + 5) {
        const [dx, dy] = v.P(...hall.siloAt);
        tags.push({ x: dx, y: dy - 4, title: `${hall.name}: disk ${h.diskFreePct.toFixed(0)}% free`, dot: th.bad, small: true, kind: "host", id: hall.name, prio: 2 });
      }
    }
    for (const l of w.lines) {
      const ww = l.w;
      if (narrow && !ww && !l.ov && !isSel("class", l.name)) continue;
      let sub = ww ? `${ww.jobs} waiting · #${ww.order || "-"} in line` : "nothing waiting";
      if (l.ov) sub = `overflow · takes ${l.ov.take} · holds ${l.ov.held}` + (ww ? ` · ${ww.jobs} waiting` : "");
      // on the dock roof, past the belt's end: a long queue is not covered by its name
      const [sx, sy] = v.P(l.x0 + 0.5, l.y1 + 0.9, 1.0);
      tags.push({ x: sx, y: sy, title: l.name, sub, dot: l.cls.color, kind: "class", id: l.name, sel: isSel("class", l.name), prio: isSel("class", l.name) ? 0 : 3 });
      // the front crate says why it waits; from afar only the line that is next
      // (or one waiting too long, or the selected one) says it
      const speaks = z > 0.75 || (ww && (ww.order === 1 || ww.oldestSeconds > ((w.state.limits || {}).waitAlertSeconds || 600))) || isSel("class", l.name);
      if (ww && ww.reason && z > 0.3 && speaks) {
        const front = [...w.crates.values()].find((c) => c.mode === "queue" && c.line === l && c.idx === 0);
        const at = front && front.top ? v.P(...front.top) : v.P(l.x0 + 0.5, l.y0 + 0.7, 1);
        tags.push({ x: at[0], y: at[1] - 6, bubble: shortReason(ww.reason), hot: ww.oldestSeconds > ((w.state.limits || {}).waitAlertSeconds || 600), kind: "class", id: l.name, prio: 2 });
      }
      if (l.extra > 0 && l.pileAt) {
        const [px, py] = v.P(...l.pileAt);
        tags.push({ x: px, y: py, badge: "+" + l.extra, prio: 9 });
      }
    }
    for (const m of w.machines.values()) {
      if (!m.top || m.dying) continue;
      if (z <= 0.8 && !isSel("runner", m.name)) continue;
      const [sx, sy] = v.P(...m.top);
      if (sx < -80 || sy < -40 || sx > v.w + 80 || sy > v.h + 40) continue;
      const r = m.r;
      tags.push({ x: sx, y: sy - 6, title: r.job || (r.state === "starting" ? "starting…" : "waiting for its job"), sub: `${(r.cpuNow || 0).toFixed(1)}cpu · ${gib(r.memBytes || 0)} / ${gib(r.heldMem || r.estMem)}`, dot: m.cls.color, small: true, kind: "runner", id: m.name, sel: isSel("runner", m.name), prio: isSel("runner", m.name) ? 0 : 4 });
    }
    if (this.opts.prioView) {
      for (const c of w.crates.values()) {
        if (c.mode !== "queue" || c.hidden || !c.top || !c.j) continue;
        const [sx, sy] = v.P(...c.top);
        const lvl = c.j.expedited ? "expedite" : c.j.level || "normal";
        tags.push({ x: sx, y: sy - 4, num: c.j.position || c.idx + 1, lvl, kind: "job", id: c.key, prio: 9 });
      }
    }
    if (w.shipSignAt) {
      const [sx, sy] = v.P(...w.shipSignAt);
      const done = [...w.crates.values()].filter((c) => c.mode === "ship");
      const n = (r) => done.filter((c) => c.result === r).length;
      tags.push({ x: sx, y: sy, title: "shipped", sub: `${n("ok")} ok · ${n("fail") + n("oom")} failed · ${n("cancel")} cancelled`, dot: th.ok, prio: 3 });
    }
    // place: the important first; a label that would cover one already placed moves up, or is left out
    ctx.textBaseline = "middle";
    tags.sort((a, b) => a.prio - b.prio);
    const placed = [];
    const hit = (r) => placed.some((q) => r.x0 < q.x1 && r.x1 > q.x0 && r.y0 < q.y1 && r.y1 > q.y0);
    const shown = [];
    for (const tg of tags) {
      if (tg.prio >= 9) { shown.push(tg); continue; }
      let r = this.measure(tg);
      let ok = !hit(r);
      for (let k = 0; !ok && k < 3 && tg.prio <= 3; k++) {
        tg.y -= r.y1 - r.y0 + 3;
        r = this.measure(tg);
        ok = !hit(r);
      }
      if (!ok && tg.prio > 2) continue;
      placed.push(r);
      shown.push(tg);
    }
    for (let i = shown.length - 1; i >= 0; i--) this.tag(shown[i]);
  }

  measure(tg) {
    const ctx = this.v.ctx;
    if (tg.bubble) {
      ctx.font = "600 11.5px ui-sans-serif, system-ui, sans-serif";
      const w = Math.min(300, ctx.measureText(tg.bubble).width + 16), h = 22;
      return { x0: tg.x - w / 2, y0: tg.y - h - 8, x1: tg.x + w / 2, y1: tg.y, w, h };
    }
    const big = tg.big, small = tg.small;
    ctx.font = `${big ? 800 : 700} ${big ? 14 : small ? 11 : 12.5}px ui-sans-serif, system-ui, sans-serif`;
    const tw = ctx.measureText(tg.title).width;
    ctx.font = `500 ${small ? 10 : 11}px ui-sans-serif, system-ui, sans-serif`;
    const sw = tg.sub ? ctx.measureText(tg.sub).width : 0;
    const w = Math.max(tw + 22, sw + 14), h = tg.sub ? (big ? 38 : small ? 30 : 34) : 22;
    return { x0: Math.round(tg.x - w / 2), y0: Math.round(tg.y - h), x1: Math.round(tg.x + w / 2), y1: Math.round(tg.y), w, h };
  }

  tag(tg) {
    const ctx = this.v.ctx, th = this.th;
    if (tg.num !== undefined) {
      const col = { expedite: "#ff7ad9", high: "#ffd640", low: "#7f9ad0", normal: th.night ? "#e9eef4" : "#14191f" }[tg.lvl];
      const r = 11;
      ctx.fillStyle = th.night ? "rgba(10,14,20,0.92)" : "rgba(255,255,255,0.95)";
      ctx.beginPath(); ctx.arc(tg.x, tg.y - r, r, 0, Math.PI * 2); ctx.fill();
      ctx.lineWidth = 2.5; ctx.strokeStyle = col; ctx.stroke();
      ctx.fillStyle = col; ctx.font = "800 11px ui-sans-serif, system-ui, sans-serif"; ctx.textAlign = "center";
      ctx.fillText(String(tg.num), tg.x, tg.y - r + 0.5); ctx.textAlign = "left";
      this.hits.push({ kind: tg.kind, id: tg.id, x0: tg.x - r, y0: tg.y - 2 * r, x1: tg.x + r, y1: tg.y, z: 9 });
      return;
    }
    if (tg.badge) {
      ctx.font = "700 11px ui-sans-serif, system-ui, sans-serif";
      const w = ctx.measureText(tg.badge).width + 10;
      ctx.fillStyle = rgb(th.warn);
      roundRect(ctx, tg.x - w / 2, tg.y - 9, w, 18, 9); ctx.fill();
      ctx.fillStyle = "#111"; ctx.textAlign = "center"; ctx.fillText(tg.badge, tg.x, tg.y); ctx.textAlign = "left";
      return;
    }
    const r = this.measure(tg);
    if (tg.bubble) {
      const { x0: x, y0: y, w, h } = r;
      ctx.fillStyle = tg.hot ? "rgba(150,24,18,0.95)" : th.night ? "rgba(250,246,236,0.96)" : "rgba(255,255,255,0.97)";
      roundRect(ctx, x, y, w, h, 6); ctx.fill();
      ctx.beginPath(); ctx.moveTo(tg.x - 6, y + h); ctx.lineTo(tg.x, y + h + 7); ctx.lineTo(tg.x + 6, y + h); ctx.fill();
      ctx.fillStyle = tg.hot ? "#fff" : "#1a1d22";
      ctx.font = "600 11.5px ui-sans-serif, system-ui, sans-serif";
      ctx.save(); ctx.beginPath(); ctx.rect(x, y, w, h); ctx.clip();
      ctx.fillText(tg.bubble, x + 8, y + h / 2 + 0.5);
      ctx.restore();
      this.hits.push({ kind: tg.kind, id: tg.id, x0: x, y0: y, x1: x + w, y1: y + h, z: 9 });
      return;
    }
    const big = tg.big, small = tg.small;
    const { x0: x, y0: y, w, h } = r;
    ctx.fillStyle = th.label;
    roundRect(ctx, x, y, w, h, 6); ctx.fill();
    ctx.strokeStyle = tg.sel ? rgb(th.paint) : th.labelLine;
    ctx.lineWidth = tg.sel ? 2 : 1;
    ctx.stroke();
    if (tg.dot) {
      ctx.fillStyle = rgb(tg.dot);
      ctx.beginPath(); ctx.arc(x + 10, y + (tg.sub ? 12 : h / 2), 4, 0, Math.PI * 2); ctx.fill();
    }
    ctx.fillStyle = th.text;
    ctx.font = `${big ? 800 : 700} ${big ? 14 : small ? 11 : 12.5}px ui-sans-serif, system-ui, sans-serif`;
    ctx.fillText(tg.title, x + 18, y + (tg.sub ? 12 : h / 2) + 0.5);
    if (tg.sub) {
      ctx.fillStyle = th.dim;
      ctx.font = `500 ${small ? 10 : 11}px ui-sans-serif, system-ui, sans-serif`;
      ctx.fillText(tg.sub, x + 7, y + h - (big ? 11 : 9));
    }
    if (tg.kind) this.hits.push({ kind: tg.kind, id: tg.id, x0: x, y0: y, x1: x + w, y1: y + h, z: 10 });
  }

  // hit records a screen box for picking (the box's top diamond and faces).
  hit(kind, id, x, y, w, d, h) {
    const v = this.v;
    const pts = [v.P(x, y, h), v.P(x + w, y, h), v.P(x + w, y + d, 0), v.P(x, y + d, 0), v.P(x + w, y + d, h), v.P(x, y + d, h), v.P(x + w, y, 0)];
    let x0 = Infinity, y0 = Infinity, x1 = -Infinity, y1 = -Infinity;
    for (const [px, py] of pts) { x0 = Math.min(x0, px); y0 = Math.min(y0, py); x1 = Math.max(x1, px); y1 = Math.max(y1, py); }
    this.hits.push({ kind, id, x0, y0, x1, y1, z: 1 });
  }

  // pick: the topmost thing under a screen point (labels first, then solids, then floors).
  pick(sx, sy) {
    for (let i = this.hits.length - 1; i >= 0; i--) {
      const h = this.hits[i];
      if (h.z >= 9 && sx >= h.x0 && sx <= h.x1 && sy >= h.y0 && sy <= h.y1) return h;
    }
    for (let i = this.hits.length - 1; i >= 0; i--) {
      const h = this.hits[i];
      if (h.z < 9 && sx >= h.x0 && sx <= h.x1 && sy >= h.y0 && sy <= h.y1) return h;
    }
    const [gx, gy] = this.v.ground(sx, sy);
    for (const hall of this.world.halls) {
      if (gx >= hall.x0 - 0.5 && gx <= hall.x1 + 0.5 && gy >= hall.y0 - 0.5 && gy <= hall.y1 + 0.3) return { kind: "host", id: hall.name };
    }
    for (const l of this.world.lines) {
      if (gx >= l.x0 - 0.6 && gx <= l.x0 + 2.2 && gy >= l.y0 - 0.5 && gy <= l.y1) return { kind: "class", id: l.name };
    }
    return null;
  }
}

function gear(ctx, x, y, r, teeth, a, col) {
  ctx.save();
  ctx.translate(x, y);
  ctx.rotate(a);
  ctx.beginPath();
  for (let i = 0; i < teeth * 2; i++) {
    const rr = i % 2 ? r * 0.78 : r;
    const a0 = (i / (teeth * 2)) * Math.PI * 2, a1 = ((i + 1) / (teeth * 2)) * Math.PI * 2;
    ctx.lineTo(Math.cos(a0) * rr, Math.sin(a0) * rr);
    ctx.lineTo(Math.cos(a1) * rr, Math.sin(a1) * rr);
  }
  ctx.closePath();
  ctx.fillStyle = rgb(col);
  ctx.fill();
  ctx.fillStyle = rgb(shade(col, 0.55));
  ctx.beginPath(); ctx.arc(0, 0, r * 0.32, 0, Math.PI * 2); ctx.fill();
  ctx.fillStyle = "rgba(0,0,0,0.35)";
  ctx.fillRect(-r * 0.06, -r * 0.7, r * 0.12, r * 0.4);
  ctx.restore();
}

function roundRect(ctx, x, y, w, h, r) {
  ctx.beginPath();
  ctx.moveTo(x + r, y);
  ctx.arcTo(x + w, y, x + w, y + h, r);
  ctx.arcTo(x + w, y + h, x, y + h, r);
  ctx.arcTo(x, y + h, x, y, r);
  ctx.arcTo(x, y, x + w, y, r);
  ctx.closePath();
}

function stripes(a, b, n) {
  const c = document.createElement("canvas");
  c.width = c.height = n * 2;
  const g = c.getContext("2d");
  g.fillStyle = rgb(b);
  g.fillRect(0, 0, n * 2, n * 2);
  g.fillStyle = rgb(a);
  g.beginPath();
  for (let k = -2; k < 4; k++) {
    g.moveTo(k * n, 0); g.lineTo(k * n + n / 2, 0); g.lineTo(k * n + n / 2 + n * 2, n * 2); g.lineTo(k * n + n * 2, n * 2);
  }
  g.fill();
  return c;
}

function chevrons(belt) {
  const c = document.createElement("canvas");
  c.width = c.height = 64;
  const g = c.getContext("2d");
  g.fillStyle = rgb(belt);
  g.fillRect(0, 0, 64, 64);
  for (let y = 0; y < 64; y += 4) { g.fillStyle = y % 8 ? "rgba(255,255,255,0.025)" : "rgba(0,0,0,0.12)"; g.fillRect(0, y, 64, 2); }
  g.strokeStyle = "rgba(255,255,255,0.13)";
  g.lineWidth = 7;
  g.beginPath(); g.moveTo(14, 8); g.lineTo(38, 32); g.lineTo(14, 56); g.stroke();
  g.fillStyle = "rgba(0,0,0,0.3)";
  g.fillRect(0, 0, 3, 64);
  return c;
}

function rollers(belt) {
  const c = document.createElement("canvas");
  c.width = c.height = 64;
  const g = c.getContext("2d");
  g.fillStyle = rgb(shade(belt, 0.8));
  g.fillRect(0, 0, 64, 64);
  for (let x = 0; x < 64; x += 16) {
    const gr = g.createLinearGradient(x, 0, x + 13, 0);
    gr.addColorStop(0, "rgba(255,255,255,0.05)");
    gr.addColorStop(0.4, "rgba(255,255,255,0.22)");
    gr.addColorStop(1, "rgba(0,0,0,0.25)");
    g.fillStyle = gr;
    g.fillRect(x + 1, 0, 13, 64);
  }
  return c;
}
