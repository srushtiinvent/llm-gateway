import React, { useLayoutEffect, useRef } from 'react';

const PAGES = [['play', 'PLAY'], ['radar', 'RADAR'], ['sim', 'SIMULATOR'], ['keys', 'KEYS']];

// Brand left, nav truly centred, round retro light-switch right.
export default function Header({ page, onPage, theme, onToggle, mode }) {
  const nav = useRef(null);
  const pill = useRef(null);

  useLayoutEffect(() => {
    const place = () => {
      const a = nav.current?.querySelector('a.on');
      if (a && pill.current) {
        pill.current.style.left = a.offsetLeft + 'px';
        pill.current.style.width = a.offsetWidth + 'px';
      }
    };
    place();
    window.addEventListener('resize', place);
    document.fonts?.ready.then(place);
    return () => window.removeEventListener('resize', place);
  }, [page]);

  return (
    <header>
      <div className="brand rv">
        <svg width="40" height="40" viewBox="0 0 64 64">
          <rect x="4" y="4" width="56" height="56" rx="14" fill="var(--acc)" />
          <circle cx="32" cy="34" r="4.5" fill="var(--on-acc)" />
          <path d="M22 44a14 14 0 0 1 0-20M42 24a14 14 0 0 1 0 20M15 51a24 24 0 0 1 0-34M49 17a24 24 0 0 1 0 34" fill="none" stroke="var(--on-acc)" strokeWidth="4.2" strokeLinecap="round" />
        </svg>
        <div><b>GATEWAY</b><small>FREE-TIER AIR TRAFFIC</small></div>
      </div>
      <nav className="rv" ref={nav}>
        <i id="navpill" ref={pill} />
        {PAGES.map(([id, label]) => (
          <a key={id} className={page === id ? 'on' : ''} onClick={() => onPage(id)}>{label}</a>
        ))}
      </nav>
      <button className="swt rv" onClick={onToggle} aria-label="Switch night / day" title={mode === 'live' ? 'Connected to the gateway' : 'Demo mode'}>
        <span id="mode">{theme === 'dark' ? 'NIGHT' : 'DAY'}</span>
        <svg viewBox="0 0 56 56">
          <circle className="plate" cx="28" cy="28" r="26" />
          <circle cx="28" cy="28" r="22.5" fill="none" stroke="var(--line)" strokeWidth="1" opacity=".7" />
          <circle className="scr" cx="28" cy="3.6" r="1.3" />
          <circle className="scr" cx="28" cy="52.4" r="1.3" />
          <g transform="translate(14.4 13)"><path className="ic im" d="M-1.8 -4.4a4.6 4.6 0 1 0 6 5.8 3.6 3.6 0 0 1-6-5.8z" /></g>
          <g transform="translate(41.6 13)" className="ic is"><circle r="2.3" /><circle r="4.9" strokeDasharray="1.7 2.75" /></g>
          <g className="knob"><g className="cap">
            <circle className="face" cx="28" cy="31" r="13" />
            <circle cx="28" cy="31" r="9.2" fill="none" stroke="var(--line)" strokeWidth="1.2" />
            <line className="ptr" x1="28" y1="31" x2="28" y2="19.5" />
            <circle cx="28" cy="31" r="2.6" fill="var(--mut)" />
          </g></g>
        </svg>
      </button>
    </header>
  );
}
