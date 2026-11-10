import React, { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from 'react';
import { api, settings } from './api.js';

const Ctx = createContext(null);
export const useGateway = () => useContext(Ctx);

const clock = () => new Date().toTimeString().slice(0, 8);
const rnd = (a, b) => a + Math.random() * (b - a);
const isDown = p => p && (p.fault === 'down' || p.fault === 'limit' || p.breaker === 'open');

const DEMO_PROVIDERS = () => [
  { name: 'gemini', fault: 'none', breaker: 'closed', minuteUsed: 3, minuteMax: 10, dayUsed: 52, dayMax: 250, avgMs: 1073, lastError: '', retryIn: 0 },
  { name: 'groq', fault: 'none', breaker: 'closed', minuteUsed: 3, minuteMax: 30, dayUsed: 59, dayMax: 1000, avgMs: 646, lastError: '', retryIn: 0 },
];
const DEMO_KEYS = () => [
  { id: 'k1', name: 'example-key-1', prefix: 'gw_0f241c…', perMin: 20, perDay: 500, left: 462, usedPct: 92, lastUsed: '2 min ago', status: 'ACTIVE' },
  { id: 'k2', name: 'example-key-2', prefix: 'gw_91ab7e…', perMin: 10, perDay: 200, left: 38, usedPct: 19, lastUsed: '1 h ago', status: 'ACTIVE' },
  { id: 'k3', name: 'example-key-3', prefix: 'gw_5c0d12…', perMin: 30, perDay: 1000, left: 1000, usedPct: 0, lastUsed: 'never', status: 'UNUSED' },
  { id: 'k4', name: 'example-key-4', prefix: 'gw_e7712a…', perMin: 20, perDay: 500, left: 500, usedPct: 0, lastUsed: '3 days ago', status: 'REVOKED' },
];
const DEMO_BOARD = () => [
  { t: '11:49:12', p: 'groq', s: 'REROUTED', c: 'gq', n: 'tried gemini ✕ → groq ✓', ms: '904 ms', tk: '216' },
  { t: '11:49:05', p: 'gemini', s: 'CACHE HIT', c: 'ok', n: 'answered from memory', ms: '0 ms', tk: 'saved 190' },
  { t: '11:48:51', p: 'gemini', s: 'CLEARED', c: 'gm', n: 'one clean landing', ms: '1,073 ms', tk: '37' },
  { t: '11:48:40', p: 'gemini', s: 'GROUNDED', c: 'bd', n: 'timeout, retried once', ms: '25.0 s', tk: 'error' },
];
const demoBars = (seed = 3) => {
  let s = seed * 7919;
  const r = () => ((s = (s * 16807) % 2147483647) / 2147483647);
  return Array.from({ length: 48 }, (_, i) => ({
    ok: 28 + 20 * Math.sin(i / 5 + seed) + r() * 14,
    cache: 8 + 6 * Math.sin(i / 4) + r() * 6,
    error: r() > 0.94 ? 6 : 0,
  }));
};

export function GatewayProvider({ children }) {
  const [mode, setMode] = useState('checking'); // checking | live | demo
  const [providers, setProviders] = useState(DEMO_PROVIDERS);
  const [keys, setKeys] = useState(DEMO_KEYS);
  const [board, setBoard] = useState(DEMO_BOARD);
  const [stats, setStats] = useState({ requests: 1284, cachePct: 38, p95: 612 });
  const [overview, setOverview] = useState({ bars: demoBars(3), savedTokens: 1420 });
  const [usage, setUsage] = useState({ minuteLeft: 14, minuteMax: 20, dayLeft: 462, dayMax: 500 });
  const [last, setLast] = useState({ tried: [{ provider: 'gemini', ok: false }, { provider: 'groq', ok: true }], provider: 'groq', ms: 904, cache: false });
  const [byok, setByokState] = useState(() => settings.byok()); // visitor's own provider keys
  const [sim, setSim] = useState({}); // live mode: this visitor's own failure tests {name: fault}
  const [signup, setSignup] = useState({ enabled: true });
  const [adminOk, setAdminOk] = useState(false);
  const cache = useRef(new Map());
  const modeRef = useRef('checking');
  modeRef.current = mode;
  // what the visitor sees: real provider state, with their personal test faults laid on top (live mode)
  const usingOwn = mode === 'live' && Object.values(byok).some(Boolean);
  const viewProviders = useMemo(
    () => (usingOwn ? providers.filter(p => byok[p.name]).map(p => ({ ...p, own: true, breaker: 'closed', retryIn: 0, lastError: '', fault: 'none', avgMs: 0, ok: 0, failed: 0 })) : providers).map(p => {
      const f = mode === 'live' ? sim[p.name] : null;
      if (!f) return p;
      const bad = f === 'down' || f === 'limit';
      return { ...p, fault: f, breaker: bad ? 'open' : f === 'none' ? p.breaker : 'closed', retryIn: bad ? 30 : p.retryIn, lastError: f === 'limit' ? '429 (your test)' : f === 'down' ? '503 (your test)' : '' };
    }),
    [providers, sim, mode, usingOwn, byok],
  );
  const provRef = useRef(viewProviders);
  provRef.current = viewProviders;
  const byokRef = useRef(byok);
  byokRef.current = byok;
  const providersRef = useRef(providers);
  providersRef.current = providers;
  const simRef = useRef(sim);
  simRef.current = sim;

  // ---- decide live vs demo on start ---------------------------------------------------------
  useEffect(() => {
    let alive = true;
    api.ping().then(
      () => alive && setMode('live'),
      () => alive && setMode('demo'),
    );
    return () => { alive = false; };
  }, []);

  const winRef = useRef('1h');
  const loadUsage = useCallback(async () => {
    if (modeRef.current !== 'live' || !settings.gatewayKey()) return;
    try {
      const u = await api.usage();
      if (u) setUsage(u);
    } catch (e) {
      if (e.status === 401) settings.clearGatewayKey(); // key was revoked or the server was reset: a new one is made on next use
    }
  }, []);

  const refresh = useCallback(async () => {
    if (modeRef.current !== 'live') return;
    try {
      const r = await api.publicStats(winRef.current);
      if (r.providers.length) setProviders(r.providers);
      setStats({ requests: r.stats.requests, cachePct: r.stats.cachePct, p95: r.stats.p95 });
      setOverview({ bars: r.stats.bars, savedTokens: r.stats.savedTokens });
      setBoard(r.flights.slice(0, 7));
      setSignup(r.signup);
      if (!settings.gatewayKey() && r.signup?.daily_limit) { // no key yet: show what a new visitor will get
        setUsage({ minuteMax: r.signup.rpm_limit, minuteLeft: r.signup.rpm_limit, dayMax: r.signup.daily_limit, dayLeft: r.signup.daily_limit });
      }
    } catch { /* keep last values */ }
    loadUsage();
  }, [loadUsage]);

  const loadAdminKeys = useCallback(async () => {
    if (modeRef.current !== 'live' || !settings.adminToken()) { setAdminOk(false); return; }
    try { setKeys(await api.keys()); setAdminOk(true); } catch { setAdminOk(false); }
  }, []);

  const loadOverview = useCallback(async win => {
    winRef.current = win;
    if (modeRef.current !== 'live') {
      setOverview(o => ({ ...o, bars: demoBars({ '1h': 3, '6h': 9, '24h': 17, '7d': 29 }[win] || 3) }));
      return;
    }
    refresh();
  }, [refresh]);

  useEffect(() => {
    if (mode !== 'live') return undefined;
    refresh();
    loadAdminKeys();
    const id = setInterval(refresh, 4000);
    return () => clearInterval(id);
  }, [mode, refresh, loadAdminKeys]);

  // ---- faults ---------------------------------------------------------------------------------
  const setFault = useCallback(async (name, fault) => {
    if (modeRef.current === 'live') {
      // Live: a personal test. It is sent with this visitor's own requests only, never changes the shared gateway.
      setSim(m => ({ ...m, [name]: fault === 'none' ? undefined : fault }));
      return null;
    }
    setProviders(ps => ps.map(p => (p.name === name ? { ...p, fault, breaker: fault === 'down' || fault === 'limit' ? 'open' : 'closed', lastError: fault === 'limit' ? '429 too many requests' : fault === 'down' ? 'timeout waiting for provider' : '', retryIn: fault === 'down' || fault === 'limit' ? 28 : 0 } : p)));
    return null;
  }, []);

  // A visitor's own free key: made automatically the first time it is needed.
  const ensureKey = useCallback(async () => {
    if (settings.gatewayKey()) return settings.gatewayKey();
    const r = await api.signup();
    settings.setGatewayKey(r.key);
    setUsage(u => ({ ...u, minuteMax: r.rpm, minuteLeft: r.rpm, dayMax: r.daily, dayLeft: r.daily }));
    return r.key;
  }, []);

  // ---- one request --------------------------------------------------------------------------------
  const addBoard = row => setBoard(b => [{ t: clock(), ...row }, ...b].slice(0, 7));

  const demoChat = useCallback(async (prompt, history = []) => {
    const ps = provRef.current;
    const ckey = JSON.stringify([history, prompt]); // same question in a different conversation is a different cache entry
    if (cache.current.has(ckey)) {
      await new Promise(r => setTimeout(r, 120));
      return { text: cache.current.get(ckey), provider: 'cache', cache: true, ms: 0, attempts: 0, tried: [] };
    }
    const tried = [];
    let served = null;
    for (const p of ps) {
      if (isDown(p)) { tried.push({ provider: p.name, ok: false }); continue; }
      tried.push({ provider: p.name, ok: true });
      served = p;
      break;
    }
    if (!served) {
      const e = new Error('All providers failed. The app gets a clear 503 instead of hanging.');
      e.status = 503; e.tried = tried;
      throw e;
    }
    const ms = Math.round((served.avgMs || 800) * (served.fault === 'slow' ? 4 : 1) * rnd(0.9, 1.1));
    // demo only: a tiny pretend memory so the conversation feature can be tried without a backend
    const said = [...history].reverse().map(m => /my name is (\w+)/i.exec(m.content || '')).find(Boolean);
    const text = /(what('| i)?s|what is|tell me) my name/i.test(prompt) ? (said ? `Your name is ${said[1]}.` : "I don't know your name yet. Tell me and I'll remember it.") : 'A circuit breaker stops calling a failing service for a short while, so the service can recover instead of being overwhelmed.';
    cache.current.set(ckey, text);
    setProviders(list => list.map(p => (p.name === served.name ? { ...p, minuteUsed: Math.min(p.minuteMax, p.minuteUsed + 1), dayUsed: p.dayUsed + 1 } : p)));
    return { text, provider: served.name, cache: false, ms, attempts: tried.length, tried };
  }, []);

  const chat = useCallback(async (prompt, history = []) => {
    const live = modeRef.current === 'live';
    let res;
    if (live) {
      await ensureKey();
      const plan = Object.fromEntries(providersRef.current.filter(p => simRef.current[p.name] && (!Object.values(byokRef.current).some(Boolean) || byokRef.current[p.name])).map(p => [p.id, simRef.current[p.name]]));
      try {
        res = await api.chat(prompt, plan, history);
      } catch (e) {
        if (e.status === 401) settings.clearGatewayKey(); // stale key: next try makes a fresh one
        loadUsage();
        throw e;
      }
    } else {
      res = await demoChat(prompt, history);
    }
    setStats(s => ({ ...s, requests: s.requests + 1 }));
    if (live) loadUsage();
    else setUsage(u => ({ ...u, minuteLeft: Math.max(0, u.minuteLeft - 1), dayLeft: Math.max(0, u.dayLeft - (res.cache ? 0 : 1)) }));
    setLast({ tried: res.tried, provider: res.provider, ms: res.ms, cache: res.cache });
    const failed = res.tried.filter(t => !t.ok).map(t => t.provider);
    addBoard({
      p: res.provider,
      s: res.cache ? 'CACHE HIT' : failed.length ? 'REROUTED' : 'CLEARED',
      c: res.cache ? 'ok' : failed.length ? 'gq' : res.provider === 'gemini' ? 'gm' : 'gq',
      n: res.cache ? 'answered from memory' : failed.length ? `tried ${failed.join(', ')} ✕ → ${res.provider} ✓` : 'one clean landing',
      ms: `${res.ms.toLocaleString()} ms`,
      tk: res.cache ? 'saved' : String(Math.round(rnd(60, 240))),
    });
    if (modeRef.current === 'live') refresh();
    return res;
  }, [demoChat, refresh, ensureKey, loadUsage]);

  const failedChat = useCallback((tried = []) => {
    setLast({ tried, provider: null, ms: 0, cache: false });
    addBoard({ p: '-', s: 'GROUNDED', c: 'bd', n: 'every provider failed → 503', ms: '-', tk: 'error' });
  }, []);

  // ---- batch of flights (simulator) -----------------------------------------------------------
  const launch = useCallback(async (n, onStep) => {
    const out = { landed: 0, rerouted: 0, lost: 0, gemini: 0, groq: 0, sum: 0 };
    for (let i = 0; i < n; i++) {
      let line;
      try {
        const r = await demoLaunchOne(provRef.current); // runs in the browser: never spends anyone's free quota
        out.landed++;
        if (r.tried.some(t => !t.ok)) out.rerouted++;
        if (r.provider === 'gemini') out.gemini++; else out.groq++;
        out.sum += r.ms;
        line = { cls: r.tried.some(t => !t.ok) ? 'ac' : 'ok', text: `#${String(i + 1).padStart(2, '0')}  ${r.provider}  ${r.ms}ms${r.tried.some(t => !t.ok) ? '  rerouted' : ''}` };
      } catch (e) {
        out.lost++;
        line = { cls: 'er', text: `#${String(i + 1).padStart(2, '0')}  ${e.status || 503} ${e.message}`.slice(0, 80) };
      }
      onStep({ ...out, i, line });
      await new Promise(r => setTimeout(r, 55));
    }
    return out;
  }, []);

  // Owner (admin token) creates keys with custom limits; in demo mode this is simulated.
  const createKey = useCallback(async ({ name, rpm, perDay }) => {
    if (modeRef.current === 'live') {
      const plain = await api.createKey({ name, rpm: Number(rpm), perDay: Number(perDay) });
      await loadAdminKeys();
      return plain;
    }
    const plain = 'gw_' + [...Array(48)].map(() => '0123456789abcdef'[Math.floor(Math.random() * 16)]).join('');
    setKeys(k => [{ id: 'k' + Date.now(), name, prefix: plain.slice(0, 10) + '…', perMin: Number(rpm), perDay: Number(perDay), left: Number(perDay), usedPct: 0, lastUsed: 'never', status: 'UNUSED' }, ...k]);
    return plain;
  }, [loadAdminKeys]);

  // A visitor asks for their own free key (live mode).
  const getMyKey = useCallback(async () => {
    if (modeRef.current !== 'live') { // demo: a pretend key so the page can be tried without a backend
      const k = 'gw_' + [...Array(48)].map(() => '0123456789abcdef'[Math.floor(Math.random() * 16)]).join('');
      settings.setGatewayKey(k);
      setUsage({ minuteMax: 6, minuteLeft: 6, dayMax: 20, dayLeft: 20 });
      return k;
    }
    const r = await api.signup();
    settings.setGatewayKey(r.key);
    setUsage({ minuteMax: r.rpm, minuteLeft: r.rpm, dayMax: r.daily, dayLeft: r.daily });
    return r.key;
  }, []);

  // Connect / disconnect the visitor's own provider keys (stored only in this browser).
  const setByok = useCallback(keys => {
    const clean = Object.fromEntries(Object.entries(keys).map(([k, v]) => [k, String(v || '').trim()]).filter(([, v]) => v));
    settings.setByok(clean);
    setByokState(clean);
  }, []);

  const revokeKey = useCallback(async id => {
    if (modeRef.current === 'live') { await api.revokeKey(id); await loadAdminKeys(); return; }
    setKeys(k => k.map(x => (x.id === id ? { ...x, status: 'REVOKED', usedPct: 0 } : x)));
  }, [loadAdminKeys]);

  const value = useMemo(
    () => ({ mode, providers: viewProviders, rawProviders: providers, byok, usingOwn, setByok, signup, adminOk, loadAdminKeys, getMyKey, loadUsage, keys, board, stats, overview, usage, last, setFault, chat, failedChat, launch, createKey, revokeKey, loadOverview, refresh, isDown, settings, setMode }),
    [mode, viewProviders, providers, byok, usingOwn, setByok, signup, adminOk, loadAdminKeys, getMyKey, loadUsage, keys, board, stats, overview, usage, last, setFault, chat, failedChat, launch, createKey, revokeKey, loadOverview, refresh],
  );
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

// one simulated flight for the simulator (no cache)
async function demoLaunchOne(ps) {
  const order = Math.random() < 0.5 ? [...ps] : [...ps].reverse();
  const tried = [];
  for (const p of order) {
    if (isDown(p)) { tried.push({ provider: p.name, ok: false }); continue; }
    const ms = Math.round((p.avgMs || 800) * (p.fault === 'slow' ? rnd(3.5, 5) : rnd(0.8, 1.25)));
    tried.push({ provider: p.name, ok: true });
    return { provider: p.name, ms, tried };
  }
  const e = new Error('all providers down');
  e.status = 503;
  throw e;
}