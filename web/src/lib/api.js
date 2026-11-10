// ---------------------------------------------------------------------------
// Talks to the Go gateway. Everything that touches the network lives here, so if
// a JSON field name differs from what the backend really returns, this is the
// only file to edit (see the `norm*` functions).
// ---------------------------------------------------------------------------

const BASE = import.meta.env.VITE_API_BASE || ''; // '' = same origin (Vite proxy in dev, WEB_DIST in prod)

const store = {
  get(k, d = '') { try { return localStorage.getItem(k) ?? d; } catch { return d; } },
  set(k, v) { try { localStorage.setItem(k, v); } catch { /* ignore */ } },
};
export const settings = {
  gatewayKey: () => store.get('gw-key'),
  setGatewayKey: v => store.set('gw-key', v.trim()),
  clearGatewayKey: () => store.set('gw-key', ''),
  // The visitor's own provider keys, e.g. {gemini:'AIza…', groq:'gsk_…'}. Kept only in this browser.
  byok: () => { try { return JSON.parse(store.get('gw-byok') || '{}') || {}; } catch { return {}; } },
  setByok: o => store.set('gw-byok', JSON.stringify(o || {})),
  adminToken: () => store.get('gw-admin'),
  setAdminToken: v => store.set('gw-admin', v.trim()),
};

// Owner-only calls (/admin/*) need the ADMIN_TOKEN. Visitors never use these: they get their own
// free key from POST /v1/signup and read public numbers from GET /v1/public/stats.
function adminHeaders() {
  const t = settings.adminToken();
  return t ? { Authorization: `Bearer ${t}` } : {};
}

async function http(path, { method = 'GET', body, headers = {}, timeout = 8000 } = {}) {
  const ctl = new AbortController();
  const timer = setTimeout(() => ctl.abort(), timeout);
  try {
    const res = await fetch(BASE + path, {
      method,
      headers: { ...(body ? { 'Content-Type': 'application/json' } : {}), ...headers },
      body: body ? JSON.stringify(body) : undefined,
      signal: ctl.signal,
    });
    let json = null;
    try { json = await res.json(); } catch { /* empty body */ }
    if (!res.ok) {
      const msg = json?.error?.message || json?.error || json?.message || res.statusText;
      const err = new Error(typeof msg === 'string' ? msg : `HTTP ${res.status}`);
      err.status = res.status;
      err.body = json;
      err.code = json?.error?.code || '';
      throw err;
    }
    return json;
  } finally {
    clearTimeout(timer);
  }
}

// ---- small helpers to read fields without knowing the exact name --------------------------
const get = (o, path) => path.split('.').reduce((a, k) => (a == null ? undefined : a[k]), o);
const pick = (o, paths, d) => {
  for (const p of paths) {
    const v = get(o, p);
    if (v !== undefined && v !== null) return v;
  }
  return d;
};
const num = (v, d = 0) => (Number.isFinite(Number(v)) ? Number(v) : d);

// ---- normalisers: backend JSON -> the shape the UI uses -------------------------------------
// Provider names: "mock-gemini" is shown as "gemini" so the demo (MOCK_PROVIDERS=true) looks the same.
export const shortName = n => String(n || '').toLowerCase().replace(/^mock-/, '');

export function normProviders(json) {
  const arr = Array.isArray(json) ? json : json?.providers || [];
  return arr
    .map(p => {
      const id = String(pick(p, ['name', 'provider', 'id'], ''));
      if (!id) return null;
      const fault = String(pick(p, ['fault'], 'none')).toLowerCase();
      const breaker = String(pick(p, ['breaker'], 'closed')).toLowerCase().replace('_', '-');
      return {
        id, // the gateway's real name (used in the simulate header)
        name: shortName(id),
        model: String(pick(p, ['model'], '')),
        fault: fault === 'ratelimited' ? 'limit' : fault,
        breaker, // closed | open | half-open
        minuteUsed: num(pick(p, ['quota.rpm_used'], 0)),
        minuteMax: num(pick(p, ['quota.rpm_limit'], 0)) || 1000,
        dayUsed: num(pick(p, ['quota.rpd_used'], 0)),
        dayMax: num(pick(p, ['quota.rpd_limit'], 0)) || 100000,
        avgMs: num(pick(p, ['avg_latency_ms'], 0)),
        lastError: String(pick(p, ['last_error'], '') || ''),
        retryIn: num(pick(p, ['retry_in_sec', 'quota.cooldown_sec'], 0)),
        ok: num(pick(p, ['requests_ok'], 0)),
        failed: num(pick(p, ['requests_failed'], 0)),
      };
    })
    .filter(Boolean);
}

