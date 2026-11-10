import React, { useEffect, useRef, useState } from 'react';
import { api } from '../lib/api.js';
import Terminal from '../components/Terminal.jsx';
import { useGateway } from '../lib/GatewayContext.jsx';

const HOST = typeof location !== 'undefined' ? location.host : 'localhost:8080';
const SCRIPT = [
  { cmd: `curl ${HOST}/v1/chat/completions -H "Authorization: Bearer gw_…"`, out: [['HTTP/1.1 200 OK', 'ok'], ['{"provider":"gemini","latency_ms":1073}', 'gm']] },
  { cmd: 'gateway status', out: [['gemini   cleared   7/10 per min', 'gm'], ['groq     cleared   27/30 per min', 'ok']] },
  { cmd: 'gateway fault gemini down', out: [['gemini grounded · traffic → groq', 'wn']] },
];
const chipClass = s => (s === 'ACTIVE' ? 'ok' : s === 'REVOKED' ? 'bd' : '');

export default function Keys() {
  const gw = useGateway();
  const term = useRef(null);
  const [name, setName] = useState('my-new-app');
  const [rpm, setRpm] = useState('20');
  const [perDay, setPerDay] = useState('500');
  const [created, setCreated] = useState('');
  const [copied, setCopied] = useState(false);
  const [error, setError] = useState('');
  const [admin, setAdmin] = useState(gw.settings.adminToken());
  const [fresh, setFresh] = useState(false);
  const live = gw.mode === 'live';
  const [myKey, setMyKey] = useState(gw.settings.gatewayKey());
  const [busy, setBusy] = useState(false);
  const [shown, setShown] = useState(false);
  const PROV = { gemini: ['GEMINI', 'aistudio.google.com/apikey'], groq: ['GROQ', 'console.groq.com/keys'], openrouter: ['OPENROUTER', 'openrouter.ai/keys'] };
  const names = (gw.rawProviders || []).map(p => p.name);
  const [vals, setVals] = useState(() => gw.settings.byok());
  const [checks, setChecks] = useState({});
  const [testing, setTesting] = useState(false);
  const saveAndTest = async () => {
    gw.setByok(vals);
    setChecks({});
    if (!Object.values(vals).some(v => v && v.trim())) return;
    setTesting(true);
    try {
      setChecks(await api.checkKeys(Object.fromEntries(Object.entries(vals).map(([k, v]) => [k, (v || '').trim()]))));
    } catch (err) {
      setError(err.message);
    }
    setTesting(false);
  };
  const forget = () => { setVals({}); setChecks({}); gw.setByok({}); };
  const owner = live && gw.adminOk; // key list and custom keys are for the gateway's owner only; visitors never see other keys // owner tools (custom keys, key table) need the admin token on a live gateway

  useEffect(() => { if (live) gw.loadUsage(); }, [live]); // eslint-disable-line react-hooks/exhaustive-deps
  const getKey = async () => {
    setError('');
    setBusy(true);
    try {
      const k = await gw.getMyKey();
      setMyKey(k);
      setShown(true);
    } catch (err) {
      setError(err.message);
    }
    setBusy(false);
  };
  const copyMine = () => {
    navigator.clipboard?.writeText(myKey).catch(() => {});
    setCopied(true);
    setTimeout(() => setCopied(false), 1600);
  };

  useEffect(() => { term.current?.play(SCRIPT); return () => term.current?.stop(); }, []);

  const burst = el => {
    const r = el.getBoundingClientRect();
    for (let i = 0; i < 14; i++) {
      const d = document.createElement('i');
      d.style.cssText = `position:fixed;left:${r.left + r.width / 2}px;top:${r.top + r.height / 2}px;width:6px;height:6px;border-radius:50%;background:var(--acc);z-index:800;pointer-events:none`;
      document.body.appendChild(d);
      const a = (Math.PI * 2 * i) / 14, dd = 40 + Math.random() * 50;
      d.animate([{ transform: 'translate(0,0)', opacity: 1 }, { transform: `translate(${Math.cos(a) * dd}px,${Math.sin(a) * dd}px) scale(.2)`, opacity: 0 }], { duration: 700, easing: 'ease-out' }).onfinish = () => d.remove();
    }
  };
  const create = async e => {
    const btn = e.currentTarget;
    setError('');
    try {
      const plain = await gw.createKey({ name: name.trim() || 'new-key', rpm, perDay });
      setCreated(plain);
      setCopied(false);
      setFresh(true);
      setTimeout(() => setFresh(false), 900);
      burst(btn);
    } catch (err) {
      setError(err.status === 401 || err.status === 403 ? 'The admin API needs a token. Paste it below.' : err.message);
    }
  };
  const copy = () => {
    navigator.clipboard?.writeText(created).catch(() => {});
    setCopied(true);
    setTimeout(() => setCopied(false), 1600);
  };

  const cols = '150px 120px 60px 70px 1fr 110px 110px 90px';
  const createCard = (
        <div className="card pad rv">
          <div className="ttl"><span>CREATE A KEY</span></div>
          <div className="lab">NAME</div>
          <input className="field" value={name} onChange={e => setName(e.target.value)} autoComplete="off" />
          <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 12, marginTop: 12 }}>
            <div><div className="lab">REQUESTS PER MINUTE</div><input className="field" value={rpm} onChange={e => setRpm(e.target.value)} /></div>
            <div><div className="lab">PROVIDER CALLS PER DAY</div><input className="field" value={perDay} onChange={e => setPerDay(e.target.value)} /></div>
          </div>
          <div className="btn2 pri" style={{ marginTop: 16, padding: 13, letterSpacing: '.14em' }} onClick={create}>CREATE KEY</div>
          {error && <div style={{ marginTop: 10, color: 'var(--bad)', font: '700 12px var(--sans)' }}>{error}</div>}
        </div>
  );

  return (
    <section className="page on" id="pg-keys">
      <div className="g2" style={{ gridTemplateColumns: '1fr 1fr' }}>
{true ? (
        <div className="card pad rv">
          <div className="ttl"><span>YOUR FREE KEY</span><span className="chip ok">NO SIGN-UP</span></div>
          {myKey ? (
            <>
              <div className="lab">YOUR KEY · STORED ONLY IN THIS BROWSER</div>
              <div style={{ display: 'flex', gap: 10, alignItems: 'center' }}>
                <div className="field" style={{ flex: 1, font: '700 12px var(--mono)', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{shown ? myKey : myKey.slice(0, 9) + '•'.repeat(24)}</div>
                <div className="btn2" style={{ padding: '12px 14px' }} onClick={() => setShown(v => !v)}>{shown ? 'HIDE' : 'SHOW'}</div>
                <div className="btn2 pri" style={{ padding: '12px 18px' }} onClick={copyMine}>{copied ? 'COPIED ✓' : 'COPY'}</div>
              </div>
              <div className="fuel" style={{ marginTop: 14 }}>
                <div className="t"><span>FLIGHTS LEFT TODAY</span><b>{gw.usage.dayLeft} / {gw.usage.dayMax}</b></div>
                <div className="tank"><i style={{ width: `${(100 * gw.usage.dayLeft) / Math.max(1, gw.usage.dayMax)}%` }} /></div>
              </div>
              <div className="mut" style={{ font: '600 11.5px/1.5 var(--sans)', marginTop: 10 }}>Up to {gw.usage.minuteMax} requests a minute. Repeated questions come from the cache and cost nothing. Resets at 00:00 UTC.</div>
              <div className="btn2" style={{ marginTop: 12, padding: 10, fontSize: 11 }} onClick={getKey}>{busy ? 'WORKING…' : 'GET A NEW KEY'}</div>
            </>
          ) : (
            <>
              <div className="mut" style={{ font: '600 13px/1.6 var(--sans)' }}>One click gives you your own key with its own daily allowance ({gw.signup?.daily_limit || 20} provider calls a day). Use it from curl, Python or any OpenAI-compatible app.</div>
              <div className="btn2 pri" style={{ marginTop: 16, padding: 13, letterSpacing: '.14em' }} onClick={getKey}>{busy ? 'WORKING…' : 'GET MY FREE KEY'}</div>
            </>
          )}
          {error && <div style={{ marginTop: 10, color: 'var(--bad)', font: '700 12px var(--sans)' }}>{error}</div>}
        </div>
        ) : createCard}
        <div className="card pad rv">
          <div className="ttl"><span>USE IT FROM CODE</span><span className="chip">OPENAI-COMPATIBLE</span></div>
          <Terminal ref={term} title="TERMINAL" minHeight={158} typingSpeed={45} delayBetweenCommands={1000} loop />
        </div>
      </div>

      {created && (
        <div className="keybox show">
          <div className="ttl" style={{ color: 'var(--acc)', marginBottom: 8 }}><span>KEY CREATED · SHOWN ONLY ONCE</span></div>
          <div style={{ display: 'flex', gap: 12, alignItems: 'center' }}>
            <div className="field" style={{ flex: 1, font: '700 13px var(--mono)', borderColor: 'var(--acc)', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{created}</div>
            <div className="btn2 pri" style={{ padding: '12px 22px' }} onClick={copy}>{copied ? 'COPIED ✓' : 'COPY'}</div>
          </div>
          <div className="mut" style={{ font: '600 11.5px var(--sans)', marginTop: 8 }}>Copy it now. We only keep a scrambled version, so it cannot be shown again.</div>
        </div>
      )}

      {live && names.length > 0 && (
        <div className="card pad rv" style={{ marginTop: 16 }}>
          <div className="ttl"><span>CONNECT YOUR OWN PROVIDER KEYS</span><span className={`chip ${gw.usingOwn ? 'ok' : ''}`}>{gw.usingOwn ? 'IN USE' : 'OPTIONAL'}</span></div>
          <div className="mut" style={{ font: '600 12.5px/1.6 var(--sans)', marginBottom: 12 }}>
            Use your own free Gemini, Groq or OpenRouter keys and your flights run on <b>your</b> provider quota, not the site's. Your keys stay in this browser and are sent only with your own requests. The gateway uses them for that one request and never stores or logs them.
          </div>
          <div style={{ display: 'grid', gap: 10 }}>
            {names.map(n => (
              <div key={n}>
                <div className="lab" style={{ display: 'flex', justifyContent: 'space-between' }}>
                  <span>{(PROV[n] || [n.toUpperCase()])[0]} KEY</span>
                  <span style={{ color: checks[n] ? (checks[n].ok ? 'var(--acc)' : 'var(--bad)') : 'var(--mut)' }}>
                    {checks[n] ? (checks[n].ok ? '✓ ' : '✕ ') + (checks[n].message || 'connected') : PROV[n] ? 'GET ONE FREE · ' + PROV[n][1] : ''}
                  </span>
                </div>
                <input className="field" type="password" autoComplete="off" spellCheck={false} placeholder={`paste your ${(PROV[n] || [n])[0].toLowerCase()} key (optional)`} value={vals[n] || ''} onChange={e => setVals(v => ({ ...v, [n]: e.target.value }))} />
              </div>
            ))}
          </div>
          <div style={{ display: 'flex', gap: 10, marginTop: 14 }}>
            <div className="btn2 pri" style={{ flex: 1, padding: 13 }} onClick={testing ? undefined : saveAndTest}>{testing ? 'TESTING…' : 'SAVE & TEST'}</div>
            <div className="btn2" style={{ padding: 13 }} onClick={forget}>FORGET MY KEYS</div>
          </div>
        </div>
      )}

      {live && (
        <div className="card pad rv" style={{ marginTop: 16 }}>
          <div className="ttl"><span>OWNER ACCESS</span><span className={`chip ${gw.adminOk ? 'ok' : ''}`}>{gw.adminOk ? 'UNLOCKED' : 'OPTIONAL'}</span></div>
          <div className="mut" style={{ font: '600 12px/1.5 var(--sans)', marginBottom: 10 }}>Only for whoever runs this gateway. Enter the ADMIN_TOKEN to create custom keys and revoke keys. Visitors never need it.</div>
          <input className="field" type="password" value={admin} placeholder="ADMIN_TOKEN (stays only in this browser)" onChange={e => { setAdmin(e.target.value); gw.settings.setAdminToken(e.target.value); gw.loadAdminKeys(); }} autoComplete="off" />
        </div>
      )}
      {live && gw.adminOk && <div style={{ marginTop: 16 }}>{createCard}</div>}

      {owner && (
      <div className="card pad rv" style={{ marginTop: 16 }}>
        <div className="ttl"><span>YOUR KEYS</span><span>{gw.keys.length} KEYS</span></div>
        <div className="rowt h" style={{ gridTemplateColumns: cols }}><span>NAME</span><span>PREFIX</span><span>PER MIN</span><span>PER DAY</span><span>USED TODAY</span><span>LAST USED</span><span>STATUS</span><span /></div>
        <div id="klist">
          {gw.keys.map((k, i) => (
            <div key={k.id} className={`rowt ${fresh && i === 0 ? 'flash' : ''}`} style={{ gridTemplateColumns: cols }}>
              <b style={{ font: '800 13px var(--sans)' }}>{k.name}</b>
              <span className="mut">{k.prefix}</span>
              <span>{k.perMin}</span>
              <span>{k.perDay}</span>
              <div><div className="bar5"><i style={{ width: `${k.usedPct}%`, background: 'var(--acc)' }} /></div><span className="mut" style={{ font: '700 9px var(--mono)' }}>{k.left} LEFT TODAY</span></div>
              <span className="mut">{k.lastUsed}</span>
              <span><span className={`chip ${chipClass(k.status)}`}>{k.status}</span></span>
              {k.status === 'REVOKED' ? <span className="mut" style={{ textAlign: 'center' }}>—</span> : <span className="btn2 dn" style={{ padding: '6px 0', fontSize: 10 }} onClick={() => gw.revokeKey(k.id)}>REVOKE</span>}
            </div>
          ))}
        </div>
      </div>
      )}
    </section>
  );
}
