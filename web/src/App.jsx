import React, { useCallback, useEffect, useRef, useState } from 'react';
import { flushSync } from 'react-dom';
import BlobCursor from './components/BlobCursor.jsx';
import GlyphBackground from './components/GlyphBackground.jsx';
import Header from './components/Header.jsx';
import LoadingScreen from './components/LoadingScreen.jsx';
import Play from './pages/Play.jsx';
import RadarPage from './pages/RadarPage.jsx';
import Simulator from './pages/Simulator.jsx';
import Keys from './pages/Keys.jsx';
import { useGateway } from './lib/GatewayContext.jsx';
import { reduceMotion, revealIntro, revealPage, revealRipple } from './lib/fx.js';

const ACC = { dark: '#D98AAB', light: '#D6277F' };
const BOOT_MS = 5200;

function Ticker() {
  const gw = useGateway();
  const down = gw.providers.filter(p => p.fault === 'down' || p.fault === 'limit' || p.breaker === 'open');
  const slow = gw.providers.some(p => p.fault === 'slow');
  const msg = down.length === gw.providers.length ? 'ALL PROVIDERS GROUNDED'
    : down.length ? `${down[0].name.toUpperCase()} GROUNDED, TRAFFIC REROUTED`
    : slow ? 'SLOW SKIES, EXPECT DELAYS' : 'ALL ROUTES CLEARED';
  const base = `★ TOWER: ${msg} ★ YOUR KEY HAS ${gw.usage.dayLeft} FLIGHTS LEFT TODAY ★ FREE-TIER QUOTA IS TRACKED PER PROVIDER ★ `;
  return <div className="tick rv"><span>{base}{base}</span></div>;
}

export default function App() {
  const gw = useGateway();
  const [theme, setThemeState] = useState(() => document.documentElement.dataset.theme || 'dark');
  const [page, setPage] = useState('play');
  const [boot, setBoot] = useState('show'); // show -> leaving -> gone
  const first = useRef(true);
  const [blobOn, setBlobOn] = useState(false);

  // loading screen, then "everything loads one by one"
  useEffect(() => {
    const ms = reduceMotion() ? 900 : BOOT_MS;
    const a = setTimeout(() => setBoot('leaving'), ms);
    const b = setTimeout(() => setBoot('gone'), ms + 700);
    return () => { clearTimeout(a); clearTimeout(b); };
  }, []);
  useEffect(() => { if (boot === 'leaving') revealIntro(); }, [boot]);

  // page change: stagger the new page in
  useEffect(() => {
    if (first.current) { first.current = false; return; }
    revealPage();
    window.scrollTo({ top: 0, behavior: 'smooth' });
  }, [page]);

  const setTheme = useCallback(async (next, x, y) => {
    const apply = () => {
      document.documentElement.dataset.theme = next;
      try { localStorage.setItem('gw-theme', next); } catch { /* ignore */ }
      flushSync(() => setThemeState(next));
      revealRipple(x, y);
    };
    if (reduceMotion() || !document.startViewTransition) { apply(); return; }
    const R = Math.hypot(Math.max(x, innerWidth - x), Math.max(y, innerHeight - y));
    const t = document.startViewTransition(apply);
    await t.ready;
    document.documentElement.animate(
      { clipPath: [`circle(0px at ${x}px ${y}px)`, `circle(${R}px at ${x}px ${y}px)`] },
      { duration: 850, easing: 'cubic-bezier(.5,0,.2,1)', pseudoElement: '::view-transition-new(root)' },
    );
  }, []);

  const toggle = e => {
    const r = e.currentTarget.getBoundingClientRect();
    setTheme(theme === 'dark' ? 'light' : 'dark', r.left + r.width / 2, r.top + r.height / 2);
  };

  // bubble (blob) cursor only on mouse devices; hide the system cursor once WebGL really drew it
  const lite = typeof location !== 'undefined' && location.search.includes('lite'); // ?lite = no WebGL effects (low-power mode)
  const fine = !lite && typeof matchMedia !== 'undefined' && matchMedia('(pointer:fine)').matches;
  useEffect(() => {
    if (!fine) return undefined;
    const t = setTimeout(() => setBlobOn(!!document.querySelector('#blobhost canvas')), 400);
    return () => clearTimeout(t);
  }, [fine]);
  useEffect(() => { document.documentElement.classList.toggle('blob-on', blobOn); }, [blobOn]);

  const Page = { play: Play, radar: RadarPage, sim: Simulator, keys: Keys }[page];

  return (
    <>
      {boot !== 'gone' && <LoadingScreen theme={theme} leaving={boot === 'leaving'} />}
      <GlyphBackground theme={theme} />
      <div className="bgglow" />
      {fine && (
        <div id="blobhost">
          <BlobCursor material="pearl" color={ACC[theme]} size={30} morph={0.15} trail={6} lag={0.5} wobble={1} splash splashCount={5} splashSize={0.4} hideCursor={false} />
        </div>
      )}
      <div className="app">
        <Header page={page} onPage={setPage} theme={theme} onToggle={toggle} mode={gw.mode} />
        <Ticker />
        <Page key={page} />
        {gw.mode === 'demo' && (
          <p className="mut" style={{ font: '700 10px var(--mono)', letterSpacing: '.1em', textAlign: 'center', marginTop: 22 }}>
            DEMO MODE · NO GATEWAY FOUND ON /v1 · START THE GO BACKEND AND REFRESH FOR LIVE DATA
          </p>
        )}
      </div>
    </>
  );
}
