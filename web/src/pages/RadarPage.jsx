import React, { useEffect, useMemo, useRef, useState } from 'react';
import { useGateway } from '../lib/GatewayContext.jsx';
import { countTo } from '../lib/fx.js';
import { settings } from '../lib/api.js';

const WINDOWS = [['1h', 'LAST HOUR', '1H'], ['6h', 'LAST 6 HOURS', '6H'], ['24h', 'LAST 24 HOURS', '24H'], ['7d', 'LAST 7 DAYS', '7D']];

function ProviderPanel({ p }) {
  const down = p.fault === 'down' || p.fault === 'limit' || p.breaker === 'open';
  const col = p.name === 'gemini' ? 'gem' : 'grq';
  const c = `var(--${down ? 'bad' : col})`;
  const tank = (l, used, max, warn) => {
    const left = Math.max(0, max - used);
    const pct = (100 * left) / Math.max(1, max);
    return (
      <div className="fuel">
        <div className="t"><span>{l}</span><b>{left} / {max}</b></div>
        <div className="tank"><i style={{ width: `${pct}%`, background: pct < warn ? 'var(--warn)' : `var(--${col})` }} /></div>
      </div>
    );
  };
  const ms = (p.avgMs * (p.fault === 'slow' ? 4 : 1)) / 1000;
  return (
    <div className="rv rvw">
    <div className={`card pv ${down ? 'down' : ''}`}>
      <div className="hd">
        <span className="lamp" style={{ background: c, boxShadow: `0 0 10px ${c}` }} />
        <b className="n">{p.name.toUpperCase()}</b>
        <span className="st" style={{ color: c }}>{down ? 'GROUNDED' : p.fault === 'slow' ? 'SLOW' : 'CLEARED'}</span>
      </div>
      {p.own ? <div className="mut" style={{ font: '700 10px/1.6 var(--mono)', letterSpacing: '.08em', margin: '10px 0' }}>YOUR OWN KEY · LIMITS ARE SET BY YOUR PROVIDER ACCOUNT</div> : tank('PER MINUTE', p.minuteUsed, p.minuteMax, 30)}
      {!p.own && tank('PER DAY', p.dayUsed, p.dayMax, 20)}
      <div className="ministat">
        <div>SPEED<b>{ms ? ms.toFixed(2) + ' s' : '-'}</b></div>
        <div>BREAKER<b style={{ color: c }}>{down ? 'OPEN' : 'CLOSED'}</b></div>
        <div>{down ? 'RETRY IN' : 'ANSWERED'}<b>{down ? (p.retryIn || 28) + ' s' : p.ok ?? 0}</b></div>
      </div>
      <div style={{ marginTop: 12, font: '700 10px var(--mono)', color: down ? 'var(--bad)' : 'var(--mut)', letterSpacing: '.08em' }}>
        {down ? 'LAST ERROR · ' + (p.lastError || (p.fault === 'limit' ? '429 too many requests' : 'timeout waiting for provider')) : p.failed ? `${p.failed} FAILED CALLS SINCE START` : 'NO ERRORS SINCE START'}
      </div>
    </div>
    </div>
  );
}

function Chart({ bars }) {
  const k = useMemo(() => {
    const mx = Math.max(1, ...bars.map(b => b.ok + b.cache + b.error));
    return 112 / mx;
  }, [bars]);
  const w = 880 / Math.max(1, bars.length);
  return (
    <svg viewBox="0 0 880 128" width="100%" height="128">
      <path d="M0 120H880M0 80H880M0 40H880" stroke="var(--line)" strokeDasharray="3 7" />
      {bars.map((b, i) => {
        let y = 120;
        return [[b.ok, 'var(--gem)'], [b.cache, 'var(--acc)'], [b.error, 'var(--bad)']].map(([v, col], j) => {
          const h = v * k;
          y -= h;
          return h > 0 ? (
            <rect key={`${i}-${j}`} x={6 + i * w} y={y.toFixed(1)} width={Math.max(2, w - 6)} height={h.toFixed(1)} rx="3" fill={col}
              style={{ transformBox: 'fill-box', transformOrigin: 'bottom', animation: `grow .7s cubic-bezier(.2,1.2,.3,1) both`, animationDelay: `${i * 14 + j * 30}ms` }} />
          ) : null;
        });
      })}
    </svg>
  );
}

