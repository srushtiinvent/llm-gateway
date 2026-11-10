import React, { useEffect, useRef } from 'react';
import { reduceMotion } from '../lib/fx.js';

// Scramble-then-settle title (left to right), plays once. Based on the React Bits Shuffle idea:
// every letter runs through random characters and lands on the real one, with a stagger.
export default function Shuffle({
  text,
  duration = 0.55,
  stagger = 0.07,
  shuffleTimes = 6,
  scrambleCharset = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789#%&?',
  onSettled,
  as: Tag = 'h1',
  className = '',
  ...rest
}) {
  const ref = useRef(null);

  useEffect(() => {
    const el = ref.current;
    if (!el) return undefined;
    let cancelled = false;
    el.textContent = '';
    el.classList.remove('is-ready', 'settled');
    const mk = t => {
      const c = document.createElement('span');
      c.className = 'shuffle-char';
      c.textContent = t === ' ' ? ' ' : t;
      return c;
    };
    const items = [...text].map(ch => {
      const w = document.createElement('span');
      w.className = 'shuffle-char-wrapper';
      const strip = document.createElement('span');
      const n = ch === ' ' ? 0 : shuffleTimes;
      for (let k = 0; k < n; k++) strip.appendChild(mk(scrambleCharset[Math.floor(Math.random() * scrambleCharset.length)]));
      strip.appendChild(mk(ch));
      w.appendChild(strip);
      el.appendChild(w);
      return { w, strip, n };
    });

    const start = () => {
      if (cancelled) return;
      items.forEach(({ w, strip }) => {
        const cw = strip.lastChild.getBoundingClientRect().width;
        w.style.width = cw + 'px';
        [...strip.children].forEach(c => { c.style.width = cw + 'px'; });
      });
      el.classList.add('is-ready');
      const settle = () => {
        items.forEach(({ strip }) => {
          while (strip.children.length > 1) strip.firstChild.remove();
          strip.getAnimations().forEach(a => a.cancel());
          strip.style.transform = 'none';
        });
        el.classList.add('settled');
        onSettled?.();
      };
      if (reduceMotion()) { settle(); return; }
      items.forEach(({ strip, n }, i) => {
        const w = strip.lastChild.getBoundingClientRect().width;
        const an = strip.animate(
          [{ transform: 'translateX(0)' }, { transform: `translateX(${-n * w}px)` }],
          { duration: duration * 1000, delay: i * stagger * 1000, easing: 'cubic-bezier(.215,.61,.355,1)', fill: 'both' },
        );
        if (i === items.length - 1) an.finished.then(() => !cancelled && settle()).catch(() => {});
      });
    };
    // wait for the pixel font, otherwise the letters get measured with the wrong font
    const fam = getComputedStyle(el).fontFamily.split(',')[0];
    Promise.race([document.fonts.load(`16px ${fam}`), new Promise(r => setTimeout(r, 2500))])
      .then(() => document.fonts.ready)
      .then(() => requestAnimationFrame(start));
    return () => { cancelled = true; };
  }, [text, duration, stagger, shuffleTimes, scrambleCharset]); // eslint-disable-line react-hooks/exhaustive-deps

  return <Tag ref={ref} className={`shuffle-parent ${className}`} aria-label={text} {...rest} />;
}
