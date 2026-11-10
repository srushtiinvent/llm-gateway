import React, { forwardRef, useEffect, useImperativeHandle, useRef } from 'react';
import { sleep } from '../lib/fx.js';

// Terminal with typed commands (typingSpeed / delayBetweenCommands, like the shadcn terminal)
// and a live log mode. Use the ref: ref.current.log(text, cls) | play(script) | clear() | stop()
const Terminal = forwardRef(function Terminal(
  { title = 'TERMINAL', minHeight = 120, max = 14, typingSpeed = 45, delayBetweenCommands = 1000, loop = false, className = '', style },
  ref,
) {
  const bodyRef = useRef(null);
  const run = useRef(0);
  const cur = useRef(null);

  const api = useRef({
    line(t, cls = '') {
      const el = bodyRef.current;
      if (!el) return;
      const d = document.createElement('span');
      d.className = 'ln ' + cls;
      d.textContent = t;
      cur.current?.remove();
      el.appendChild(d);
      while (el.children.length > max) el.firstChild.remove();
    },
    clear() {
      if (bodyRef.current) bodyRef.current.innerHTML = '';
    },
    stop() { run.current++; },
    async type(t, id) {
      const el = bodyRef.current;
      if (!el) return false;
      cur.current?.remove();
      const d = document.createElement('span');
      d.className = 'ln cmd';
      cur.current = document.createElement('span');
      cur.current.className = 'cur';
      el.appendChild(d);
      d.appendChild(cur.current);
      for (const ch of t) {
        if (id !== run.current) return false;
        d.insertBefore(document.createTextNode(ch), cur.current);
        await sleep(typingSpeed);
      }
      return id === run.current;
    },
    async play(script) {
      const id = ++run.current;
      api.current.clear();
      do {
        for (const s of script) {
          if (id !== run.current) return;
          if (!(await api.current.type(s.cmd, id))) return;
          await sleep(300);
          for (const [t, c] of s.out || []) {
            if (id !== run.current) return;
            api.current.line(t, c);
            await sleep(120);
          }
          if (cur.current && bodyRef.current) bodyRef.current.appendChild(cur.current);
          await sleep(delayBetweenCommands);
        }
        if (loop && id === run.current) { await sleep(1600); api.current.clear(); }
      } while (loop && id === run.current);
    },
  });

  useImperativeHandle(ref, () => ({
    log: (t, c) => api.current.line(t, c),
    clear: () => api.current.clear(),
    play: s => api.current.play(s),
    stop: () => api.current.stop(),
  }));
  useEffect(() => () => api.current.stop(), []);

  return (
    <div className={`term ${className}`} style={style}>
      <div className="bar"><i /><i /><i /><span>{title}</span></div>
      <div className="body" ref={bodyRef} style={{ minHeight }} />
    </div>
  );
});
export default Terminal;
