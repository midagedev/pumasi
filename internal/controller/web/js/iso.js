// The isometric view: a 2:1 projection of the ground plane (x down-right,
// y down-left, z up), a camera (pan, zoom) and the few solids everything is
// built from. Light comes from the upper left: tops are lightest, faces that
// look down-left are lit, faces that look down-right are in shade.
import { rgb, shade, rng } from "./util.js";

export const TW = 64, TH = 32, TZ = 34;

export class View {
  constructor(canvas) {
    this.canvas = canvas;
    this.ctx = canvas.getContext("2d");
    this.cam = { x: 0, y: 0, z: 1 };
    this.w = 0; this.h = 0; this.dpr = 1;
  }
  resize() {
    const r = this.canvas.getBoundingClientRect();
    this.dpr = Math.min(window.devicePixelRatio || 1, 2.5);
    this.w = r.width; this.h = r.height;
    const W = Math.max(1, Math.round(r.width * this.dpr)), H = Math.max(1, Math.round(r.height * this.dpr));
    if (this.canvas.width !== W || this.canvas.height !== H) { this.canvas.width = W; this.canvas.height = H; }
  }
  // iso pixel coordinates at zoom 1, before the camera
  iso(x, y, z = 0) { return [(x - y) * TW / 2, (x + y) * TH / 2 - z * TZ]; }
  // screen coordinates in css px
  P(x, y, z = 0) {
    const c = this.cam;
    return [((x - y) * TW / 2 - c.x) * c.z + this.w / 2, ((x + y) * TH / 2 - z * TZ - c.y) * c.z + this.h / 2];
  }
  // screen -> ground (z = 0)
  ground(sx, sy) {
    const c = this.cam;
    const ix = (sx - this.w / 2) / c.z + c.x, iy = (sy - this.h / 2) / c.z + c.y;
    const a = ix / (TW / 2), b = iy / (TH / 2);
    return [(a + b) / 2, (b - a) / 2];
  }
  base() { this.ctx.setTransform(this.dpr, 0, 0, this.dpr, 0, 0); }
  // The ground plane: draw in tile units afterwards.
  onGround(z = 0) {
    const c = this.cam, d = this.dpr, k = c.z * d;
    this.ctx.setTransform(TW / 2 * k, TH / 2 * k, -TW / 2 * k, TH / 2 * k,
      (this.w / 2 - c.x * c.z) * d, (this.h / 2 - c.y * c.z - z * TZ * c.z) * d);
  }
  // A vertical face at y = y0: local x along world x, local y downward (-z).
  onFaceY(y0, zTop) {
    const c = this.cam, d = this.dpr, k = c.z * d;
    const [ox, oy] = this.iso(0, y0, zTop);
    this.ctx.setTransform(TW / 2 * k, TH / 2 * k, 0, TZ * k, (this.w / 2 + (ox - c.x) * c.z) * d, (this.h / 2 + (oy - c.y) * c.z) * d);
  }
  // A vertical face at x = x0: local x along world -y (so text reads), local y downward.
  onFaceX(x0, zTop) {
    const c = this.cam, d = this.dpr, k = c.z * d;
    const [ox, oy] = this.iso(x0, 0, zTop);
    this.ctx.setTransform(TW / 2 * k, -TH / 2 * k, 0, TZ * k, (this.w / 2 + (ox - c.x) * c.z) * d, (this.h / 2 + (oy - c.y) * c.z) * d);
  }
  visible(x, y, z = 0, pad = 120) {
    const [sx, sy] = this.P(x, y, z);
    return sx > -pad && sy > -pad && sx < this.w + pad && sy < this.h + pad * 2;
  }
}

// Path helpers in screen space (css px; the context is at base()).
export function poly(ctx, pts) {
  ctx.beginPath();
  ctx.moveTo(pts[0][0], pts[0][1]);
  for (let i = 1; i < pts.length; i++) ctx.lineTo(pts[i][0], pts[i][1]);
  ctx.closePath();
}

