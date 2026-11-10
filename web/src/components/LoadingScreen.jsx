import React, { useRef } from 'react';
import Radar from './Radar.jsx';
import Shuffle from './Shuffle.jsx';

// Loading screen: React Bits Radar behind a glass orb (logo, 3D shuffle title, progress).
// Works in both themes (uses the theme variables) and tilts the title toward the mouse.
export default function LoadingScreen({ theme = 'dark', leaving = false }) {
  const titleRef = useRef(null);

  const tilt = () => {
    const h = titleRef.current?.parentElement?.querySelector('h1');
    if (!h || window.matchMedia('(prefers-reduced-motion: reduce)').matches) return;
    const move = e => {
      const x = e.clientX / innerWidth - 0.5;
      const y = e.clientY / innerHeight - 0.5;
      h.style.transform = `rotateY(${x * 22}deg) rotateX(${-y * 16}deg)`;
    };
    window.addEventListener('pointermove', move, { passive: true });
  };

  const lite = typeof location !== 'undefined' && location.search.includes('lite');
  const light = theme === 'light';
  const small = typeof window !== 'undefined' && window.innerWidth < 600;

  return (
    <div className={`loading-screen ${theme}${leaving ? ' is-leaving' : ''}`} role="status" aria-label="Loading LLM Gateway">
      <div className="loader-radar" aria-hidden="true">
        {!lite && (
        <Radar
          color={light ? '#D6277F' : '#D98AAB'}
          lightMode={light}
          mode="sweep"
          speed={1}
          trail={0.5}
          scale={small ? 0.95 : 0.5}
          centerY={0.5}
          ringCount={6}
          spokeCount={12}
          targets={6}
          clutter={0.35}
          mouseInteraction
          grain={0.02}
          glow={1.3}
          brightness={1.2}
          gridOpacity={1.5}
          intro
        />
        )}
      </div>
      <div className="loader-content" ref={titleRef}>
        <svg className="loader-logo" viewBox="0 0 64 64" aria-hidden="true">
          <rect x="4" y="4" width="56" height="56" rx="14" fill="var(--acc)" />
          <circle cx="32" cy="34" r="4.5" fill="var(--on-acc)" />
          <path d="M22 44a14 14 0 0 1 0-20M42 24a14 14 0 0 1 0 20M15 51a24 24 0 0 1 0-34M49 17a24 24 0 0 1 0 34" fill="none" stroke="var(--on-acc)" strokeWidth="4.2" strokeLinecap="round" />
        </svg>
        <p className="loader-label">TRAFFIC CONTROL</p>
        <Shuffle text="LLM GATEWAY" id="loadtitle" onSettled={tilt} />
        <p className="loader-status">Preparing the tower</p>
        <div className="loader-track" aria-hidden="true"><span /></div>
      </div>
    </div>
  );
}
