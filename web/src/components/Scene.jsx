import React, { forwardRef, useEffect, useImperativeHandle, useRef } from 'react';
import sceneSvg from '../lib/scene.svg?raw';
import { T, reduceMotion } from '../lib/fx.js';

const NS = 'http://www.w3.org/2000/svg';
const LET = { gemini: 'G', groq: 'Q' };

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

  // hangars / lanes / sub labels follow the live provider state
  useEffect(() => {
    providers.forEach(p => {
      const L = LET[p.name];
      if (!L) return;
      const down = p.fault === 'down' || p.fault === 'limit' || p.breaker === 'open';
      const h = q('#h' + L);
      h?.classList.toggle('down', down);
      h?.classList.toggle('live', !down);
      q('#lane' + L)?.classList.toggle('bad', down);
      const sub = q(L === 'G' ? '#gSub' : '#qSub');
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
      ['laneA', 'laneG', 'laneQ'].forEach(x => {
        if (!id || x === id) q('#' + x)?.classList.remove('act');
      });
      if (id && cls) q('#' + id)?.classList.add(cls);
    },
    laneBad(id, on) { q('#' + id)?.classList.toggle('bad', on); },
    laneRemove(id, cls) { q('#' + id)?.classList.remove(cls); },
    shake(provider) {
      const h = q('#h' + LET[provider]);
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