// GET /v1/public/stats
export function normStats(json) {
  const st = json?.stats || {};
  const series = Array.isArray(st.series) ? st.series : [];
  const requests = num(st.requests);
  return {
    requests,
    cachePct: Math.round(100 * num(json?.cache_hit_rate)),
    p95: Math.round(num(st.p95_latency_ms)),
    savedTokens: num(st.saved_prompt_tokens) + num(st.saved_completion_tokens),
    // one bar per time bucket: provider answers / cache answers / errors
    bars: series.map(b => {
      const cache = num(b.cache_hits), error = num(b.errors);
      return { ok: Math.max(0, num(b.requests) - cache - error), cache, error };
    }),
  };
}

const parseTried = list =>
  (Array.isArray(list) ? list : String(list || '').split(',').filter(Boolean)).map(t => {
    const [prov, ...rest] = String(t).split(':');
    const outcome = rest.join(':');
    return { provider: shortName(prov), ok: outcome === 'ok', skipped: outcome.startsWith('skipped'), outcome };
  }).filter((t, i, a) => !(i > 0 && !t.ok && !a[i - 1].ok && a[i - 1].provider === t.provider)); // a retry of the same provider shows once

// recent flights (public: no keys, no text)
export function normFlights(json) {
  const arr = Array.isArray(json) ? json : json?.recent || json?.logs || [];
  return arr.map(l => {
    const provider = shortName(pick(l, ['provider'], '')) || '-';
    const failed = String(pick(l, ['status'], 'ok')) === 'error';
    const cache = !!pick(l, ['cache_hit'], false);
    const tried = parseTried(pick(l, ['tried'], ''));
    const bad = tried.filter(t => !t.ok).map(t => t.provider);
    const rerouted = !failed && !cache && bad.length > 0;
    const ms = num(pick(l, ['latency_ms'], 0));
    const d = new Date(pick(l, ['ts', 'time'], Date.now()));
    return {
      t: Number.isNaN(d.getTime()) ? '--:--:--' : d.toTimeString().slice(0, 8),
      p: provider,
      s: failed ? 'GROUNDED' : cache ? 'CACHE HIT' : rerouted ? 'REROUTED' : 'CLEARED',
      c: failed ? 'bd' : cache ? 'ok' : rerouted ? 'gq' : provider === 'gemini' ? 'gm' : 'gq',
      n: failed ? 'no provider could answer' : cache ? 'answered from memory' : rerouted ? `tried ${bad.join(', ')} ✕ → ${provider} ✓` : 'one clean landing',
      ms: cache ? '0 ms' : `${ms.toLocaleString()} ms`,
      tk: '-',
    };
  });
}

// owner only: GET /admin/keys
export function normKeys(json) {
  const arr = Array.isArray(json) ? json : json?.keys || [];
  return arr.map(k => {
    const revoked = !!pick(k, ['revoked'], false);
    const created = pick(k, ['created_at'], null);
    return {
      id: k.id,
      name: String(pick(k, ['name'], 'key')),
      prefix: String(pick(k, ['prefix'], 'gw_')) + '…',
      perMin: num(pick(k, ['rpm_limit'], 0)),
      perDay: num(pick(k, ['daily_limit'], 0)),
      left: 0,
      usedPct: 0,
      lastUsed: created ? 'created ' + new Date(created).toLocaleDateString() : '-',
      status: revoked ? 'REVOKED' : 'ACTIVE',
    };
  });
}

