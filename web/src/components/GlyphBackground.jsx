import React, { useEffect, useRef } from 'react';

// Subtle "glyph matrix" canvas background. Colour follows the theme (white in dark, black in light).
export default function GlyphBackground({ glyphs = '01·•+*/\\<>=', cellSize = 14, mutationRate = 0.04, interval = 90, fadeBottom = 0.6, alpha = 0.16, theme }) {
  const ref = useRef(null);
  const draw = useRef(() => {});

  useEffect(() => {
    const c = ref.current;
    const x = c.getContext('2d');
    const S = cellSize;
    const reduce = window.matchMedia('(prefers-reduced-motion: reduce)').matches;
    let cols = 0, rows = 0, g = [], dpr = 1;
    const rand = () => glyphs[Math.floor(Math.random() * glyphs.length)];
    const paint = () => {
      x.setTransform(dpr, 0, 0, dpr, 0, 0);
      x.clearRect(0, 0, innerWidth, innerHeight);
      x.font = '600 12px ui-monospace,Menlo,monospace';
      x.textAlign = 'center';
      x.textBaseline = 'middle';
      const rgb = getComputedStyle(document.documentElement).getPropertyValue('--gl').trim() || '255,255,255';
      for (let r = 0; r < rows; r++) {
        const fade = 1 - Math.max(0, (r / rows - (1 - fadeBottom)) / fadeBottom);
        const a = alpha * Math.max(0, fade);
        if (a < 0.004) continue;
        x.fillStyle = `rgba(${rgb},${a})`;
        for (let k = 0; k < cols; k++) x.fillText(g[r * cols + k], k * S + S / 2, r * S + S / 2);
      }
    };
    draw.current = paint;
    const size = () => {
      dpr = Math.min(devicePixelRatio || 1, 2);
      c.width = innerWidth * dpr;
      c.height = innerHeight * dpr;
      cols = Math.ceil(innerWidth / S);
      rows = Math.ceil(innerHeight / S);
      g = Array.from({ length: cols * rows }, rand);
      paint();
    };
    const tick = () => {
      const n = Math.floor(g.length * mutationRate);
      for (let i = 0; i < n; i++) g[Math.floor(Math.random() * g.length)] = rand();
      paint();
    };
    size();
    addEventListener('resize', size);
    const id = reduce ? 0 : setInterval(tick, interval);
    return () => { removeEventListener('resize', size); clearInterval(id); };
  }, [glyphs, cellSize, mutationRate, interval, fadeBottom, alpha]);

  useEffect(() => { setTimeout(() => draw.current(), 30); }, [theme]);

  return <canvas id="glyphs" ref={ref} aria-hidden="true" />;
}
