import React, { useEffect, useRef, useState } from 'react';
import Terminal from '../components/Terminal.jsx';
import { useGateway } from '../lib/GatewayContext.jsx';

const FAULTS = [['BREAK', 'down', 'dn'], ['SLOW', 'slow', ''], ['LIMIT', 'limit', ''], ['FIX', 'none', '']];
const HELP = {
  none: 'Healthy. Takes traffic as normal.',
  down: 'Gateway tries twice, the breaker opens, and this provider is skipped for 30 s.',
  slow: 'Answers arrive late. The gateway waits, then may give up and reroute.',
  limit: 'Provider says 429, so the gateway rests it and sends traffic elsewhere.',
};
const MISSIONS = [
  { id: 'out', ic: '✕', col: 'var(--bad)', t: 'GEMINI OUTAGE', d: 'Gemini goes down. Watch flights reroute.', f: { gemini: 'down', groq: 'none' } },
  { id: 'storm', ic: '429', col: 'var(--warn)', t: 'RATE LIMIT STORM', d: 'Gemini says too many requests and rests.', f: { gemini: 'limit', groq: 'none' } },
  { id: 'slow', ic: '◔', col: 'var(--acc)', t: 'EVERYONE IS SLOW', d: 'Both slow. Which one wins?', f: { gemini: 'slow', groq: 'slow' } },
  { id: 'black', ic: '■', col: 'var(--mut)', t: 'TOTAL BLACKOUT', d: 'Both down. See the honest error.', f: { gemini: 'down', groq: 'down' } },
];
const clock = () => new Date().toTimeString().slice(0, 8);

function Node({ i, small, big, sub, color, border }) {
  return (
    <div className="nd pop" style={{ animationDelay: `${i * 160}ms`, ...(border ? { borderColor: border } : {}) }}>
      <small>{small}</small><b style={{ color }}>{big}</b><span>{sub}</span>
    </div>
  );
}

function Chain({ last }) {
  const g = last.tried.find(t => t.provider === 'gemini');
  const q = last.tried.find(t => t.provider === 'groq');
  const sub = (t, ok, bad, none) => (t ? (t.ok ? ok : bad) : none);
  return (
    <div className="chain">
      <Node i={0} small="1 · KEY" big="CLEARED ✓" sub="key allowed" color="var(--acc)" />
      <span className="arw">→</span>
      <Node i={1} small="2 · MEMORY" big={last.cache ? 'HIT' : 'MISS'} sub={last.cache ? 'answered from memory' : 'no saved answer'} color="var(--tx)" />
      <span className="arw">→</span>
      <Node i={2} small="3 · GEMINI" big={g ? (g.ok ? 'LANDED ✓' : 'SKIPPED') : 'NOT NEEDED'} sub={sub(g, `${last.ms} ms`, 'breaker open · 28 s', last.cache ? 'cache answered' : 'groq went first')} color={g && !g.ok ? 'var(--bad)' : 'var(--gem)'} border={g && !g.ok ? 'var(--bad)' : g?.ok ? 'var(--gem)' : ''} />
      <span className="arw">→</span>
      <Node i={3} small="4 · GROQ" big={q ? (q.ok ? 'LANDED ✓' : 'FAILED') : 'NOT NEEDED'} sub={sub(q, `${last.ms} ms`, 'also down', 'gemini answered')} color={q && !q.ok ? 'var(--bad)' : 'var(--grq)'} border={q && !q.ok ? 'var(--bad)' : q?.ok ? 'var(--grq)' : ''} />
    </div>
  );
}

