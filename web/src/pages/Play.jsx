import React, { useEffect, useRef, useState } from 'react';
import Scene from '../components/Scene.jsx';
import Terminal from '../components/Terminal.jsx';
import { useGateway } from '../lib/GatewayContext.jsx';
import { countTo, sleep } from '../lib/fx.js';

const LANE = { gemini: '#laneG', groq: '#laneQ' };
const LANEID = { gemini: 'laneG', groq: 'laneQ' };
const NAME = { gemini: 'GEMINI', groq: 'GROQ' };
const clock = () => new Date().toTimeString().slice(0, 8);

function ProviderCard({ p, onBreak, onFix }) {
  const down = p.fault === 'down' || p.fault === 'limit' || p.breaker === 'open';
  const label = p.fault === 'limit' ? 'RATE LIMITED' : down ? 'GROUNDED' : p.fault === 'slow' ? 'SLOW' : 'CLEARED';
  const fuel = (t, used, max, warnBelow) => {
    const left = Math.max(0, max - used); // fuel left in the tank
    const pct = (100 * left) / Math.max(1, max);
    return (
      <div className="fuel">
        <div className="t"><span>{t}</span><b>{left} / {max}</b></div>
        <div className="tank"><i className={pct < warnBelow ? 'low' : ''} style={{ width: `${pct}%` }} /></div>
      </div>
    );
  };
  return (
    <div className="rv rvw">
    <div id={p.name === 'gemini' ? 'pG' : 'pQ'} className={`card pv ${down ? 'down' : ''}`}>
      <div className="hd"><span className="lamp" /><b className="n">{NAME[p.name] || p.name.toUpperCase()}</b><span className="st">{label}</span></div>
      {p.own ? (
        <div className="mut" style={{ font: '700 10px/1.6 var(--mono)', letterSpacing: '.08em', padding: '10px 0 6px' }}>USING YOUR OWN KEY · YOUR PROVIDER ACCOUNT SETS THE LIMITS</div>
      ) : (
        <>
          {fuel('PER MINUTE', p.minuteUsed, p.minuteMax, 30)}
          {fuel('PER DAY', p.dayUsed, p.dayMax, 20)}
        </>
      )}
      <div className="btns">
        <div className="btn brk" onClick={onBreak}>BREAK</div>
        <div className="btn fix" onClick={onFix}>FIX</div>
      </div>
    </div>
    </div>
  );
}

