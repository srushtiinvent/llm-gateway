export const reduceMotion = () => window.matchMedia?.('(prefers-reduced-motion: reduce)').matches ?? false;
export const T = ms => (reduceMotion() ? 0 : ms);
export const sleep = ms => new Promise(r => setTimeout(r, T(ms)));

// "everything loads one by one": adds .in to .rv / .rvs elements with a delay each
export function stagger(list, delays) {
  list.forEach((el, i) => {
    el.classList.remove('in');
    el.style.setProperty('--d', delays ? delays[i] : i * 90);
  });
  void document.body.offsetHeight;
  requestAnimationFrame(() => list.forEach(el => el.classList.add('in')));
}
const items = () => [...document.querySelectorAll('header .rv, .tick.rv, .page.on .rv, .page.on .rvs')];
export function revealIntro() {
  const L = items();
  stagger(L, L.map((el, i) => (el.classList.contains('rvs') ? 560 + i * 40 : Math.min(i, 14) * 105)));
}
export function revealPage() {
  const L = [...document.querySelectorAll('.page.on .rv, .page.on .rvs')];
  stagger(L, L.map((el, i) => (el.classList.contains('rvs') ? 420 + i * 70 : i * 95)));
}
// ripple outwards from the theme knob
export function revealRipple(x, y) {
  const R = Math.hypot(Math.max(x, innerWidth - x), Math.max(y, innerHeight - y));
  const L = items();
  stagger(L, L.map(el => {
    const r = el.getBoundingClientRect();
    return Math.round(60 + 720 * (Math.hypot(r.left + r.width / 2 - x, r.top + r.height / 2 - y) / R) * 0.85);
  }));
}

export function countTo(el, to, fmt = v => v, ms = 800) {
  if (!el) return;
  const from = parseFloat(el.dataset.v || 0);
  el.dataset.v = to;
  const t0 = performance.now();
  (function step(n) {
    const k = Math.max(0, Math.min(1, (n - t0) / T(ms || 1)));
    el.textContent = fmt(from + (to - from) * (1 - Math.pow(1 - k, 3)));
    if (k < 1) requestAnimationFrame(step);
  })(t0);
}