export default function Simulator() {
  const gw = useGateway();
  const term = useRef(null);
  const [n, setN] = useState(20);
  const [running, setRunning] = useState(false);
  const [res, setRes] = useState({ landed: 0, rerouted: 0, lost: 0, gemini: 0, groq: 0, sum: 0 });

  useEffect(() => { term.current?.log('simulator ready · break something, then launch flights', ''); }, []);

  const fault = async (name, f) => {
    const err = await gw.setFault(name, f);
    term.current?.log(`${clock()}  ${name}: fault set to ${f}${err ? '  (' + err + ')' : ''}`, err ? 'er' : f === 'none' ? 'ok' : 'wn');
  };
  const mission = async m => {
    await gw.setFault('gemini', m.f.gemini);
    await gw.setFault('groq', m.f.groq);
    term.current?.log(`${clock()}  mission: ${m.t.toLowerCase()}`, 'ac');
  };
  const launch = async () => {
    if (running) return;
    setRunning(true);
    setRes({ landed: 0, rerouted: 0, lost: 0, gemini: 0, groq: 0, sum: 0 });
    term.current?.log(`${clock()}  launching ${n} flights…`, 'ac');
    const out = await gw.launch(n, s => {
      setRes(s);
      if (s.i < 7) term.current?.log(`${clock()}  ${s.line.text}`, s.line.cls);
    });
    term.current?.log(`${clock()}  done: ${out.landed} landed, ${out.rerouted} rerouted, ${out.lost} lost`, out.lost ? 'er' : 'ok');
    setRunning(false);
  };

  const byName = Object.fromEntries(gw.providers.map(p => [p.name, p]));
  const consoleCard = (name, color) => {
    const p = byName[name];
    const f = p?.fault || 'none';
    return (
      <div className="card pad rv">
        <div className="ttl"><span style={{ color }}>{name.toUpperCase()} CONSOLE</span><span className={`chip ${f === 'none' ? 'ok' : f === 'slow' || f === 'limit' ? 'wn' : 'bd'}`}>FAULT · {f.toUpperCase()}</span></div>
        <div style={{ display: 'flex', gap: 8 }}>
          {FAULTS.map(([label, v, c]) => <div key={v} className={`btn2 ${c} ${f === v ? 'on' : ''}`} style={{ flex: 1 }} onClick={() => fault(name, v)}>{label}</div>)}
        </div>
        <div className="mut" style={{ font: '600 11.5px/1.5 var(--sans)', marginTop: 12 }}>{HELP[f]}</div>
      </div>
    );
  };

  return (
    <section className="page on" id="pg-sim">
      {gw.mode === 'live' && (
        <div className="card pad rv" style={{ marginBottom: 16, font: '600 12px var(--sans)' }}>
          <span className="chip ok">YOUR OWN SANDBOX</span>&nbsp; Faults you set here apply only to you, and the {n} flights run in your browser, so nobody's free quota is spent. For real requests through the gateway, use the Play page.
        </div>
      )}
      <div className="g2" style={{ gridTemplateColumns: '1fr 310px', alignItems: 'start' }}>
        <div className="g2" style={{ alignContent: 'start' }}>
          <div className="g2" style={{ gridTemplateColumns: '1fr 1fr' }}>
            {consoleCard('gemini', 'var(--gem)')}
            {consoleCard('groq', 'var(--grq)')}
          </div>
          <div className="rv"><Terminal ref={term} title="TOWER LOG · SIMULATOR" minHeight={250} max={12} style={{ maxHeight: 330, overflow: 'hidden' }} /></div>
          <div className="card pad rv">
            <div className="ttl"><span>LAST FLIGHT · STEP BY STEP</span></div>
            <Chain key={JSON.stringify(gw.last)} last={gw.last} />
          </div>
        </div>
        <div className="g2" style={{ alignContent: 'start' }}>
          <div className="card pad rv">
            <div className="ttl"><span>MISSIONS · ONE CLICK</span></div>
            {MISSIONS.map((m, i) => (
              <div key={m.id} className="mission" style={i === MISSIONS.length - 1 ? { marginBottom: 0 } : undefined} onClick={() => mission(m)}>
                <div className="ic" style={{ color: m.col }}>{m.ic}</div>
                <div><b>{m.t}</b><span>{m.d}</span></div>
              </div>
            ))}
          </div>
          <div className="card pad rv">
            <div className="ttl"><span>SEND TRAFFIC</span><b style={{ font: '800 16px var(--sans)', color: 'var(--tx)', letterSpacing: 0 }}>{n}</b></div>
            <input type="range" min="5" max="60" value={n} onChange={e => setN(+e.target.value)} />
            <div className={`btn2 pri ${running ? 'dis' : ''}`} style={{ marginTop: 14, padding: 13, fontSize: 12.5, letterSpacing: '.14em' }} onClick={launch}>LAUNCH {n} FLIGHTS</div>
            <div className="mini4">
              <div>LANDED<b style={{ color: 'var(--acc)' }}>{res.landed}</b></div>
              <div>REROUTED<b>{res.rerouted}</b></div>
              <div>LOST<b style={{ color: 'var(--bad)' }}>{res.lost}</b></div>
              <div>AVG SPEED<b>{res.landed ? Math.round(res.sum / res.landed).toLocaleString() + 'ms' : '-'}</b></div>
            </div>
            <div className="splitbar"><i style={{ width: `${(100 * res.gemini) / n}%`, background: 'var(--gem)' }} /><i style={{ width: `${(100 * res.groq) / n}%`, background: 'var(--grq)' }} /></div>
            <div style={{ display: 'flex', justifyContent: 'space-between', font: '700 9px var(--mono)', color: 'var(--mut)', marginTop: 5, letterSpacing: '.1em' }}><span>GEMINI {res.gemini}</span><span>GROQ {res.groq}</span></div>
          </div>
        </div>
      </div>
    </section>
  );
}
