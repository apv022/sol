// Package-free test harness for Sol's browser JavaScript. Tests run in a real browser (the
// same engines users have), with no Node.js: open a suite's test/index.html through any static
// server, e.g. `python3 -m http.server` from the repo root.
//
// API: test(name, fn) registers a test (fn may be async); assert.{ok, equal, deepEqual,
// notEqual, rejects, throws} fail by throwing. Results render into the page and set
// document.title to "PASS n" or "FAIL n/m" so a headless run can read the outcome.

const tests = [];
// Suites may stub fetch (sol.js tests do); keep the real one for reporting results.
const realFetch = globalThis.fetch.bind(globalThis);

/** Registers a test. Top-level code in a suite may also call assert directly. */
export function test(name, fn) { tests.push({ name, fn }); }

class AssertionError extends Error {}

function fmt(v) {
  try { return JSON.stringify(v); } catch { return String(v); }
}

function same(a, b) {
  if (Object.is(a, b)) return true;
  if (typeof a !== 'object' || typeof b !== 'object' || a === null || b === null) return false;
  if (Array.isArray(a) !== Array.isArray(b)) return false;
  const ka = Object.keys(a), kb = Object.keys(b);
  if (ka.length !== kb.length) return false;
  return ka.every((k) => Object.prototype.hasOwnProperty.call(b, k) && same(a[k], b[k]));
}

/** Strict assertions, a subset of node:assert/strict with the same names. */
export const assert = {
  ok(v, msg) { if (!v) throw new AssertionError(msg || `expected truthy, got ${fmt(v)}`); },
  equal(a, b, msg) { if (!Object.is(a, b)) throw new AssertionError(msg || `${fmt(a)} !== ${fmt(b)}`); },
  notEqual(a, b, msg) { if (Object.is(a, b)) throw new AssertionError(msg || `${fmt(a)} === ${fmt(b)}`); },
  deepEqual(a, b, msg) { if (!same(a, b)) throw new AssertionError(msg || `${fmt(a)} not deep-equal ${fmt(b)}`); },
  throws(fn, check, msg) {
    try { fn(); } catch (e) {
      if (check && !check(e)) throw new AssertionError(msg || `wrong error: ${e && e.message}`);
      return;
    }
    throw new AssertionError(msg || 'expected a throw');
  },
  async rejects(p, check, msg) {
    try { await (typeof p === 'function' ? p() : p); } catch (e) {
      if (check && !check(e)) throw new AssertionError(msg || `wrong rejection: ${e && e.message}`);
      return;
    }
    throw new AssertionError(msg || 'expected a rejection');
  },
};

function line(text, ok) {
  const li = document.createElement('li');
  li.textContent = text;
  li.style.color = ok ? 'var(--green, green)' : 'var(--red, red)';
  document.getElementById('results').append(li);
}

/**
 * Runs a suite: `load` imports the suite module (so a failing top-level assertion is
 * reported), then every registered test runs in order.
 */
export async function run(load) {
  // Suite pages carry the UI origins' style policy (cmd/browsertest): an inline style
  // attribute anywhere fails the suite. CSSOM (element.style) is allowed.
  const violations = [];
  document.addEventListener('securitypolicyviolation', (e) => violations.push(`${e.violatedDirective} ${e.sample || e.blockedURI || ''}`.trim()));
  document.body.style.fontFamily = 'system-ui';
  document.body.style.padding = '16px';
  let failed = 0;
  try { await load(); line('top-level checks', true); } catch (e) { failed++; line(`top-level: ${e.message}`, false); }
  for (const t of tests) {
    try { await t.fn(); line(t.name, true); } catch (e) { failed++; line(`${t.name}: ${e.message}`, false); }
  }
  await new Promise((r) => setTimeout(r, 0)); // violation events are queued
  if (violations.length) { failed++; line(`CSP: ${violations.join('; ')}`, false); }
  const total = tests.length + 1;
  document.title = failed ? `FAIL ${failed}/${total}` : `PASS ${total}`;
  document.getElementById('summary').textContent = document.title;
  // Report to cmd/browsertest when it is the server; harmless 404/501 anywhere else.
  realFetch('/__result', { method: 'POST', body: document.title + '\n' +
    [...document.querySelectorAll('#results li')].map((li) => li.textContent).join('\n') }).catch(() => {});
}
