import React, { forwardRef, useEffect, useImperativeHandle, useRef } from 'react';
import sceneSvg from '../lib/scene.svg?raw';
import { T, reduceMotion } from '../lib/fx.js';
import { provColor } from '../lib/providers.js';

const NS = 'http://www.w3.org/2000/svg';

// Hangar geometry inside the 900x440 scene. One hangar per provider, stacked down the
// right-hand side, so the picture grows with however many providers are configured.
const HANGAR = { x: 700, w: 170, h: 82, hubX: 506, hubY: 225, top: 70, bottom: 352 };
export const laneId = name => 'lane-' + String(name).replace(/[^a-zA-Z0-9]/g, '-');
const hangarId = name => 'h-' + String(name).replace(/[^a-zA-Z0-9]/g, '-');

/** Vertical centre of hangar i of n, spread evenly between top and bottom. */
function centreY(i, n) {
  const { top, bottom } = HANGAR;
  if (n <= 1) return (top + bottom) / 2;
  return top + ((bottom - top) * i) / (n - 1);
}

// The animated air-traffic scene (SVG). `providers` colours the hangars; the ref gives Play the
// animation tools: fly(laneId, from, to, ms), burst(x, y, color, n), lane(id, cls), shake(provider).
const Scene = forwardRef(function Scene({ providers }, ref) {
  const root = useRef(null);
  const q = s => root.current?.querySelector(s);

  useEffect(() => {
    const g = q('#stars');
    if (g && !g.childNodes.length) {
      for (let i = 0; i < 46; i++) {
        const c = document.createElementNS(NS, 'circle');
        c.setAttribute('class', 'star');
        c.setAttribute('cx', Math.random() * 900);
        c.setAttribute('cy', Math.random() * 400);
        c.setAttribute('r', (Math.random() * 1.4 + 0.6).toFixed(1));
        c.style.animationDelay = (-Math.random() * 3).toFixed(2) + 's';
        g.appendChild(c);
      }
    }
  }, []);

  // Build one lane + hangar per provider, then keep them in step with the live state.
  const names = providers.map(p => p.name).join('|');
  useEffect(() => {
    const lanes = q('#lanes'), hangars = q('#hangars');
    if (!lanes || !hangars) return;
    lanes.textContent = '';
    hangars.textContent = '';
    const n = providers.length;
    const { x, w, hubX, hubY } = HANGAR;
    // With many providers the hangars would overlap, so shrink them to fit the gap.
    const gap = n > 1 ? (HANGAR.bottom - HANGAR.top) / (n - 1) : HANGAR.h * 2;
    const k = Math.min(1, gap / (HANGAR.h + 14));
    const h = HANGAR.h * k;
    const el = (tag, attrs, parent, text) => {
      const e = document.createElementNS(NS, tag);
      Object.entries(attrs).forEach(([k, v]) => e.setAttribute(k, v));
      if (text !== undefined) e.textContent = text;
      parent.appendChild(e);
      return e;
    };
    providers.forEach((p, i) => {
      const cy = centreY(i, n);          // hangar centre
      const top = cy - h / 2;
      const mid = x + w / 2;
      // lanes fan out from the hub so they stay distinguishable
      const startY = hubY + (i - (n - 1) / 2) * 20;
      el('path', { id: laneId(p.name), class: 'lane', style: `--pc:${provColor(p.name, providers)}`,
                   d: `M${hubX} ${startY} C${hubX + 84} ${startY} ${x - 100} ${cy} ${x} ${cy}` }, lanes);

      const g = el('g', { class: 'hangar live', id: hangarId(p.name),
                          style: `--pc:${provColor(p.name, providers)}` }, hangars);
      const smoke = el('g', { class: 'smoke' }, g);
      [[mid - 25, 5], [mid - 5, 6], [mid + 15, 7]].forEach(([sx, r]) =>
        el('circle', { cx: sx, cy: top - 4, r: r * k }, smoke));
      el('path', { class: 'body',
                   d: `M${x} ${top + h} V${top + 26 * k} Q${x} ${top} ${mid} ${top} Q${x + w} ${top} ${x + w} ${top + 26 * k} V${top + h} Z` }, g);
      el('rect', { class: 'haz', x, y: top + 64 * k, width: w, height: 18 * k, fill: 'url(#hz)' }, g);
      el('text', { class: 'lbl', x: mid, y: top + 40 * k, 'text-anchor': 'middle',
                   style: `font-size:${(13 * k).toFixed(1)}px` }, g, p.name.toUpperCase());
      el('text', { class: 'sub', id: hangarId(p.name) + '-sub', x: mid, y: top + 58 * k, 'text-anchor': 'middle',
                   style: `font-size:${(9.5 * k).toFixed(1)}px` }, g, '');
      el('text', { class: 'stamp', x: mid, y: top + 106 * k, 'text-anchor': 'middle' }, g, 'GROUNDED');
    });
  }, [names]);

  // live state: fuel, faults, grounded stamps
  useEffect(() => {
    providers.forEach(p => {
      const down = p.fault === 'down' || p.fault === 'limit' || p.breaker === 'open';
      const h = q('#' + hangarId(p.name));
      h?.classList.toggle('down', down);
      h?.classList.toggle('live', !down);
      q('#' + laneId(p.name))?.classList.toggle('bad', down);
      const sub = q('#' + hangarId(p.name) + '-sub');
      if (sub) {
        sub.textContent = down
          ? p.fault === 'limit' ? '429 · RESTING' : 'DOWN · TRIES FAILING'
          : p.fault === 'slow' ? 'SLOW · DELAYS' : `CLEARED · FUEL ${Math.round((100 * (p.minuteMax - p.minuteUsed)) / Math.max(1, p.minuteMax))}%`;
      }
    });
  }, [providers]);

  const mk = (tag, attrs, parent) => {
    const e = document.createElementNS(NS, tag);
    Object.entries(attrs).forEach(([k, v]) => e.setAttribute(k, v));
    (parent || q('#fx')).appendChild(e);
    return e;
  };

  const api = {
    burst(x, y, color = 'var(--acc)', n = 10) {
      for (let i = 0; i < n; i++) {
        const a = (Math.PI * 2 * i) / n;
        const d = 18 + Math.random() * 26;
        const c = mk('circle', { cx: x, cy: y, r: 3, fill: color });
        c.animate(
          [{ transform: 'translate(0,0) scale(1)', opacity: 1 }, { transform: `translate(${Math.cos(a) * d}px,${Math.sin(a) * d}px) scale(.2)`, opacity: 0 }],
          { duration: T(700), easing: 'ease-out' },
        ).onfinish = () => c.remove();
      }
      const r = mk('circle', { cx: x, cy: y, r: 10, fill: 'none', stroke: color, 'stroke-width': 3, class: 'burst' });
      setTimeout(() => r.remove(), T(700));
    },
    fly(sel, from, to, ms) {
      const p = q(sel);
      const plane = q('#plane');
      const L = p.getTotalLength();
      return new Promise(res => {
        const t0 = performance.now();
        let last = 0;
        plane.setAttribute('opacity', 1);
        (function step(now) {
          const k = Math.max(0, Math.min(1, (now - t0) / T(ms || 1)));
          const e = k < 0.5 ? 2 * k * k : 1 - Math.pow(-2 * k + 2, 2) / 2;
          const f = from + (to - from) * e;
          const a = p.getPointAtLength(L * f);
          const b = p.getPointAtLength(L * Math.min(1, Math.max(0, f + (to > from ? 0.01 : -0.01))));
          plane.setAttribute('transform', `translate(${a.x},${a.y}) rotate(${(Math.atan2(b.y - a.y, b.x - a.x) * 180) / Math.PI})`);
          if (now - last > 38) {
            last = now;
            const c = mk('circle', { cx: a.x, cy: a.y, r: 3.2, fill: 'var(--acc)', class: 'trail' });
            setTimeout(() => c.remove(), 900);
          }
          if (k < 1 && !reduceMotion()) requestAnimationFrame(step);
          else res();
        })(t0);
      });
    },
    point(sel, f) {
      const p = q(sel);
      return p.getPointAtLength(p.getTotalLength() * f);
    },
    lane(id, cls) {
      root.current?.querySelectorAll('.lane').forEach(el => {
        if (!id || el.id === id) el.classList.remove('act');
      });
      if (id && cls) q('#' + id)?.classList.add(cls);
    },
    hasLane(id) { return Boolean(q('#' + id)); },
    laneBad(id, on) { q('#' + id)?.classList.toggle('bad', on); },
    laneRemove(id, cls) { q('#' + id)?.classList.remove(cls); },
    shake(provider) {
      const h = q('#' + hangarId(provider));
      if (!h) return;
      h.classList.remove('shake');
      void h.getBoundingClientRect();
      h.classList.add('shake');
    },
    hubFlash() {
      const h = q('#hubcore');
      h?.classList.add('hubflash');
      setTimeout(() => h?.classList.remove('hubflash'), 500);
    },
    hidePlane() { q('#plane')?.setAttribute('opacity', 0); },
  };
  useImperativeHandle(ref, () => api);

  return <div ref={root} className="scene-host" dangerouslySetInnerHTML={{ __html: sceneSvg }} />;
});
export default Scene;