export default function Play() {
  const gw = useGateway();
  const scene = useRef(null);
  const term = useRef(null);
  const reqRef = useRef(null);
  const cacheRef = useRef(null);
  const p95Ref = useRef(null);
  const msRef = useRef(null);
  const [prompt, setPrompt] = useState('Explain a circuit breaker in one sentence.');
  const [busy, setBusy] = useState(false);
  const [history, setHistory] = useState([]); // the conversation so far, sent with every question
  const [ticket, setTicket] = useState(null); // {who, tag, tried, text, done, error}

  useEffect(() => {
    if (gw.mode === 'checking') return;
    term.current?.log('gateway ready · ' + (gw.mode === 'live' ? 'connected to the Go gateway' : 'demo mode (no backend found)'), 'ok');
  }, [gw.mode]);
  useEffect(() => {
    countTo(reqRef.current, gw.stats.requests, v => Math.round(v).toLocaleString(), 900);
    countTo(cacheRef.current, gw.stats.cachePct, v => Math.round(v) + '%', 900);
    countTo(p95Ref.current, gw.stats.p95, v => Math.round(v) + 'ms', 900);
  }, [gw.stats]);

  const bounce = async (name, f) => {
    const lane = LANE[name];
    if (!lane) return;
    scene.current.lane(LANEID[name], 'act');
    await scene.current.fly(lane, 0, 0.62, 650);
    const pt = scene.current.point(lane, 0.62);
    scene.current.burst(pt.x, pt.y, 'var(--bad)', 12);
    scene.current.shake(name);
    await sleep(220);
    await scene.current.fly(lane, 0.62, 0, 420);
    scene.current.laneRemove(LANEID[name], 'act');
    scene.current.laneBad(LANEID[name], true);
  };

  const takeoff = async e => {
    e?.preventDefault();
    if (busy) return;
    setBusy(true);
    setTicket(null);
    const S = scene.current;
    const asked = prompt;
    const request = gw.chat(asked, history.slice(-12)).then(r => ({ r }), error => ({ error }));

    S.lane('laneA', 'act');
    await S.fly('#laneA', 0, 1, 900);
    S.burst(450, 225);
    S.hubFlash();
    S.lane('laneA');
    const { r, error } = await request;

    if (error) {
      for (const t of (error.tried?.length ? error.tried : error.status === 503 ? gw.providers.map(p => ({ provider: p.name })) : [])) await bounce(t.provider);
      S.hidePlane();
      gw.failedChat(error.tried || []);
      term.current?.log(`${clock()}  POST /v1/chat/completions  ${error.status || 'error'} ${error.message}`.slice(0, 110), 'er');
      const limited = error.status === 429 && !(error.tried || []).length && !['free_flights_used'].includes(error.code);
      const who = ['own_keys_required', 'free_flights_used'].includes(error.code) ? 'ADD YOUR OWN KEY' : error.code === 'signup_limit' ? 'NO MORE FREE KEYS TODAY' : limited ? 'YOU HIT YOUR LIMIT' : error.status === 401 ? 'KEY PROBLEM' : 'NO PROVIDER COULD ANSWER';
      const tag = ['own_keys_required', 'free_flights_used'].includes(error.code) ? 'FREE FLIGHT USED' : error.code === 'signup_limit' ? 'TRY TOMORROW' : limited ? 'SLOW DOWN' : error.status === 401 ? 'TRY AGAIN' : 'ALL GROUNDED';
      setTicket({ who, tag, tried: (error.tried || []).map(t => t.provider + ' ✕').join('  ') || '-', text: error.message, done: true, error: true });
      setBusy(false);
      return;
    }

    let landed = false;
    for (const t of r.tried) {
      if (!LANE[t.provider]) continue;
      if (!t.ok) { await bounce(t.provider); continue; }
      const lane = LANE[t.provider];
      S.laneBad(LANEID[t.provider], false);
      S.lane(LANEID[t.provider], 'act');
      await S.fly(lane, 0, 1, gw.providers.find(p => p.name === t.provider)?.fault === 'slow' ? 1700 : 800);
      const end = S.point(lane, 1);
      S.burst(end.x, end.y);
      S.hidePlane();
      landed = true;
    }
    if (!landed) S.hidePlane();
    if (r.cache) S.burst(450, 225, 'var(--acc)', 14);

    const rerouted = r.tried.some(t => !t.ok);
    setTicket({
      who: r.cache ? 'ANSWERED FROM MEMORY' : 'ANSWERED BY ' + r.provider.toUpperCase(),
      tag: r.cache ? 'CACHE HIT' : rerouted ? 'REROUTED' : 'CLEARED',
      tried: r.tried.map(t => `${t.provider} ${t.ok ? '✓' : '✕'}`).join('  ') || 'cache',
      text: '',
      done: false,
    });
    term.current?.log(`${clock()}  POST /v1/chat/completions  ${r.provider}  ${r.ms}ms${rerouted ? '  rerouted' : ''}`, rerouted ? 'ac' : 'ok');
    requestAnimationFrame(() => countTo(msRef.current, r.ms, v => Math.round(v).toLocaleString() + ' ms', 900));
    const txt = r.text || '(empty answer)';
    if (r.text) setHistory(h => [...h, { role: 'user', content: asked }, { role: 'assistant', content: r.text }].slice(-12)); // the next question carries this conversation
    for (let i = 0; i <= txt.length; i += 2) {
      setTicket(tk => tk && { ...tk, text: txt.slice(0, i) });
      await new Promise(res => setTimeout(res, 14));
    }
    setTicket(tk => tk && { ...tk, text: txt, done: true });
    setBusy(false);
  };

  return (
    <section className="page on" id="pg-play">
      <main>
        <div>
          <div className="card stage rv">
            <div className="hud">
              <div>REQUESTS<b ref={reqRef}>0</b></div>
              <div>CACHE HITS<b ref={cacheRef}>0%</b></div>
              <div>P95 SPEED<b ref={p95Ref}>0ms</b></div>
            </div>
            <Scene ref={scene} providers={gw.providers} />
            <div className="hint">{gw.mode === 'live' ? 'TIP: BREAK A PROVIDER (ONLY FOR YOU), THEN TAKE OFF' : 'TIP: BREAK A PROVIDER, THEN TAKE OFF'}</div>
          </div>

          <div className="card chat rv">
            {ticket && (
              <div className={`ticket show ${ticket.done ? 'done' : ''} ${ticket.error ? 'err' : ''}`}>
                <div className="a">
                  <div className="hd2"><span>{ticket.who}</span><em>{ticket.tag}</em></div>
                  <p>{ticket.text}</p>
                </div>
                <div className="b">
                  <div>SPEED<b ref={msRef}>0 ms</b></div>
                  <div>TRIED<b style={{ fontSize: 13 }}>{ticket.tried}</b></div>
                </div>
              </div>
            )}
            {gw.requireOwn && (
              <div className="mut" style={{ font: '700 12px/1.5 var(--sans)', margin: '0 2px 10px', padding: '10px 12px', border: '1px dashed var(--acc)', borderRadius: 12 }}>
                {gw.usage.dayLeft <= 0 ? 'You used your free flight on the site\u2019s keys.' : 'To fly, connect your own free Gemini or Groq key.'} Connect your own free key to keep going. It takes a minute and the quota is yours.{' '}
                <span style={{ color: 'var(--acc)', cursor: 'pointer', textDecoration: 'underline' }} onClick={() => window.dispatchEvent(new CustomEvent('gw-nav', { detail: 'keys' }))}>Open the Keys page →</span>
              </div>
            )}
            {gw.mode === 'live' && !gw.requireOwn && !gw.usingOwn && gw.signup?.daily_limit > 0 && gw.signup.daily_limit <= 3 && (
              <div className="mut" style={{ font: '600 11.5px/1.5 var(--sans)', margin: '0 2px 10px' }}>
                You get {gw.usage.dayLeft} free {gw.usage.dayLeft === 1 ? 'flight' : 'flights'} on the site&apos;s keys. For more, connect your own free key on the Keys page.
              </div>
            )}
            <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', margin: '0 2px 8px', font: '700 10px var(--mono)', letterSpacing: '.1em', color: 'var(--mut)' }}>
              <span>{history.length ? `CONVERSATION · ${history.length / 2} ${history.length / 2 === 1 ? 'TURN' : 'TURNS'} REMEMBERED` : 'NEW CONVERSATION · ASK FOLLOW-UPS, IT REMEMBERS'}</span>
              {history.length > 0 && !busy && <span style={{ cursor: 'pointer', color: 'var(--acc)' }} onClick={() => { setHistory([]); setTicket(null); }}>NEW CHAT ↺</span>}
            </div>
            <form className="inp" onSubmit={takeoff}>
              <input value={prompt} onChange={e => setPrompt(e.target.value)} autoComplete="off" placeholder="Ask something…" />
              <button className="go" disabled={busy}>TAKE OFF</button>
            </form>
            {gw.mode === 'live' && !gw.settings.gatewayKey() && !busy && (
              <div className="mut" style={{ font: '600 11px var(--sans)', marginTop: 8 }}>Your free key is made automatically on your first take off and stays only in this browser.</div>
            )}
          </div>
        </div>

        <aside className="side">
          {gw.providers.map(p => (
            <ProviderCard key={p.name} p={p} onBreak={() => gw.setFault(p.name, 'down')} onFix={() => gw.setFault(p.name, 'none')} />
          ))}
          <div className="card pv rv">
            <div className="hd"><span className="lamp" style={{ background: 'var(--acc)', boxShadow: '0 0 8px var(--glow)' }} /><b className="n">{gw.usingOwn ? 'YOUR OWN KEYS' : 'YOUR KEY'}</b><span className="st">{gw.usingOwn ? 'CONNECTED' : `${gw.usage.dayLeft} LEFT TODAY`}</span></div>
            {gw.usingOwn ? (
              <div className="mut" style={{ font: '600 11.5px/1.5 var(--sans)', marginTop: 8 }}>Requests use your own provider keys, so the site's free allowance is not touched.</div>
            ) : (
            <div className="fuel">
              <div className="t"><span>THIS MINUTE</span><b>{gw.usage.minuteLeft} / {gw.usage.minuteMax}</b></div>
              <div className="tank"><i className={gw.usage.minuteLeft < 5 ? 'low' : ''} style={{ width: `${(100 * gw.usage.minuteLeft) / Math.max(1, gw.usage.minuteMax)}%` }} /></div>
            </div>
            )}
          </div>
        </aside>
      </main>
      <Terminal ref={term} title="TOWER LOG · LIVE" minHeight={96} max={6} className="rv" style={{ marginTop: 16 }} />
    </section>
  );
}