// X-Provider-Key-Gemini: ... (one header per connected provider; used for that request only)
const byokHeaders = (keys = settings.byok()) =>
  Object.fromEntries(Object.entries(keys).filter(([, v]) => v).map(([n, v]) => [`X-Provider-Key-${n[0].toUpperCase()}${n.slice(1)}`, v]));

// ---- calls ------------------------------------------------------------------------------------
export const api = {
  // Is a gateway answering on this address? (/healthz is public and light.)
  async ping() {
    const j = await http('/healthz', { timeout: 2500 });
    if (j?.status !== 'ok') throw new Error('not a gateway');
  },
  async publicStats(win = '1h') {
    const j = await http(`/v1/public/stats?window=${win}`);
    return { providers: normProviders(j), stats: normStats(j), flights: normFlights(j), signup: j?.signup || {} };
  },
  // Try each key with a one-token request: {gemini:{ok,message}, ...}
  async checkKeys(keys) {
    const j = await http('/v1/byok/check', { method: 'POST', headers: byokHeaders(keys), timeout: 30000 });
    return j?.results || {};
  },
  // Visitors: a personal free key, created with one click.
  async signup() {
    const j = await http('/v1/signup', { method: 'POST' });
    return { key: j.key, rpm: j.rpm_limit, daily: j.daily_limit };
  },
  async usage() {
    const key = settings.gatewayKey();
    if (!key) return null;
    const j = await http('/v1/usage', { headers: { Authorization: `Bearer ${key}` } });
    return {
      minuteMax: num(j.rpm_limit),
      minuteLeft: j.rpm_limit > 0 ? num(j.rpm_available_now) : num(j.rpm_limit),
      dayMax: num(j.daily_limit),
      dayLeft: j.unlimited ? num(j.daily_limit) : num(j.daily_remaining),
      unlimited: !!j.unlimited,
    };
  },
  // One real request. `sim` = {providerId: 'down'|'slow'|'ratelimited'} affects ONLY this request.
  async chat(prompt, sim = {}, history = []) {
    const key = settings.gatewayKey();
    const t0 = performance.now();
    const own = byokHeaders();
    const simHeader = Object.entries(sim).filter(([, m]) => m && m !== 'none').map(([id, m]) => `${id}=${m === 'limit' ? 'ratelimited' : m}`).join(',');
    let json;
    try {
      json = await http('/v1/chat/completions', {
        method: 'POST',
        headers: { ...(key ? { Authorization: `Bearer ${key}` } : {}), ...own, ...(simHeader ? { 'X-Gateway-Simulate': simHeader } : {}) },
        timeout: 60000,
        body: { model: 'auto', messages: [...history, { role: 'user', content: prompt }] },
      });
    } catch (e) {
      e.tried = parseTried(e.body?.gateway?.tried);
      e.code = e.body?.error?.code || '';
      throw e;
    }
    const g = json?.gateway || {};
    return {
      text: json?.choices?.[0]?.message?.content || '',
      provider: shortName(g.provider || 'unknown'),
      cache: g.cache === 'hit',
      ms: num(g.latency_ms, Math.round(performance.now() - t0)),
      attempts: num(g.attempts, 1),
      tried: parseTried(g.tried),
    };
  },

  // ---- owner only (needs ADMIN_TOKEN) ----
  async keys() { return normKeys(await http('/admin/keys', { headers: adminHeaders() })); },
  async createKey({ name, rpm, perDay }) {
    const j = await http('/admin/keys', { method: 'POST', headers: adminHeaders(), body: { name, rpm, daily: perDay } });
    return j?.key || JSON.stringify(j);
  },
  async revokeKey(id) { return http(`/admin/keys/${id}/revoke`, { method: 'POST', headers: adminHeaders() }); },
};