export default function RadarPage() {
  const gw = useGateway();
  const [win, setWin] = useState('1h');
  const ring = useRef(null);
  const savedRef = useRef(null);
  const u = gw.usage;
  const pct = u.minuteLeft / Math.max(1, u.minuteMax);

  useEffect(() => { gw.loadOverview(win); }, [win, gw.mode]); // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => { const t = setTimeout(() => ring.current?.setAttribute('stroke-dasharray', `${377 * pct * 0.7} 377`), 120); return () => clearTimeout(t); }, [pct]);
  useEffect(() => { countTo(savedRef.current, gw.stats.cachePct, v => Math.round(v) + '%', 900); }, [gw.stats.cachePct]);

  return (
    <section className="page on" id="pg-radar">
      <div className="g2 radar-top" style={{ gridTemplateColumns: `repeat(${Math.max(1, gw.providers.length)}, minmax(0, 1fr)) 290px` }}>
        {gw.providers.map(p => <ProviderPanel key={p.name} p={p} />)}
        <div className="card pad rv" style={{ textAlign: 'center' }}>
          <div className="ttl"><span>YOUR FLIGHTS LEFT</span></div>
          <div style={{ position: 'relative', width: 150, margin: '0 auto' }}>
            <svg width="150" height="150" viewBox="0 0 150 150">
              <circle cx="75" cy="75" r="60" fill="none" stroke="var(--panel2)" strokeWidth="12" />
              <circle ref={ring} cx="75" cy="75" r="60" fill="none" stroke="var(--acc)" strokeWidth="12" strokeLinecap="round" strokeDasharray="0 377" transform="rotate(-90 75 75)" style={{ transition: 'stroke-dasharray 1.1s cubic-bezier(.2,.8,.2,1)', filter: 'drop-shadow(0 0 8px var(--glow))' }} />
            </svg>
            <div style={{ position: 'absolute', inset: 0, display: 'grid', placeItems: 'center' }}>
              <div><div className="big" style={{ fontSize: 40 }}>{u.minuteLeft}</div><div className="mut" style={{ font: '700 9px var(--mono)', letterSpacing: '.1em' }}>OF {u.minuteMax} / MIN</div></div>
            </div>
          </div>
          <div className="fuel" style={{ textAlign: 'left', marginTop: 14 }}>
            <div className="t"><span>TODAY</span><b>{u.dayLeft} / {u.dayMax}</b></div>
            <div className="tank"><i style={{ width: `${(100 * u.dayLeft) / Math.max(1, u.dayMax)}%` }} /></div>
          </div>
          <div className="mut" style={{ font: '600 11px/1.4 var(--sans)', marginTop: 10 }}>Resets at UTC midnight (5:30 AM IST). Cache hits are free.{gw.mode === 'live' && !settings.gatewayKey() ? ' Take off once on the Play page to get your free key.' : ''}</div>
        </div>
      </div>

      <div className="g2" style={{ gridTemplateColumns: '1fr 290px', marginTop: 16 }}>
        <div className="card pad rv">
          <div className="ttl">
            <span>TRAFFIC · {WINDOWS.find(w => w[0] === win)[1]}</span>
            <span><span style={{ color: 'var(--gem)' }}>■ PROVIDER</span> &nbsp;<span style={{ color: 'var(--acc)' }}>■ CACHE</span> &nbsp;<span style={{ color: 'var(--bad)' }}>■ ERROR</span></span>
          </div>
          <div style={{ display: 'flex', gap: 6, marginBottom: 8 }}>
            {WINDOWS.map(([k, , label]) => <span key={k} className={`chip ${win === k ? 'ok' : ''}`} style={{ cursor: 'pointer' }} onClick={() => setWin(k)}>{label}</span>)}
          </div>
          <Chart bars={gw.overview.bars} />
        </div>
        <div className="card pad rv">
          <div className="ttl"><span>QUOTA SAVED</span></div>
          <div className="big" style={{ color: 'var(--acc)' }} ref={savedRef}>0%</div>
          <div className="mut" style={{ font: '600 12.5px/1.5 var(--sans)', marginTop: 8 }}>
            of requests came from memory. That saved <b style={{ color: 'var(--tx)' }}>{(gw.overview.savedTokens || 0).toLocaleString()} tokens</b> of free quota.
          </div>
        </div>
      </div>

      <div className="card pad rv" style={{ marginTop: 16 }}>
        <div className="ttl"><span>FLIGHT BOARD · LIVE</span><span className="chip ok">{gw.mode === 'live' ? 'LIVE' : 'DEMO'}</span></div>
        <div className="rowt h" style={{ gridTemplateColumns: '80px 90px 120px 1fr 90px 90px' }}><span>TIME</span><span>PROVIDER</span><span>STATUS</span><span>WHAT HAPPENED</span><span>SPEED</span><span>TOKENS</span></div>
        <div id="board">
          {gw.board.map((r, i) => (
            <div key={r.t + i + r.p} className={`rowt ${i === 0 ? 'flash' : ''}`} style={{ gridTemplateColumns: '80px 90px 120px 1fr 90px 90px' }}>
              <span className="mut">{r.t}</span>
              <span style={{ color: `var(--${r.p === 'gemini' ? 'gem' : 'grq'})` }}>{r.p}</span>
              <span><span className={`chip ${r.c}`}>{r.s}</span></span>
              <span className="mut">{r.n}</span>
              <span>{r.ms}</span>
              <span className="mut">{r.tk}</span>
            </div>
          ))}
        </div>
      </div>
    </section>
  );
}
