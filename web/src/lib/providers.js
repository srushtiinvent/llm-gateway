// One place that decides how a provider is shown, so adding a provider to the
// gateway (OpenRouter, Cerebras, Mistral, an overflow key…) needs no UI changes.

// Named providers keep the colour people already associate with them; anything
// else cycles through the spare slots.
const KNOWN = { gemini: 'gem', groq: 'grq', openrouter: 'opr', cerebras: 'cer', mistral: 'mst' };
const SPARE = ['opr', 'cer', 'mst', 'ovf'];

/** CSS colour-variable suffix for a provider: "gem", "grq", "opr", … */
export function provSlot(name, all = []) {
  const n = String(name || '').toLowerCase();
  if (KNOWN[n]) return KNOWN[n];
  const others = all.map(p => (p.name || p).toLowerCase()).filter(x => !KNOWN[x]);
  const i = others.indexOf(n);
  return SPARE[(i < 0 ? others.length : i) % SPARE.length];
}

/** Full CSS colour for a provider, e.g. "var(--gem)". */
export const provColor = (name, all) => `var(--${provSlot(name, all)})`;

/** Terminal/flight-board colour code for a provider. */
export const provCode = (name, all) => (provSlot(name, all) === 'gem' ? 'gm' : 'gq');