// box: an axis-aligned block from (x,y,z) of w×d×h. col is [r,g,b];
// top/left/right are the shades of the three faces that show.
export function box(v, x, y, z, w, d, h, col, o = {}) {
  const ctx = v.ctx;
  const A = v.P(x, y, z + h), B = v.P(x + w, y, z + h), C = v.P(x + w, y + d, z + h), D = v.P(x, y + d, z + h);
  const C0 = v.P(x + w, y + d, z), D0 = v.P(x, y + d, z), B0 = v.P(x + w, y, z);
  // left face (y = y + d)
  poly(ctx, [D, C, C0, D0]);
  ctx.fillStyle = o.left || rgb(shade(col, o.lk || 0.82));
  ctx.fill();
  // right face (x = x + w)
  poly(ctx, [C, B, B0, C0]);
  ctx.fillStyle = o.right || rgb(shade(col, o.rk || 0.62));
  ctx.fill();
  // top
  poly(ctx, [A, B, C, D]);
  ctx.fillStyle = o.top || rgb(shade(col, o.tk || 1.12));
  ctx.fill();
  if (o.edge !== false) {
    // a lit rim on the top's front edges and a dark crease: reads as a solid at any zoom
    ctx.lineWidth = Math.max(0.6, v.cam.z * 0.9);
    ctx.strokeStyle = o.rim || "rgba(255,255,255,0.16)";
    ctx.beginPath(); ctx.moveTo(D[0], D[1]); ctx.lineTo(C[0], C[1]); ctx.lineTo(B[0], B[1]); ctx.stroke();
    ctx.strokeStyle = o.crease || "rgba(0,0,0,0.28)";
    ctx.beginPath(); ctx.moveTo(C[0], C[1]); ctx.lineTo(C0[0], C0[1]); ctx.stroke();
  }
  return [A, B, C, D, C0, D0, B0];
}

// cylinder standing at ground point (cx, cy): radius in tiles, height in z units.
export function cylinder(v, cx, cy, z, r, h, col, o = {}) {
  const ctx = v.ctx, k = v.cam.z;
  const [bx, by] = v.P(cx, cy, z), [tx, ty] = v.P(cx, cy, z + h);
  const rx = r * TW / 2 * k * 1.41, ry = r * TH / 2 * k * 1.41;
  const g = ctx.createLinearGradient(bx - rx, 0, bx + rx, 0);
  g.addColorStop(0, rgb(shade(col, 0.95)));
  g.addColorStop(0.35, rgb(shade(col, 1.12)));
  g.addColorStop(1, rgb(shade(col, 0.55)));
  ctx.fillStyle = g;
  ctx.beginPath();
  ctx.moveTo(bx - rx, ty);
  ctx.lineTo(bx - rx, by);
  ctx.ellipse(bx, by, rx, ry, 0, Math.PI, 0, true);
  ctx.lineTo(bx + rx, ty);
  ctx.closePath();
  ctx.fill();
  if (o.fill !== undefined) {
    // a level inside: liquid seen through a glass band on the front
    const lv = Math.max(0, Math.min(1, o.fill));
    const ly = by - (by - ty) * lv;
    ctx.save();
    ctx.beginPath();
    ctx.moveTo(bx - rx * 0.82, ly);
    ctx.lineTo(bx - rx * 0.82, by - ry * 0.2);
    ctx.ellipse(bx, by - ry * 0.2, rx * 0.82, ry * 0.8, 0, Math.PI, 0, true);
    ctx.lineTo(bx + rx * 0.82, ly);
    ctx.ellipse(bx, ly, rx * 0.82, ry * 0.8, 0, 0, Math.PI, false);
    ctx.closePath();
    ctx.fillStyle = o.liquid;
    ctx.fill();
    ctx.restore();
  }
  ctx.beginPath();
  ctx.ellipse(tx, ty, rx, ry, 0, 0, Math.PI * 2);
  ctx.fillStyle = o.top || rgb(shade(col, 1.25));
  ctx.fill();
  ctx.lineWidth = Math.max(0.5, k * 0.8);
  ctx.strokeStyle = "rgba(0,0,0,0.25)";
  ctx.stroke();
}

// A noise texture: a small tile of speckles to use as a pattern on the ground.
export function noiseTile(n, base, amp, seed, opts = {}) {
  const c = document.createElement("canvas");
  c.width = c.height = n;
  const g = c.getContext("2d");
  g.fillStyle = rgb(base);
  g.fillRect(0, 0, n, n);
  const r = rng(seed);
  const img = g.getImageData(0, 0, n, n), d = img.data;
  for (let i = 0; i < d.length; i += 4) {
    const k = (r() - 0.5) * amp;
    d[i] += k; d[i + 1] += k; d[i + 2] += k;
  }
  g.putImageData(img, 0, 0);
  if (opts.blots) {
    for (let i = 0; i < opts.blots; i++) {
      g.fillStyle = `rgba(0,0,0,${0.03 + r() * 0.05})`;
      g.beginPath();
      g.ellipse(r() * n, r() * n, 2 + r() * n * 0.12, 1 + r() * n * 0.08, r() * 3, 0, Math.PI * 2);
      g.fill();
    }
  }
  if (opts.seams) {
    g.strokeStyle = opts.seams;
    g.lineWidth = 1;
    g.strokeRect(0.5, 0.5, n - 1, n - 1);
  }
  return c;
}
