// PDU Monitor: the single page. No framework: small helpers that build DOM nodes (text is always added as text, never as HTML).
(() => {
'use strict';

const app = document.getElementById('app');
const S = { session: null, csrf: '', settings: {}, timer: null, refresh: null, shell: null, sel: {}, banner: null, bannerTimer: null };

// ---------------------------------------------------------------- helpers
function h(tag, props, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (v === false || v == null) continue;
    if (k === 'class') el.className = v;
    else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (k === 'value') el.value = v;
    else if (k === 'checked') el.checked = !!v;
    else if (v === true) el.setAttribute(k, '');
    else el.setAttribute(k, v);
  }
  for (const kid of kids.flat(Infinity)) {
    if (kid == null || kid === false) continue;
    el.append(kid.nodeType ? kid : document.createTextNode(String(kid)));
  }
  return el;
}
// append and replaceChildren write null as the text "null": these leave such children out, as h() does.
const present = (kids) => kids.flat(Infinity).filter((k) => k != null && k !== false);
function add(el, ...kids) { el.append(...present(kids)); }
function put(el, ...kids) { el.replaceChildren(...present(kids)); }
const svgNS = 'http://www.w3.org/2000/svg';
function svg(tag, attrs, ...kids) {
  const el = document.createElementNS(svgNS, tag);
  for (const [k, v] of Object.entries(attrs || {})) el.setAttribute(k, v);
  for (const kid of kids) if (kid) el.append(kid);
  return el;
}
const nf = (n, d = 1) => (n == null ? '—' : Number(n).toFixed(d));
const fA = (n) => nf(n, 1);
const fW = (n) => (n == null ? '—' : Math.round(n).toLocaleString('en-US').replace(/,/g, ' '));
const fKW = (n) => (n == null ? '—' : (n / 1000).toFixed(2));
const gib = (b) => (b == null ? '—' : Math.round(b / 2 ** 30).toLocaleString('en-US').replace(/,/g, ' '));
const tib = (b) => (b == null ? '—' : (b / 2 ** 40).toFixed(1));
const pct = (f) => (f == null ? '—' : Math.round(f * 100) + '%');
const lim = (n) => (n === Math.round(n) ? String(n) : n.toFixed(1));
function ago(sec) {
  if (sec == null) return '—';
  if (sec < 90) return sec + ' s';
  if (sec < 5400) return Math.round(sec / 60) + ' min';
  if (sec < 172800) return Math.round(sec / 3600) + ' h';
  return Math.round(sec / 86400) + ' d';
}
function clock(ts) { return new Date(ts * 1000).toLocaleString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' }); }
function datetime(ts) { return new Date(ts).toLocaleString([], { year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit' }); }
const STATUS_TEXT = { ok: 'OK', warning: 'High', over: 'Over limit', unknown: 'No data' };
const pill = (st) => h('span', { class: 'pill ' + st }, STATUS_TEXT[st] || st);
function store(key, value) { try { if (value === undefined) return localStorage.getItem(key); localStorage.setItem(key, value); } catch (e) { return null; } }
const canOperate = () => S.session && ['operator', 'admin'].includes(S.session.user.role);
const isAdmin = () => S.session && S.session.user.role === 'admin';

class ApiError extends Error { constructor(status, message, expired) { super(message); this.status = status; this.expired = !!expired; } }
async function api(method, path, body) {
  const opt = { method, headers: {}, credentials: 'same-origin' };
  if (method !== 'GET') opt.headers['X-CSRF-Token'] = S.csrf;
  if (body !== undefined) {
    if (typeof body === 'string') { opt.body = body; opt.headers['Content-Type'] = 'application/json'; }
    else { opt.body = JSON.stringify(body); opt.headers['Content-Type'] = 'application/json'; }
  }
  const r = await fetch(path, opt);
  let data = null;
  try { data = await r.json(); } catch (e) { /* no body */ }
  if (r.status === 401 && !path.startsWith('/api/login')) { stopPolling(); S.session = null; showLogin('Your session has ended: log in again.'); throw new ApiError(401, 'log in'); }
  if (!r.ok) throw new ApiError(r.status, (data && data.error) || r.statusText, data && data.expired);
  return data;
}

// ---------------------------------------------------------------- bars and little graphics
function bar(fraction, status, thin) {
  const fill = h('i', { class: status });
  fill.style.width = fraction == null ? '0%' : Math.min(100, Math.max(0, fraction * 100)) + '%';
  return h('div', { class: 'bar' + (thin ? ' thin' : '') }, fill);
}
function sparkline(points, key, limit) {
  const vals = points.filter((p) => p[key] != null);
  const w = 600, hgt = 70;
  const root = svg('svg', { class: 'spark', viewBox: `0 0 ${w} ${hgt}`, preserveAspectRatio: 'none' });
  if (vals.length < 2) { return h('div', { class: 'muted small' }, 'History is being collected: it fills with one point a minute.'); }
  const t0 = vals[0].t, t1 = vals[vals.length - 1].t;
  const max = Math.max(...vals.map((p) => p[key]), limit || 0) * 1.1 || 1;
  const x = (t) => ((t - t0) / (t1 - t0 || 1)) * w;
  const y = (v) => hgt - (v / max) * (hgt - 4) - 2;
  const d = vals.map((p, i) => (i ? 'L' : 'M') + x(p.t).toFixed(1) + ' ' + y(p[key]).toFixed(1)).join(' ');
  root.append(svg('path', { class: 'area', d: d + ` L${w} ${hgt} L0 ${hgt} Z` }), svg('path', { class: 'line', d }));
  if (limit) root.append(svg('line', { class: 'limit', x1: 0, x2: w, y1: y(limit), y2: y(limit) }));
  return root;
}

// ---------------------------------------------------------------- dialogs and the banner
function dialog(title, body, buttons) {
  return new Promise((resolve) => {
    const dlg = h('dialog', {}, h('h2', {}, title), body, h('div', { class: 'buttons' }));
    const row = dlg.querySelector('.buttons');
    for (const b of buttons) {
      row.append(h('button', { class: b.class || '', onclick: () => { dlg.close(); resolve(b.value); } }, b.label));
    }
    dlg.addEventListener('cancel', () => resolve(null));
    dlg.addEventListener('close', () => dlg.remove());
    document.body.append(dlg);
    dlg.showModal();
  });
}
const confirmDialog = (title, body, label, danger) => dialog(title, body, [{ label: 'Cancel', value: false }, { label, value: true, class: danger ? 'danger' : 'primary' }]);
function dialogOpen() { return !!document.querySelector('dialog[open]'); }
function typing() { const a = document.activeElement; return a && ['INPUT', 'TEXTAREA', 'SELECT'].includes(a.tagName) && a.type !== 'checkbox'; }

function setBanner(b) {
  S.banner = b;
  const holder = S.shell && S.shell.banner;
  if (!holder) return;
  holder.replaceChildren();
  if (!b) return;
  holder.append(h('div', { class: 'banner ' + (b.error ? 'err' : (b.running ? '' : 'okk')), role: 'status', 'aria-live': 'polite' },
    b.running ? h('div', { class: 'spinner' }) : null,
    h('div', {}, h('div', { style: 'font-weight:650' }, b.title), h('div', { class: b.error ? 'error small' : 'muted small' }, b.error || b.detail)),
    h('div', { class: 'spacer' }),
    !b.running ? h('button', { onclick: () => setBanner(null) }, 'Close') : null));
}
async function followOperation(op) {
  setBanner(op);
  clearTimeout(S.bannerTimer);
  for (let i = 0; i < 600; i++) {
    await new Promise((r) => setTimeout(r, 700));
    let cur;
    try { cur = await api('GET', '/api/ops/' + op.id); } catch (e) { setBanner({ ...op, running: false, error: 'Lost track of the operation: ' + e.message }); return; }
    setBanner(cur);
    if (!cur.running) {
      if (S.refresh) S.refresh();
      if (!cur.error) S.bannerTimer = setTimeout(() => { if (S.banner && !S.banner.running) setBanner(null); }, 5000);
      return;
    }
  }
}
// "Restart / On / Off" for ticked ports, with the confirmation that lists them. Nothing is switched without it.
async function powerFlow(action, items) {
  if (!items.length) return;
  const ports = items.flatMap((i) => i.ports);
  const verbs = { restart: ['Restart', 'Restart'], on: ['Switch on', 'Switch on'], off: ['Switch off', 'Switch off'] };
  const delay = (S.settings && S.settings.restartDelaySeconds) || 8;
  const effects = {
    restart: `All ports are switched off, the PDUs confirm it, and after ${delay} seconds all are switched on again. The power is lost during that time.`,
    off: 'The power is cut at once.', on: 'The power is restored at once.' };
  const list = h('ul', {}, items.slice(0, 12).map((i) => h('li', {}, i.label)), items.length > 12 ? h('li', { class: 'muted' }, `… and ${items.length - 12} more`) : null);
  const body = h('div', {}, h('div', {}, `${items.length} ${items.length === 1 ? 'item' : 'items'} (${ports.length} port${ports.length === 1 ? '' : 's'}):`), list, h('div', { class: 'effect' }, effects[action]));
  const ok = await confirmDialog(`${verbs[action][0]} ${items.length} ${items.length === 1 ? 'item' : 'items'}?`, body, verbs[action][1], true);
  if (!ok) return;
  try {
    const op = await api('POST', '/api/power', { action, confirm: true, targets: ports.map((p) => ({ pdu: p.pdu, outlet: p.outlet })) });
    followOperation(op);
  } catch (e) { setBanner({ title: 'Could not start', detail: '', error: e.message, running: false }); }
}

// ---------------------------------------------------------------- shell and routing
function icon() {
  return svg('svg', { viewBox: '0 0 64 64', 'aria-hidden': 'true' }, svg('rect', { width: 64, height: 64, rx: 14, fill: '#1a2a4d' }), svg('path', { d: 'M36 8 20 36h10l-4 20 18-30H33z', fill: '#ffcc33' }));
}
function ensureShell(active) {
  if (!S.shell) {
    const nav = h('nav', { class: 'nav' });
    const who = h('div', { class: 'who' });
    const top = h('header', { class: 'topbar' }, h('a', { class: 'brand', href: '#/' }, icon(), 'PDU Monitor'), nav, h('div', { class: 'spacer' }), who);
    const main = h('main');
    const banner = h('div');
    app.replaceChildren(top, main, banner);
    S.shell = { nav, who, main, banner };
    setBanner(S.banner);
  }
  const { nav, who } = S.shell;
  put(nav, S.session.limited ? null : [
    h('a', { href: '#/', class: active === 'overview' ? 'on' : '' }, 'Overview'),
    h('a', { href: '#/hosts', class: active === 'hosts' ? 'on' : '' }, 'Hosts'),
    isAdmin() ? h('a', { href: '#/admin', class: active === 'admin' ? 'on' : '' }, 'Settings') : null]);
  put(who,
    S.session.demo ? h('span', { class: 'demo-pill' }, 'DEMO') : null,
    h('a', { href: '#/security', class: 'whoami' + (active === 'security' ? ' on' : ''), title: 'Your login: second step, passkeys' }, `${S.session.user.displayName || S.session.user.name} · ${S.session.user.role}`),
    h('button', { class: 'small', onclick: logout }, 'Log out'));
  return S.shell.main;
}
function mount(active, node) { ensureShell(active).replaceChildren(node); }

async function logout() {
  try { await api('POST', '/api/logout', {}); } catch (e) { /* already gone */ }
  stopPolling(); S.session = null; S.shell = null; showLogin();
}
function stopPolling() { clearInterval(S.timer); S.timer = null; S.refresh = null; }
function startPolling(fn) {
  stopPolling();
  S.refresh = fn;
  const every = Math.max(5, (S.settings && S.settings.pollSeconds) || 10) * 1000;
  S.timer = setInterval(() => { if (!document.hidden && !dialogOpen() && !typing()) fn(); }, every);
}
function failed(e, where) {
  if (e instanceof ApiError && e.status === 401) return;
  mount('', h('div', { class: 'card' }, h('h2', {}, 'Something went wrong'), h('p', { class: 'error' }, e.message), h('button', { onclick: route }, 'Try again')));
}

const routes = [
  [/^#?\/?$/, () => viewOverview()],
  [/^#\/rack\/(\d+)$/, (m) => viewRack(+m[1])],
  [/^#\/pdu\/(\d+)$/, (m) => viewPDU(+m[1])],
  [/^#\/hosts$/, () => viewHosts()],
  [/^#\/host\/(\d+)$/, (m) => viewHost(+m[1])],
  [/^#\/security$/, () => viewSecurity()],
  [/^#\/admin(?:\/(\w+))?$/, (m) => viewAdmin(m[1] || 'racks')],
];
function route() {
  if (!S.session) return;
  stopPolling();
  const hash = location.hash || '#/';
  if (S.session.limited && hash !== '#/security') { location.hash = '#/security'; return; } // the administrator requires a second step: only setting it up
  for (const [re, fn] of routes) {
    const m = hash.match(re);
    if (m) { fn(m).catch((e) => failed(e)); window.scrollTo(0, 0); return; }
  }
  location.hash = '#/';
}
window.addEventListener('hashchange', route);

// ---------------------------------------------------------------- login
// ---- passkeys (WebAuthn): the browser hands over binary values, the server speaks base64url
const b64u = {
  dec(text) { const t = text.replace(/-/g, '+').replace(/_/g, '/') + '='.repeat((4 - (text.length % 4)) % 4); return Uint8Array.from(atob(t), (c) => c.charCodeAt(0)).buffer; },
  enc(buf) { let t = ''; new Uint8Array(buf).forEach((x) => { t += String.fromCharCode(x); }); return btoa(t).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, ''); },
};
const passkeysInBrowser = () => !!(window.PublicKeyCredential && navigator.credentials);
// One question to the browser at a time: a password manager that never answers leaves the first one open, and the browser then says
// "A request is already pending". A new question cancels the old one, and none waits for more than two minutes.
let passkeyAsk = null;
function passkeySignal() {
  if (passkeyAsk) passkeyAsk.abort();
  const ctl = new AbortController();
  passkeyAsk = ctl;
  setTimeout(() => ctl.abort(), 120000);
  return ctl.signal;
}
function passkeyError(e) {
  if (e && e.name === 'AbortError') return 'Stopped waiting for the passkey. Try again, or use a code.';
  if (e && /pending/i.test(e.message || '')) return 'The browser is still waiting for an earlier passkey question (a password manager may hold it). Reload the page and use another method, or try again.';
  if (e && e.name === 'NotAllowedError') return 'The passkey was not used (cancelled, or it took too long). Try again.';
  if (e && e.name === 'InvalidStateError') return 'This device has a passkey for this site already.';
  if (e && e.name === 'SecurityError') return 'The browser refuses a passkey for this address: it must be the https address with the name of the server.';
  return (e && e.message) || 'The passkey did not work.';
}
// listed: ask for the passkeys of this person by id (what a security key needs); not listed: the browser shows every passkey it can find for this
// site, from the laptop, the phone, and the password managers (iCloud, LastPass, 1Password, Bitwarden…), which often do not answer a list.
async function passkeyGet(options, listed) {
  const cred = await navigator.credentials.get({ publicKey: { challenge: b64u.dec(options.challenge), rpId: options.rpId, timeout: options.timeout, userVerification: options.userVerification,
    allowCredentials: listed ? (options.allowCredentials || []).map((c) => ({ type: c.type, id: b64u.dec(c.id) })) : [] }, signal: passkeySignal() });
  return { id: b64u.enc(cred.rawId), clientDataJSON: b64u.enc(cred.response.clientDataJSON), authenticatorData: b64u.enc(cred.response.authenticatorData), signature: b64u.enc(cred.response.signature) };
}
async function passkeyCreate(options) {
  const cred = await navigator.credentials.create({ publicKey: { challenge: b64u.dec(options.challenge), rp: options.rp, timeout: options.timeout, attestation: options.attestation,
    user: { id: b64u.dec(options.user.id), name: options.user.name, displayName: options.user.displayName }, pubKeyCredParams: options.pubKeyCredParams,
    authenticatorSelection: options.authenticatorSelection, excludeCredentials: (options.excludeCredentials || []).map((c) => ({ type: c.type, id: b64u.dec(c.id) })) }, signal: passkeySignal() });
  return { id: b64u.enc(cred.rawId), clientDataJSON: b64u.enc(cred.response.clientDataJSON), attestationObject: b64u.enc(cred.response.attestationObject) };
}

function loggedIn(info) {
  S.session = info; S.csrf = info.csrf; S.settings = info.settings || {};
  route();
}

function showLogin(message) {
  S.shell = null;
  const user = h('input', { type: 'text', name: 'username', autocomplete: 'username', autofocus: true, required: true });
  const pass = h('input', { type: 'password', name: 'password', autocomplete: 'current-password', required: true });
  const err = h('div', { class: 'error', role: 'alert' }, message || '');
  const go = h('button', { class: 'primary', type: 'submit' }, 'Log in');
  const form = h('form', {}, h('label', {}, h('div', { class: 'small muted' }, 'User name'), user), h('label', {}, h('div', { class: 'small muted' }, 'Password'), pass), err, go,
    S.demoHint ? h('div', { class: 'note' }, 'Demo mode: demo / demo (admin), operator / operator, viewer / viewer') : null);
  form.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    go.disabled = true; err.textContent = '';
    try {
      const info = await api('POST', '/api/login', { username: user.value.trim(), password: pass.value });
      if (info.mfa) { showSecondStep(info); return; }
      loggedIn(info);
    } catch (e) { err.textContent = e.message; pass.value = ''; pass.focus(); } finally { go.disabled = false; }
  });
  app.replaceChildren(h('div', { class: 'login' }, h('div', { class: 'card' }, h('div', { class: 'brand' }, icon(), 'PDU Monitor'), h('p', { class: 'muted' }, 'Power and hosts of the data centre'), form)));
  user.focus();
}

// The password was right and the person has a second step: a code of the authenticator app, a passkey (fingerprint), or a recovery code.
function showSecondStep(info) {
  const m = info.methods || {};
  let useRecovery = !m.totp && !m.passkey && m.recovery;
  const code = h('input', { type: 'text', name: 'code', autocomplete: 'one-time-code', inputmode: useRecovery ? 'text' : 'numeric', required: true, autofocus: true, maxlength: 14 });
  const label = h('div', { class: 'small muted' });
  const err = h('div', { class: 'error', role: 'alert' });
  const go = h('button', { class: 'primary', type: 'submit' }, 'Continue');
  const finish = (e) => {
    if (e instanceof ApiError && e.expired) { showLogin('The login took too long or had too many wrong codes: enter the password again.'); return; }
    err.textContent = e.message;
  };
  const setLabel = () => { label.textContent = useRecovery ? 'Recovery code (one of the ten you saved)' : 'Code from the authenticator app (6 digits)'; code.setAttribute('inputmode', useRecovery ? 'text' : 'numeric'); };
  setLabel();
  const form = h('form', {}, m.totp || useRecovery ? h('label', {}, label, code) : null, err, m.totp || useRecovery ? go : null);
  form.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    go.disabled = true; err.textContent = '';
    try {
      loggedIn(await api('POST', '/api/login/mfa', { token: info.token, method: useRecovery ? 'recovery' : 'totp', code: code.value.trim() }));
    } catch (e) { code.value = ''; code.focus(); finish(e); } finally { go.disabled = false; }
  });
  const extra = h('div', { style: 'display:grid;gap:8px;margin-top:12px' });
  if (m.passkey && passkeysInBrowser()) {
    // The buttons stay usable while the browser waits: a click again cancels the old question and asks anew (a password manager that never
    // answers must not lock the person out of the other ways).
    const use = (listed) => async () => {
      err.textContent = 'Waiting for the passkey… (click again to ask again, or use a code)';
      try {
        const q = await api('POST', '/api/login/passkey/begin', { token: info.token });
        const a = await passkeyGet(q, listed);
        loggedIn(await api('POST', '/api/login/passkey/finish', { token: info.token, ...a }));
      } catch (e) { if (e instanceof ApiError) finish(e); else if (!(e && e.name === 'AbortError')) err.textContent = passkeyError(e); }
    };
    extra.append(h('button', { type: 'button', class: m.totp ? '' : 'primary', onclick: use(false) }, 'Use a passkey (fingerprint, phone, iCloud, LastPass…)'));
    extra.append(h('button', { type: 'button', class: 'link', onclick: use(true), title: 'A security key (USB, NFC) is found by its id: use this if the button above does not find it' }, 'Use a security key'));
  } else if (m.passkey) {
    extra.append(h('div', { class: 'note' }, 'This account has a passkey, but this browser or address cannot use it (it needs https and the name of the server).'));
  }
  if (m.recovery && (m.totp || m.passkey)) {
    extra.append(h('button', { type: 'button', class: 'link', onclick: () => { useRecovery = !useRecovery; setLabel(); code.value = ''; code.focus(); put(form, h('label', {}, label, code), err, go); } }, 'I lost my device: use a recovery code'));
  }
  const cancel = h('button', { type: 'button', class: 'link', onclick: () => { if (passkeyAsk) passkeyAsk.abort(); showLogin(); } }, 'Cancel');
  app.replaceChildren(h('div', { class: 'login' }, h('div', { class: 'card' }, h('div', { class: 'brand' }, icon(), 'PDU Monitor'), h('p', { class: 'muted' }, `Second step for ${info.user}`), form, extra, h('div', { style: 'margin-top:10px' }, cancel))));
  code.focus();
}

// ---------------------------------------------------------------- overview
// Mistakes in servman (two devices on one outlet) are not supposed to exist: they are the first thing on the page, one per line, until
// servman is right. After a fix, "Check again" compares once more: the list goes, or it says what is still wrong.
function servmanBanner(sm) {
  if (!sm || !sm.configured) return null;
  const when = sm.checkedAt ? ' · checked ' + clock(Math.floor(new Date(sm.checkedAt).getTime() / 1000)) : '';
  const recheck = isAdmin() ? h('button', { class: 'small', onclick: async (ev) => { ev.target.disabled = true; try { await api('POST', '/api/admin/servman/refresh', {}); } catch (e) { /* the page shows the reason */ } if (S.refresh) S.refresh(); } }, 'Check again') : null;
  const fix = isAdmin() ? h('a', { href: '#/admin/names' }, 'Names') : null;
  if (sm.conflicts && sm.conflicts.length) {
    return h('div', { class: 'notice bad conflicts', role: 'alert' },
      h('div', { style: 'display:flex;gap:12px;align-items:center;flex-wrap:wrap' }, h('strong', {}, `Servman has ${sm.conflicts.length} conflict${sm.conflicts.length === 1 ? '' : 's'}: two devices on one outlet. Fix them in servman.`), h('div', { class: 'spacer' }), fix, recheck),
      h('ul', { style: 'margin:8px 0 0;padding-left:20px' }, sm.conflicts.map((c) => h('li', {}, c.text))),
      h('div', { class: 'small muted', style: 'margin-top:4px' }, 'Nothing is written to those outlets until servman says one thing' + when));
  }
  if (sm.error) return h('div', { class: 'notice' }, `Servman could not be read, so its conflicts are not checked: ${sm.error}`, ' ', recheck);
  if (!sm.checkedAt) return null;
  return h('div', { class: 'small c-ok', style: 'margin-bottom:10px' }, '✓ Servman has no conflicts' + when);
}

async function viewOverview() {
  let data;
  const prefs = { list: store('ov.list') === '1', busiest: store('ov.busiest') === '1' };
  const rank = { over: 3, warning: 2, ok: 1, unknown: 0 };
  function draw() {
    const t = data.totals;
    let racks = data.racks.slice();
    if (prefs.busiest) racks.sort((a, b) => (rank[b.summary.attention] - rank[a.summary.attention]) || ((b.summary.fraction || -1) - (a.summary.fraction || -1)));
    const hs = data.hosts;
    const stats = h('div', { class: 'stats' },
      h('div', { class: 'stat' }, h('div', { class: 'label' }, 'Total power'), h('div', { class: 'value' }, fKW(t.watts), h('small', {}, ' kW'))),
      h('div', { class: 'stat' }, h('div', { class: 'label' }, 'Total current'), h('div', { class: 'value' }, fA(t.amps), h('small', {}, ' A'))),
      h('div', { class: 'stat' }, h('div', { class: 'label' }, 'Racks'), h('div', { class: 'value' }, t.racks)),
      h('div', { class: 'stat' }, h('div', { class: 'label' }, 'Over limit'), h('div', { class: 'value ' + (t.over ? 'c-over' : 'c-ok') }, t.over)),
      h('a', { class: 'stat', href: '#/hosts' }, h('div', { class: 'label' }, 'Hypervisors'),
        h('div', { class: 'value' }, hs.online || 0, h('small', {}, ' of ' + hs.total + ' online'),
          hs.degraded ? h('small', { class: 'c-over' }, '  · ' + hs.degraded + ' ZFS pool problem') : null,
          hs.spaceOver ? h('small', { class: 'c-over' }, '  · ' + hs.spaceOver + ' low on disk space') : null,
          hs.spaceWarn ? h('small', { class: 'c-warning' }, '  · ' + hs.spaceWarn + ' running short') : null)),
      h('div', { class: 'spacer' }),
      h('div', { class: 'stat' }, h('div', { class: 'label' }, 'Updated'), h('div', { class: 'value small', style: 'font-size:15px' }, clock(data.now))));
    const controls = h('div', { class: 'actionbar' },
      h('div', { class: 'seg' },
        h('button', { class: prefs.list ? '' : 'on', onclick: () => { prefs.list = false; store('ov.list', '0'); draw(); } }, 'Tiles'),
        h('button', { class: prefs.list ? 'on' : '', onclick: () => { prefs.list = true; store('ov.list', '1'); draw(); } }, 'List')),
      h('label', { class: 'small' }, h('input', { type: 'checkbox', checked: prefs.busiest, onchange: (e) => { prefs.busiest = e.target.checked; store('ov.busiest', prefs.busiest ? '1' : '0'); draw(); } }), ' Busiest first'));
    let body;
    if (!racks.length) {
      body = h('div', { class: 'card' }, h('h2', {}, 'No racks yet'), h('p', { class: 'muted' }, isAdmin() ? 'Add racks and PDUs in Settings, or import the settings file of the Mac application.' : 'An administrator has to add the racks.'),
        isAdmin() ? h('a', { class: 'btn primary', href: '#/admin' }, 'Open Settings') : null);
    } else if (prefs.list) {
      body = h('div', { class: 'tablewrap' }, h('table', {},
        h('thead', {}, h('tr', {}, h('th', {}, 'Rack'), h('th', { class: 'num' }, 'kW'), h('th', { class: 'num' }, 'Amps'), h('th', { class: 'num' }, 'Limit'), h('th', {}, 'Load'), h('th', {}, 'Status'), h('th', {}, 'PDUs'))),
        h('tbody', {}, racks.map((r) => h('tr', {},
          h('td', { class: 'name' }, h('a', { href: '#/rack/' + r.id }, r.name)),
          h('td', { class: 'num' }, fKW(r.summary.watts)),
          h('td', { class: 'num c-' + r.summary.status }, fA(r.summary.amps)),
          h('td', { class: 'num' }, lim(r.limit) + ' A'),
          h('td', { style: 'min-width:180px' }, h('div', { style: 'display:flex;gap:10px;align-items:center' }, h('div', { style: 'flex:1' }, bar(r.summary.fraction, r.summary.status, true)), h('span', { class: 'num small', style: 'width:42px;text-align:right' }, pct(r.summary.fraction)))),
          h('td', {}, pill(r.summary.attention)),
          h('td', { class: 'muted small' }, r.pdus.map((p) => h('a', { href: '#/pdu/' + p.id, style: 'margin-right:10px' }, p.name))))))));
    } else {
      body = h('div', { class: 'cards tiles' }, racks.map(rackTile));
    }
    mount('overview', h('div', {}, servmanBanner(data.servman), h('div', { class: 'page-head' }, h('div', { class: 'grow' }, h('h1', {}, 'Overview'))), stats, controls, body));
  }
  const load = async () => { data = await api('GET', '/api/overview'); draw(); };
  await load();
  startPolling(() => load().catch(() => {}));
}
function rackTile(r) {
  const s = r.summary;
  const offline = s.offline ? h('span', { class: 'c-warning' }, `${s.offline} of ${s.pdus} PDU offline`) : `${s.pdus} PDU`;
  const note = s.worstPdu === 'over' ? 'A PDU in this rack is over its own limit' : (s.worstPdu === 'warning' ? 'A PDU in this rack is close to its own limit' : null);
  return h('a', { class: 'card tile' + (s.attention === 'over' ? ' over' : ''), href: '#/rack/' + r.id },
    h('div', { style: 'display:flex;justify-content:space-between;align-items:center' }, h('div', { class: 'name' }, r.name), pill(s.attention)),
    h('div', { class: 'big' }, fA(s.amps), h('small', {}, ` A of ${lim(r.limit)} A`)),
    bar(s.fraction, s.status),
    h('div', { class: 'row' }, h('span', { class: 'num' }, fKW(s.watts) + ' kW'), offline),
    note ? h('div', { class: 'alert c-' + s.worstPdu }, note) : null);
}

// ---------------------------------------------------------------- one rack
async function viewRack(id) {
  let data;
  const sel = S.sel['r' + id] || (S.sel['r' + id] = new Set());
  function draw() {
    const r = data.rack, s = r.summary;
    const servers = data.servers;
    for (const k of [...sel]) if (!servers.some((x) => x.key === k)) sel.delete(k);
    const operate = canOperate();
    const chosen = () => servers.filter((x) => sel.has(x.key)).map((x) => ({ label: x.name + (x.label ? ` (${x.label})` : ''), ports: x.ports }));
    const head = h('div', { class: 'page-head' },
      h('div', { class: 'grow' }, h('div', { class: 'small' }, h('a', { href: '#/' }, '← Overview')),
        h('div', { style: 'display:flex;gap:14px;align-items:center;flex-wrap:wrap' }, h('h1', {}, 'Rack ' + r.name), pill(s.attention))),
      h('div', { style: 'text-align:right' }, h('div', { class: 'stat' }, h('div', { class: 'value c-' + (s.status === 'unknown' ? 'unknown' : s.status) }, fA(s.amps), h('small', {}, ` A of ${lim(r.limit)} A`))),
        h('div', { class: 'muted num' }, `${fKW(s.watts)} kW · ${fW(s.watts)} W`)));
    const pdus = h('div', { class: 'cards pdus' }, r.pdus.map((p) => h('a', { class: 'card pducard', href: '#/pdu/' + p.id },
      h('div', { class: 't' }, h('strong', {}, p.name), h('span', { class: 'muted small' }, p.vendor === 'apc' ? 'APC' : 'CyberPower'), h('span', { class: 'spacer' }),
        p.error ? h('span', { class: 'c-warning', title: p.error }, '⚠') : null),
      h('div', { class: 'amps c-' + p.status }, fA(p.amps), h('small', { class: 'muted', style: 'font-weight:400;font-size:13px' }, ` A of ${lim(p.maxAmps)} A · ${fW(p.watts)} W`)),
      bar(p.amps == null ? null : p.amps / p.maxAmps, p.status, true))));
    const rows = servers.map((x) => {
      const on = x.power === 'on' ? true : (x.power === 'off' ? false : null);
      return h('tr', { class: sel.has(x.key) ? 'selected' : '' },
        operate ? h('td', { class: 'chk' }, h('input', { type: 'checkbox', checked: sel.has(x.key), 'aria-label': 'Select ' + x.name, onchange: (e) => { e.target.checked ? sel.add(x.key) : sel.delete(x.key); draw(); } })) : null,
        h('td', { class: 'name' }, x.name, x.label ? h('div', { class: 'muted small', style: 'font-weight:400' }, x.label) : null,
          operate ? h('button', { class: 'link small', title: 'Give this server a name', onclick: () => rename(x) }, '✎') : null),
        h('td', { class: 'kind-' + x.kind }, x.kind === 'network' ? 'Network' : 'Server'),
        h('td', { class: 'muted' }, x.ports.map((p, i) => [i ? ' · ' : '', h('a', { href: '#/pdu/' + p.pdu }, `${p.pduName} #${p.outlet}`)])),
        h('td', {}, h('span', { class: 'dot ' + (on === true ? 'ok' : (on === false ? 'over' : '')) }), x.power === 'unknown' ? '?' : (x.power === 'mixed' ? h('span', { class: 'c-warning' }, 'partly') : (on ? 'ON' : 'OFF'))),
        h('td', { class: 'num' }, fW(x.watts)), h('td', { class: 'num' }, nf(x.amps, 2)),
        h('td', {}, x.host ? h('a', { href: '#/host/' + x.host.id }, x.host.name) : h('span', { class: 'muted' }, '—'),
          x.host ? h('span', { class: 'muted small', style: 'margin-left:8px' }, (x.host.online && x.host.idlePct != null ? `${Math.round(100 - idleShown(x.host.idlePct))}% cpu` : 'no data') + (x.host.vms != null ? ` · ${x.host.vms} VMs` : '')) : null));
    });
    const table = servers.length ? h('div', { class: 'tablewrap' }, h('table', {},
      h('thead', {}, h('tr', {}, operate ? h('th', { class: 'chk' }) : null, h('th', {}, 'Server'), h('th', {}, 'Type'), h('th', {}, 'Ports'), h('th', {}, 'State'), h('th', { class: 'num' }, 'Power (W)'), h('th', { class: 'num' }, 'Current (A)'), h('th', {}, 'Hypervisor'))),
      h('tbody', {}, rows)))
      : h('div', { class: 'card' }, h('h2', {}, 'No servers yet'), h('p', { class: 'muted' }, 'Name the outlets on the PDUs after the servers (for example 200U31). Outlets with the default name "Outlet_N" are not shown.'));
    const bars = operate && servers.length ? h('div', { class: 'actionbar' },
      h('button', { onclick: () => { servers.forEach((x) => sel.add(x.key)); draw(); } }, 'Select all'),
      h('button', { disabled: !sel.size, onclick: () => { sel.clear(); draw(); } }, 'Clear'),
      h('span', { class: 'muted' }, sel.size ? `${sel.size} selected` : 'Tick servers to switch them'), h('span', { class: 'sp' }),
      h('button', { disabled: !sel.size, onclick: () => powerFlow('restart', chosen()) }, '↻ Restart'),
      h('button', { disabled: !sel.size, onclick: () => powerFlow('on', chosen()) }, '⏻ On'),
      h('button', { disabled: !sel.size, class: 'danger', onclick: () => powerFlow('off', chosen()) }, '○ Off')) : null;
    mount('overview', h('div', {}, head, h('div', { style: 'margin-bottom:16px' }, pdus), bars, table,
      h('p', { class: 'muted' }, 'Total rack power: ', h('strong', { class: 'num' }, fW(s.watts) + ' W'), '  ', h('strong', { class: 'num c-' + s.status }, fA(s.amps) + ' A'))));
  }
  async function rename(x) {
    const input = h('input', { type: 'text', value: x.label || '', maxlength: 100, placeholder: 'For example: web-01', style: 'width:100%' });
    const res = await dialog('Name for ' + x.name, h('div', {}, h('p', { class: 'muted' }, 'Shown under the id in this view. The id on the PDU outlets does not change.'), input), [{ label: 'Cancel', value: false }, { label: 'Save', value: true, class: 'primary' }]);
    if (res) { try { await api('PUT', `/api/racks/${id}/labels`, { key: x.key, label: input.value }); await load(); } catch (e) { setBanner({ title: 'Could not save the name', error: e.message, running: false }); } }
  }
  const load = async () => { data = await api('GET', '/api/racks/' + id); draw(); };
  await load();
  startPolling(() => load().catch(() => {}));
}

// ---------------------------------------------------------------- one PDU
async function viewPDU(id) {
  let data, history = null;
  const sel = S.sel['p' + id] || (S.sel['p' + id] = new Set());
  function draw() {
    const p = data.pdu, snap = data.snapshot;
    const operate = canOperate();
    const outlets = snap ? snap.outlets : [];
    for (const n of [...sel]) if (!outlets.some((o) => o.number === n)) sel.delete(n);
    const chosen = () => outlets.filter((o) => sel.has(o.number)).map((o) => ({ label: `${o.name && !/^outlet[_ -]?\d*$/i.test(o.name) ? o.name + ' — ' : ''}${p.name} outlet ${o.number}`, ports: [{ pdu: p.id, outlet: o.number }] }));
    const head = h('div', { class: 'page-head' },
      h('div', { class: 'grow' }, h('div', { class: 'small' }, h('a', { href: '#/rack/' + p.rackId }, '← Rack ' + data.rackName)),
        h('div', { style: 'display:flex;gap:14px;align-items:baseline;flex-wrap:wrap' }, h('h1', {}, p.name), h('span', { class: 'muted' }, p.vendor === 'apc' ? 'APC' : 'CyberPower'), pill(p.status))),
      h('div', { style: 'text-align:right' }, h('div', { class: 'stat' }, h('div', { class: 'value c-' + p.status }, fA(p.amps), h('small', {}, ` A of ${lim(p.maxAmps)} A · ${fW(p.watts)} W`)))));
    const err = p.error ? h('div', { class: 'notice' }, '⚠ ' + p.error + (snap ? ' — showing the last reading' : '')) : null;
    const metered = data.hasOutletMetering;
    const rows = outlets.map((o) => h('tr', { class: sel.has(o.number) ? 'selected' : '' },
      operate ? h('td', { class: 'chk' }, h('input', { type: 'checkbox', checked: sel.has(o.number), 'aria-label': 'Select outlet ' + o.number, onchange: (e) => { e.target.checked ? sel.add(o.number) : sel.delete(o.number); draw(); } })) : null,
      h('td', { class: 'num' }, o.number),
      h('td', { class: 'name' }, /^outlet[_ -]?\d*$/i.test(o.name || '') || !o.name ? h('span', { class: 'muted', style: 'font-weight:400' }, '—') : o.name),
      h('td', { class: 'muted' }, o.bank == null ? '—' : o.bank),
      h('td', {}, h('span', { class: 'dot ' + (o.on === true ? 'ok' : (o.on === false ? 'over' : '')) }), o.on == null ? '?' : (o.on ? 'ON' : 'OFF')),
      h('td', { class: 'num' }, o.amps == null ? '—' : fA(o.amps)), h('td', { class: 'num' }, o.watts == null ? '—' : fW(o.watts))));
    const table = h('div', { class: 'tablewrap', style: p.error ? 'opacity:.6' : '' }, h('table', {},
      h('thead', {}, h('tr', {}, operate ? h('th', { class: 'chk' }) : null, h('th', { class: 'num' }, 'Outlet'), h('th', {}, 'Server'), h('th', {}, 'Bank'), h('th', {}, 'State'), h('th', { class: 'num' }, 'Current (A)'), h('th', { class: 'num' }, 'Power (W)'))),
      h('tbody', {}, rows)));
    const actions = operate && outlets.length ? h('div', { class: 'actionbar' },
      h('button', { onclick: () => { outlets.forEach((o) => sel.add(o.number)); draw(); } }, 'Select all'),
      h('button', { disabled: !sel.size, onclick: () => { sel.clear(); draw(); } }, 'Clear'),
      h('span', { class: 'muted' }, sel.size ? `${sel.size} selected` : 'Tick ports to switch them'), h('span', { class: 'sp' }),
      h('button', { disabled: !sel.size, onclick: () => powerFlow('restart', chosen()) }, '↻ Restart'),
      h('button', { disabled: !sel.size, onclick: () => powerFlow('on', chosen()) }, '⏻ On'),
      h('button', { disabled: !sel.size, class: 'danger', onclick: () => powerFlow('off', chosen()) }, '○ Off')) : null;
    const totals = snap ? h('div', { class: 'card' }, h('h3', {}, 'Totals'), h('dl', { class: 'kv', style: 'margin-top:10px' },
      snap.phases.map((ph) => [h('dt', {}, 'Phase ' + ph.number), h('dd', {}, `${fA(ph.amps)} A · ${ph.watts == null ? '—' : fW(ph.watts) + ' W'}${ph.volts ? ' · ' + Math.round(ph.volts) + ' V' : ''}`)]),
      snap.banks.map((b) => {
        const l = data.bankMaxAmps, st = l ? (b.amps > l ? 'over' : (b.amps >= l * 0.8 ? 'warning' : 'ok')) : 'unknown';
        return [h('dt', {}, 'Bank ' + b.number), h('dd', { class: l ? 'c-' + st : '' }, `${fA(b.amps)} A · ${b.watts == null ? '—' : fW(b.watts) + ' W'}${l ? ' of ' + lim(l) + ' A' : ''}`)];
      }))) : null;
    const info = snap ? h('div', { class: 'card' }, h('h3', {}, 'Device'), h('dl', { class: 'kv', style: 'margin-top:10px' },
      [['Vendor', p.vendor === 'apc' ? 'APC' : 'CyberPower'], ['Model', snap.info.model || '—'], ['Outlets', snap.info.outlets], ['Breakers', snap.info.breakers == null ? '—' : snap.info.breakers],
       ['Orientation', snap.info.orientation || '—'], ['Voltage', snap.info.lineVoltage ? Math.round(snap.info.lineVoltage) + ' V' : '—'], ['Firmware', snap.info.firmware || '—'], ['Serial', snap.info.serial || '—'],
       ['Address', `${data.host}:${data.port}`]].map(([k, v]) => [h('dt', {}, k), h('dd', {}, String(v))])),
      !metered ? h('p', { class: 'note' }, 'This PDU does not measure single outlets, only banks and the whole unit.') : null) : null;
    const hist = h('div', { class: 'card' }, h('h3', {}, 'Last 24 hours (amps)'), history ? sparkline(history, 'amps', p.maxAmps) : h('div', { class: 'muted small' }, 'Loading…'));
    mount('overview', h('div', {}, head, err, actions, table, h('div', { class: 'two', style: 'margin-top:14px' }, totals, info), h('div', { style: 'margin-top:14px' }, hist)));
  }
  const load = async () => { data = await api('GET', '/api/pdus/' + id); draw(); };
  await load();
  api('GET', `/api/pdus/${id}/history?hours=24`).then((r) => { history = r.points; if (!dialogOpen()) draw(); }).catch(() => {});
  startPolling(() => load().catch(() => {}));
}

// ---------------------------------------------------------------- hosts
function poolTotals(pools) {
  if (!pools || !pools.length) return null;
  let total = 0, avail = 0, bad = null;
  for (const p of pools) {
    total += p.usable_total != null ? p.usable_total : p.size;
    avail += p.usable_avail != null ? p.usable_avail : p.free;
    if (p.health !== 'ONLINE') bad = p.health;
  }
  return { total, avail, bad };
}
// what the host has for storage: ZFS pools (usable space) or, without them, LVM volume groups (free space of the group)
function storageOf(x) {
  const z = poolTotals(x.pools);
  if (z) return { kind: 'zfs', total: z.total, avail: z.avail, bad: z.bad };
  if (x.lvm && x.lvm.length) return { kind: 'lvm', total: x.lvm.reduce((a, g) => a + g.size, 0), avail: x.lvm.reduce((a, g) => a + g.free, 0), bad: null };
  return null;
}
// a thin pool or snapshot that is nearly full is the dangerous one: the busiest of its two percentages decides
const fillPct = (f) => Math.max(f.data_pct == null ? 0 : f.data_pct, f.meta_pct == null ? 0 : f.meta_pct);
const fillClass = (pctv) => (pctv >= 90 ? 'over' : (pctv >= 80 ? 'warning' : 'ok'));
function hostDot(x) {
  const st = !x.enabled ? 'unknown' : (!x.trusted ? 'warning' : (x.online ? 'ok' : (x.stale ? 'warning' : 'over')));
  const why = !x.enabled ? 'disabled' : (!x.trusted ? 'the key of this host is not confirmed yet' : (x.online ? 'online' : (x.error || (x.stale ? 'the data is old' : 'no answer'))));
  return h('span', { class: 'dot ' + st, title: why });
}
// CPU idle. The hosts have hyper-threads: the second thread of a core adds far less than a core, so a host that reports 50 % idle has no room
// left in practice. Two ways to look at it, chosen by every person for themselves (kept on the server): "raw" is what the host reports; "threads"
// counts 50 % reported as 0 % and 100 % as 100 % (90 % reported = 80 %). The colours mean the same in both: red when 55 % or less is reported
// (10 % in the second way), orange under 60 % reported (20 %).
const cpuMode = () => (S.session && S.session.prefs && S.session.prefs.cpuIdleMode === 'threads' ? 'threads' : 'raw');
const idleShown = (raw) => (raw == null ? null : (cpuMode() === 'threads' ? Math.max(0, (raw - 50) * 2) : raw));
const idleLevel = (raw) => (raw == null ? '' : (raw < 55 ? 'over' : (raw < 60 ? 'warning' : 'ok')));
const idleClass = (raw) => (raw == null ? '' : 'c-' + idleLevel(raw));
const idleLimits = () => (cpuMode() === 'threads' ? 'red below 10 %, orange below 20 %' : 'red below 55 %, orange below 60 %');
async function setCpuMode(mode) {
  const prefs = await api('PUT', '/api/me/prefs', { cpuIdleMode: mode });
  S.session.prefs = prefs;
}
function cpuModeControl(onChange) {
  const one = (mode, label, hint) => h('button', { class: cpuMode() === mode ? 'on' : '', title: hint, onclick: async () => {
    if (cpuMode() === mode) return;
    try { await setCpuMode(mode); } catch (e) { setBanner({ title: 'Not saved', error: e.message, running: false }); return; }
    if (onChange) onChange();
  } }, label);
  return h('div', { class: 'seg', role: 'group', 'aria-label': 'How CPU idle is shown' },
    one('raw', 'As reported', 'The idle % the host reports, 0-100'),
    one('threads', 'Threads discounted', '50 % reported counts as 0 %, 100 % as 100 %: 90 % reported shows as 80 %'));
}
const freeClass = (f) => (f == null ? '' : (f < 0.1 ? 'c-over' : (f < 0.2 ? 'c-warning' : 'c-ok')));

async function viewHosts() {
  let data;
  const sort = S.hostSort || (S.hostSort = { key: 'name', dir: 1 });
  let filter = '';
  const cols = {
    name: (x) => x.name.toLowerCase(), rack: (x) => (x.rackName || '') + (x.outletKey || ''), idle: (x) => x.idlePct, load: (x) => (x.load ? x.load[1] : null), cores: (x) => x.cores,
    used: (x) => x.memUsed, free: (x) => (x.memTotal != null ? x.memTotal - x.memUsed : null), total: (x) => x.memTotal,
    stotal: (x) => (storageOf(x) || {}).total, sfree: (x) => { const t = storageOf(x); return t ? t.avail / t.total : null; },
    zfs: (x) => ((storageOf(x) || {}).bad ? 0 : 1), vms: (x) => x.vms, watts: (x) => (x.power ? x.power.watts : null), age: (x) => x.ageSeconds };
  function draw() {
    let hosts = data.hosts.filter((x) => !filter || (x.name + ' ' + (x.rackName || '') + ' ' + (x.outletKey || '')).toLowerCase().includes(filter));
    const get = cols[sort.key];
    hosts.sort((a, b) => { const va = get(a), vb = get(b); if (va == null && vb == null) return 0; if (va == null) return 1; if (vb == null) return -1; return (va < vb ? -1 : (va > vb ? 1 : 0)) * sort.dir; });
    const th = (key, label, num) => h('th', { class: 'sortable' + (num ? ' num' : ''), onclick: () => { sort.dir = sort.key === key ? -sort.dir : 1; sort.key = key; draw(); } }, label + (sort.key === key ? (sort.dir > 0 ? ' ↑' : ' ↓') : ''));
    const group = (cls, text, span) => h('th', { class: 'group-head ' + cls, colspan: span, style: 'text-align:center' }, text);
    const rows = hosts.map((x) => {
      const st = storageOf(x);
      const freeFrac = st ? st.avail / st.total : null;
      const memFree = x.memTotal != null ? x.memTotal - x.memUsed : null;
      return h('tr', { class: x.online ? '' : 'dim' },
        h('td', { class: 'name' }, hostDot(x), h('a', { href: '#/host/' + x.id }, x.name)),
        h('td', { class: 'muted' }, x.rackName ? (x.rackId != null ? h('a', { href: '#/rack/' + x.rackId }, `${x.rackName}${x.outletKey ? ' · ' + x.outletKey : ''}`) : '') : (x.outletKey || '—')),
        h('td', { class: 'num ' + idleClass(x.idlePct), title: cpuMode() === 'threads' && x.idlePct != null ? `the host reports ${Math.round(x.idlePct)} % idle` : '' }, x.idlePct == null ? '—' : Math.round(idleShown(x.idlePct))), h('td', { class: 'num' }, x.load ? Math.round(x.load[1]) : '—'), h('td', { class: 'num' }, x.cores == null ? '—' : x.cores),
        h('td', { class: 'num' }, gib(x.memUsed)), h('td', { class: 'num' }, gib(memFree)), h('td', { class: 'num' }, gib(x.memTotal)),
        h('td', { class: 'num' }, st ? tib(st.total) : '—'),
        h('td', { class: 'num ' + freeClass(x.spaceFree != null ? x.spaceFree : freeFrac) }, st ? `${tib(st.avail)} (${Math.round(freeFrac * 100)}%)` : '—'),
        h('td', {}, st ? h('span', { class: 'pill ' + (x.space || 'unknown'), title: x.spaceWhy || '' }, st.kind === 'lvm' ? 'LVM' : (st.bad || 'ONLINE')) : h('span', { class: 'muted' }, '—')),
        h('td', { class: 'num' }, x.vms == null ? '—' : x.vms), h('td', { class: 'num' }, x.power ? fW(x.power.watts) : '—'),
        h('td', { class: 'num muted' }, x.ageSeconds == null ? (x.error ? h('span', { class: 'c-warning', title: x.error }, 'no data') : '—') : ago(x.ageSeconds)));
    });
    const search = h('input', { type: 'search', placeholder: 'Filter hosts…', value: filter, 'aria-label': 'Filter hosts', oninput: (e) => { filter = e.target.value.trim().toLowerCase(); drawBodyOnly(); } });
    function drawBodyOnly() { draw(); const s = S.shell.main.querySelector('input[type=search]'); if (s) { s.focus(); s.setSelectionRange(s.value.length, s.value.length); } }
    const bad = data.hosts.filter((x) => x.enabled && !x.online).length;
    const table = h('div', { class: 'tablewrap' }, h('table', {},
      h('thead', {}, h('tr', {}, h('th', { colspan: 2 }), group('g-compute', 'Compute', 3), group('g-mem', 'Mem', 3), group('g-storage', 'Storage', 3), group('g-info', '', 3)),
        h('tr', {}, th('name', 'Host'), th('rack', 'Rack · unit'), th('idle', cpuMode() === 'threads' ? 'Idle % (threads discounted)' : 'Idle %', 1), th('load', '5m load', 1), th('cores', 'Cores', 1), th('used', 'Used (G)', 1), th('free', 'Free (G)', 1), th('total', 'Total (G)', 1),
          th('stotal', 'Total (T)', 1), th('sfree', 'Free (T)', 1), th('zfs', 'Pool'), th('vms', 'VMs', 1), th('watts', 'Power (W)', 1), th('age', 'Age', 1))),
      h('tbody', {}, rows.length ? rows : h('tr', {}, h('td', { colspan: 14, class: 'muted' }, data.hosts.length ? 'No host matches.' : 'No hosts yet.' + (isAdmin() ? ' Add them in Settings → Hosts.' : ''))))));
    mount('hosts', h('div', {}, h('div', { class: 'page-head' }, h('div', { class: 'grow' }, h('h1', {}, 'Hypervisors'), h('div', { class: 'muted' }, `${data.hosts.length} hosts` + (bad ? ` · ${bad} without current data` : ''))), h('div', { style: 'display:grid;gap:6px;justify-items:end' }, search, h('div', { class: 'small muted', style: 'display:flex;gap:8px;align-items:center' }, 'CPU idle:', cpuModeControl(draw)))), table,
      h('p', { class: 'note' }, 'Click a host to see which VMs load it. "Free" is what the host can still give out (memory: available; storage: usable space of the ZFS pools, not the raw size with parity; or the free space of the LVM volume groups).')));
  }
  const load = async () => { data = await api('GET', '/api/hosts'); draw(); };
  await load();
  startPolling(() => load().catch(() => {}));
}

// ---------------------------------------------------------------- one host
async function viewHost(id) {
  let data, top = null, topError = null, topLoading = false;
  let windowName = store('host.window') || '6h';
  let showAll = false;
  let idle = null, idleError = null, idleRange = store('host.idleRange') === '1y' ? '1y' : '14d';
  const idleRanges = { '14d': '14 days', '1y': '1 year' };
  const windows = { '1h': 'Last hour', '6h': '6 hours', '24h': '24 hours', '7d': '7 days' };
  function draw() {
    const x = data.host;
    const st = poolTotals(x.pools);
    const head = h('div', { class: 'page-head' }, h('div', { class: 'grow' }, h('div', { class: 'small' }, h('a', { href: '#/hosts' }, '← Hypervisors')),
      h('div', { style: 'display:flex;gap:12px;align-items:center;flex-wrap:wrap' }, hostDot(x), h('h1', {}, x.name),
        !x.enabled ? h('span', { class: 'pill unknown' }, 'disabled') : (!x.trusted ? h('span', { class: 'pill warning' }, 'key not confirmed') : (x.online ? h('span', { class: 'pill ok' }, 'online') : h('span', { class: 'pill over' }, x.stale ? 'data is old' : 'no answer'))),
        st && st.bad ? h('span', { class: 'pill over' }, 'ZFS ' + st.bad) : null,
        x.space && x.space !== 'ok' && !(st && st.bad) ? h('span', { class: 'pill ' + x.space }, 'Disk space: ' + (x.spaceWhy || 'low')) : null),
      h('div', { class: 'muted small' }, (x.collector ? `via ${x.collector}` : `${x.address}:${x.port}`) + (x.ageSeconds != null ? ` · data ${ago(x.ageSeconds)} old` : '') + (x.agent ? ` · agent ${x.agent}` : '')),
      x.rackName ? h('div', { class: 'small' }, 'Rack ', h('a', { href: '#/rack/' + x.rackId }, x.rackName), x.outletKey ? ' · outlet ' + x.outletKey : '') : null));
    const warn = x.error && x.error !== 'disabled' ? h('div', { class: 'notice' + (x.ageSeconds == null ? ' bad' : '') }, '⚠ ' + x.error + (x.ageSeconds != null ? ' — showing the last data' : '')) : null;
    const problems = x.problems && x.problems.length ? h('div', { class: 'notice' }, 'The agent reports: ' + x.problems.join('; ')) : null;

    const memUsed = x.memUsed, memTotal = x.memTotal;
    const vmMem = (x.vmList || []).reduce((a, v) => a + (v.rss || 0), 0);
    const arc = x.memArc || 0;
    const other = memUsed != null ? Math.max(0, memUsed - vmMem - arc) : 0;
    const seg = (cls, v, color) => { const i = h('i', { class: cls }); i.style.width = (memTotal ? (v / memTotal) * 100 : 0) + '%'; i.style.background = color; return i; };
    const memCard = h('div', { class: 'card' }, h('h3', {}, 'Memory'), memTotal == null ? h('p', { class: 'muted' }, 'No data') : [
      h('div', { class: 'stat', style: 'margin:8px 0' }, h('div', { class: 'value' }, gib(memUsed), h('small', {}, ` G used of ${gib(memTotal)} G`))),
      h('div', { class: 'bar stack' }, seg('', vmMem, 'var(--accent)'), seg('', arc, '#9b59b6'), seg('', other, 'var(--unk)')),
      h('div', { class: 'note' }, h('span', { style: 'color:var(--accent)' }, '■ '), `VMs ${gib(vmMem)} G   `, x.memArc != null ? [h('span', { style: 'color:#9b59b6' }, '■ '), `ZFS cache ${gib(arc)} G   `] : null, h('span', { style: 'color:var(--unk)' }, '■ '), `other ${gib(other)} G   · available ${gib(memTotal - memUsed)} G`)]);
    const cpuCard = h('div', { class: 'card' }, h('h3', {}, 'Processor'), x.idlePct == null ? h('p', { class: 'muted' }, 'No data') : [
      h('div', { class: 'stat', style: 'margin:8px 0' }, h('div', { class: 'value ' + idleClass(x.idlePct) }, Math.round(idleShown(x.idlePct)), h('small', {}, ' % idle'), cpuMode() === 'threads' ? h('small', { class: 'muted', style: 'margin-left:8px' }, `(reported ${Math.round(x.idlePct)} %)`) : null)),
      bar((100 - idleShown(x.idlePct)) / 100, idleLevel(x.idlePct)),
      h('dl', { class: 'kv', style: 'margin-top:10px' }, h('dt', {}, 'Cores'), h('dd', {}, x.cores == null ? '—' : x.cores), h('dt', {}, 'Load 1 / 5 / 15 min'), h('dd', {}, x.load ? x.load.map((v) => Math.round(v * 10) / 10).join(' / ') : '—'),
        h('dt', {}, 'VMs running'), h('dd', {}, x.vms == null ? '—' : x.vms), x.otherCores != null ? [h('dt', {}, 'Host itself'), h('dd', {}, nf(x.otherCores, 2) + ' cores')] : null)]);
    const zfsCards = (x.pools || []).map((p) => {
      const total = p.usable_total != null ? p.usable_total : p.size, avail = p.usable_avail != null ? p.usable_avail : p.free;
      const f = total ? avail / total : null;
      return h('div', { style: 'margin-top:8px' }, h('div', { style: 'display:flex;gap:10px;align-items:center' }, h('strong', {}, p.name), h('span', { class: 'pill ' + (p.health === 'ONLINE' ? 'ok' : 'over') }, p.health), h('span', { class: 'muted small' }, 'ZFS')),
        h('div', { class: 'stat', style: 'margin:6px 0' }, h('div', { class: 'value ' + freeClass(f) }, tib(avail), h('small', {}, ` T free of ${tib(total)} T usable (${Math.round(f * 100)}%)`))),
        bar(total ? 1 - avail / total : null, f != null && f < 0.1 ? 'over' : (f != null && f < 0.2 ? 'warning' : 'ok'), true),
        h('div', { class: 'note' }, `Raw: ${tib(p.size)} T, ${p.cap_pct}% allocated (includes the parity of raidz).`));
    });
    const lvmCards = (x.lvm || []).map((g) => {
      const f = g.size ? g.free / g.size : null;
      return h('div', { style: 'margin-top:8px' }, h('div', { style: 'display:flex;gap:10px;align-items:center' }, h('strong', {}, g.name), h('span', { class: 'muted small' }, 'LVM volume group')),
        h('div', { class: 'stat', style: 'margin:6px 0' }, h('div', { class: 'value ' + freeClass(f) }, tib(g.free), h('small', {}, ` T free of ${tib(g.size)} T (${Math.round(f * 100)}%)`))),
        bar(g.size ? 1 - g.free / g.size : null, f != null && f < 0.1 ? 'over' : (f != null && f < 0.2 ? 'warning' : 'ok'), true),
        h('div', { class: 'note' }, `${g.lvs} logical volume${g.lvs === 1 ? '' : 's'} on ${g.pvs} physical volume${g.pvs === 1 ? '' : 's'}.`),
        (g.fill || []).map((v) => {
          const p = fillPct(v);
          return h('div', { style: 'margin-top:6px' }, h('div', { class: 'small', style: 'display:flex;justify-content:space-between' }, h('span', {}, `${v.kind === 'thin-pool' ? 'Thin pool' : 'Snapshot'} ${v.name}`),
            h('span', { class: 'c-' + (p >= 90 ? 'over' : (p >= 80 ? 'warning' : 'ok')) }, `${nf(v.data_pct, 0)}% data` + (v.meta_pct != null ? ` · ${nf(v.meta_pct, 0)}% metadata` : ''))), bar(p / 100, fillClass(p), true));
        }));
    });
    const storCard = h('div', { class: 'card' }, h('h3', {}, 'Storage'), !zfsCards.length && !lvmCards.length ? h('p', { class: 'muted' }, 'No ZFS pool or LVM volume group reported') : [zfsCards, lvmCards]);
    const fsCard = x.fs && x.fs.length ? h('div', { class: 'card', style: 'margin-top:14px' }, h('h2', { style: 'margin-bottom:8px' }, 'Filesystems'),
      h('div', { class: 'tablewrap', style: 'box-shadow:none' }, h('table', {}, h('thead', {}, h('tr', {}, h('th', {}, 'Mounted on'), h('th', {}, 'Device'), h('th', {}, 'Type'), h('th', { class: 'num' }, 'Size (G)'), h('th', { class: 'num' }, 'Used (G)'), h('th', { class: 'num' }, 'Free (G)'), h('th', { style: 'width:160px' }, 'Use'))),
        h('tbody', {}, x.fs.map((f) => {
          const usedFrac = f.size ? f.used / f.size : null, freeFrac = f.size ? f.avail / f.size : null;
          return h('tr', {}, h('td', { class: 'name' }, f.mount), h('td', { class: 'muted' }, f.dev), h('td', { class: 'muted' }, f.type), h('td', { class: 'num' }, gib(f.size)), h('td', { class: 'num' }, gib(f.used)),
            h('td', { class: 'num ' + freeClass(freeFrac) }, gib(f.avail)), h('td', {}, h('div', { style: 'display:flex;gap:8px;align-items:center' }, h('div', { style: 'flex:1' }, bar(usedFrac, freeFrac != null && freeFrac < 0.1 ? 'over' : (freeFrac != null && freeFrac < 0.2 ? 'warning' : 'ok'), true)), h('span', { class: 'small muted' }, pct(usedFrac)))));
        }))))) : null;
    const powerCard = x.power ? h('div', { class: 'card' }, h('h3', {}, 'Power (from the PDUs)'), h('div', { class: 'stat', style: 'margin:8px 0' }, h('div', { class: 'value' }, fW(x.power.watts), h('small', {}, ' W'))),
      h('div', { class: 'note' }, `Outlet ${x.power.server} · ${x.power.ports} port${x.power.ports === 1 ? '' : 's'} · ${x.power.state}  `, h('a', { href: '#/rack/' + x.power.rackId }, 'Open the rack'))) : null;

    // the busiest VMs
    const tabs = h('div', { class: 'seg' }, Object.keys(windows).map((w) => h('button', { class: w === windowName ? 'on' : '', onclick: () => { windowName = w; store('host.window', w); top = null; topError = null; draw(); loadTop(); } }, windows[w])));
    let topBody;
    if (topError) topBody = h('p', { class: 'error' }, topError);
    else if (!top) topBody = h('p', { class: 'muted' }, topLoading ? 'Asking the host…' : 'No data yet.');
    else if (!top.vms.length) topBody = h('p', { class: 'muted' }, 'No VM used CPU in this period (or the agent has no samples yet).');
    else {
      const rowsData = showAll ? top.vms : top.vms.slice(0, 5);
      const scale = Math.max(...rowsData.map((v) => v.peak_cores), 0.01);
      topBody = [h('div', { class: 'topbars' }, rowsData.map((v, i) => {
        const m = h('div', { class: 'meter', title: `average ${nf(v.avg_cores, 2)} cores, busiest minute ${nf(v.peak_cores, 2)} cores` }, h('i'), h('b'));
        m.firstChild.style.width = Math.min(100, (v.avg_cores / scale) * 100) + '%';
        m.lastChild.style.left = Math.min(99.5, (v.peak_cores / scale) * 100) + '%';
        return h('div', { class: 'rowx' }, h('span', { class: 'rank' }, i + 1), h('span', { class: 'nm', title: v.name }, v.name), m,
          h('span', { class: 'val' }, `${nf(v.avg_cores, 2)} cores · ${nf(v.avg_pct, 1)}%`, h('div', { class: 'muted small' }, `peak ${nf(v.peak_cores, 1)} · ${hoursOf(v.cpu_s)}`)));
      })),
      top.vms.length > 5 ? h('button', { class: 'link', style: 'margin-top:8px', onclick: () => { showAll = !showAll; draw(); } }, showAll ? 'Show the top 5 only' : `Show all ${top.vms.length}`) : null,
      h('p', { class: 'note' }, 'Average load in the period (bar) and the busiest single minute (orange mark); "%" is the share of the whole host. ' +
        `Other CPU use of the host itself: ${hoursOf(top.other_cpu_s)}.` + (top.coverage < 0.95 ? ` Only ${Math.round(top.coverage * 100)}% of the period has data (${ago(Math.round(top.covered_s))} collected).` : ''))];
    }
    const topCard = h('div', { class: 'card', style: 'margin-top:14px' }, h('div', { style: 'display:flex;align-items:center;gap:14px;flex-wrap:wrap;margin-bottom:8px' }, h('h2', {}, 'Busiest VMs'), tabs), topBody);

    // CPU idle history: one vertical bar per day (14 days) or per week (a year); the server keeps hourly averages
    const idleTabs = h('div', { class: 'seg' }, Object.keys(idleRanges).map((r) => h('button', { class: r === idleRange ? 'on' : '', onclick: () => { idleRange = r; store('host.idleRange', r); idle = null; idleError = null; draw(); loadIdle(); } }, idleRanges[r])));
    let idleBody;
    if (idleError) idleBody = h('p', { class: 'error' }, idleError);
    else if (!idle) idleBody = h('p', { class: 'muted' }, 'Loading…');
    else if (!idle.buckets.some((b) => b.n > 0)) idleBody = h('p', { class: 'muted' }, 'No history yet. The server starts to keep it from the first reading it gets from this host.');
    else {
      const week = idle.unit === 'week';
      const when = (b) => new Date(b.t * 1000).toLocaleDateString(undefined, { weekday: week ? undefined : 'short', day: 'numeric', month: 'short', year: week ? 'numeric' : undefined });
      // newest on the left: what a person looks for first is today
      const shown = idle.buckets.slice().reverse();
      const cols = shown.map((b, i) => {
        const col = h('div', { class: 'col' + (b.avg == null ? ' empty' : ''), title: b.avg == null ? `${week ? 'Week of ' : ''}${when(b)}: no readings` : `${week ? 'Week of ' : ''}${when(b)}\naverage idle ${nf(idleShown(b.avg), 1)}%${cpuMode() === 'threads' ? ` (reported ${nf(b.avg, 1)}%)` : ''}\nlowest hour ${nf(idleShown(b.min), 1)}%${cpuMode() === 'threads' ? ` (reported ${nf(b.min, 1)}%)` : ''}\n${b.n} readings` },
          h('span', { class: 'v' }, b.avg == null ? '' : Math.round(idleShown(b.avg))));
        const fill = h('i', { class: b.avg == null ? '' : idleLevel(b.avg) });
        fill.style.height = b.avg == null ? '0%' : Math.max(2, idleShown(b.avg)) + '%';
        col.append(fill);
        const d = new Date(b.t * 1000);
        // a label under every bar for days; for weeks only at the left edge of a month (the bar to its left is newer), otherwise 53 labels would collide
        const left = i ? new Date(shown[i - 1].t * 1000) : null;
        const leftLabelled = i > 1 && new Date(shown[i - 2].t * 1000).getMonth() !== new Date(shown[i - 1].t * 1000).getMonth();
        const label = week ? (!left || (left.getMonth() !== d.getMonth() && !leftLabelled) ? d.toLocaleDateString(undefined, { month: 'short' }) : '') : String(d.getDate());
        col.append(h('em', {}, label));
        return col;
      });
      idleBody = [h('div', { class: 'idlebars' + (week ? ' weeks' : '') }, h('div', { class: 'ticks' }, h('span', {}, '100'), h('span', {}, '50'), h('span', {}, '0')), h('div', { class: 'cols' }, cols)),
        h('p', { class: 'note' }, (week ? 'Newest on the left. Average CPU idle per week (weeks begin on Monday), ' : 'Newest on the left. Average CPU idle per day, ') + idleLimits() + '. Hover a bar for the lowest hour' + (cpuMode() === 'threads' ? ' and for what the host reported' : '') + '. ' +
          (idle.firstData && idle.buckets[0].t < idle.firstData - 86400 ? 'Older bars (right) are empty because the server keeps this history only since ' + new Date(idle.firstData * 1000).toLocaleDateString() + '.' : ''))];
    }
    const idleCard = h('div', { class: 'card', style: 'margin-top:14px' }, h('div', { style: 'display:flex;align-items:center;gap:14px;flex-wrap:wrap;margin-bottom:8px' }, h('h2', {}, 'CPU idle %'), idleTabs, h('div', { class: 'spacer' }), cpuModeControl(draw)), idleBody);

    const vmCard = x.vmList && x.vmList.length ? h('div', { class: 'card', style: 'margin-top:14px' }, h('h2', { style: 'margin-bottom:8px' }, 'VMs running now'),
      h('div', { class: 'tablewrap', style: 'box-shadow:none' }, h('table', {}, h('thead', {}, h('tr', {}, h('th', {}, 'VM'), h('th', { class: 'num' }, 'CPU now (cores)'), h('th', { class: 'num' }, 'Memory (G)'))),
        h('tbody', {}, x.vmList.slice().sort((a, b) => (b.cores || 0) - (a.cores || 0)).map((v) => h('tr', {}, h('td', { class: 'name' }, v.name), h('td', { class: 'num' }, nf(v.cores, 2)), h('td', { class: 'num' }, (v.rss / 2 ** 30).toFixed(1)))))))) : null;
    mount('hosts', h('div', {}, head, warn, problems, h('div', { class: 'cards pdus', style: 'grid-template-columns:repeat(auto-fit,minmax(280px,1fr))' }, cpuCard, memCard, storCard, powerCard), fsCard, topCard, idleCard, vmCard));
  }
  const hoursOf = (s) => (s >= 3600 ? (s / 3600).toFixed(1) + ' CPU-h' : Math.round(s / 60) + ' CPU-min');
  async function loadTop() {
    if (!data.host.trusted || !data.host.enabled) return;
    topLoading = true;
    try { top = await api('GET', `/api/hosts/${id}/top?window=${windowName}`); topError = null; } catch (e) { if (!(e instanceof ApiError && e.status === 401)) { topError = e.message; top = null; } } finally { topLoading = false; }
    if (!dialogOpen()) draw();
  }
  async function loadIdle() {
    const asked = idleRange;
    try {
      const r = await api('GET', `/api/hosts/${id}/idle?range=${asked}&tz=${new Date().getTimezoneOffset()}`);
      if (asked !== idleRange) return; // the person switched meanwhile
      idle = r; idleError = null;
    } catch (e) { if (!(e instanceof ApiError && e.status === 401)) { idleError = e.message; idle = null; } }
    if (!dialogOpen()) draw();
  }
  const load = async () => { data = await api('GET', '/api/hosts/' + id); draw(); };
  await load();
  loadTop();
  loadIdle();
  startPolling(() => load().then(loadTop).then(loadIdle).catch(() => {}));
}

// ---------------------------------------------------------------- your login: the second step
function qrImage(rows) {
  const n = rows.length, q = 4; // four modules of white around it, as the standard asks
  let d = '';
  rows.forEach((row, y) => { for (let x = 0; x < n; x++) if (row[x] === '1') d += `M${x + q} ${y + q}h1v1h-1z`; });
  return svg('svg', { viewBox: `0 0 ${n + 2 * q} ${n + 2 * q}`, width: 220, height: 220, role: 'img', 'aria-label': 'QR code to scan with the authenticator app', shapeRendering: 'crispEdges', style: 'background:#fff;border-radius:8px' },
    svg('path', { d, fill: '#000' }));
}
function showRecoveryCodes(codes, intro) {
  const text = codes.join('\n');
  const copied = h('span', { class: 'c-ok small' });
  return dialog('Recovery codes', h('div', {}, h('p', {}, intro || 'Keep these somewhere safe (a password manager, a printed page). Each one works once and is the way in when a phone is lost.'),
    h('pre', { class: 'key', style: 'font-size:15px;line-height:1.7;letter-spacing:1px' }, text),
    h('div', {}, h('button', { class: 'small', onclick: async () => { try { await navigator.clipboard.writeText(text); copied.textContent = ' Copied.'; } catch (e) { copied.className = 'error small'; copied.textContent = ' Select the codes and copy them by hand.'; } } }, 'Copy'), copied),
    h('p', { class: 'note' }, 'They are shown only now: the server keeps no readable copy.')), [{ label: 'I have saved them', value: true, class: 'primary' }]);
}

async function viewSecurity() {
  let st = await api('GET', '/api/me/mfa');
  const reload = async () => { st = await api('GET', '/api/me/mfa'); S.session.limited = st.limited; draw(); };
  const since = (iso) => datetime(iso);
  async function remove(f) {
    const ok = await confirmDialog(`Remove “${f.name}”?`, h('p', {}, f.kind === 'totp' ? 'The code of this authenticator app will no longer log anybody in.' : 'This passkey will no longer log anybody in.'), 'Remove', true);
    if (!ok) return;
    try { st = await api('DELETE', '/api/me/mfa/' + f.id); draw(); } catch (e) { setBanner({ title: 'Not removed', error: e.message, running: false }); }
  }
  async function afterAdd(r) {
    st = r;
    S.session.limited = r.limited; // a limited session became a full one
    if (r.recoveryCodes && r.recoveryCodes.length) await showRecoveryCodes(r.recoveryCodes);
    if (!S.session.limited && S.session.limitedBefore) { S.session.limitedBefore = false; location.hash = '#/'; S.shell = null; route(); return; }
    draw();
  }
  async function addApp() {
    const name = h('input', { value: '', placeholder: 'for example: phone', maxlength: 40, required: true });
    const err = h('div', { class: 'error', role: 'alert' });
    const ask = await dialog('Add an authenticator app', h('div', {}, h('p', {}, 'Any app that makes 6-digit codes works: Google Authenticator, Microsoft Authenticator, Authy, 1Password, Aegis… Give this device a name so that you know which one it is.'), field('Name', name)),
      [{ label: 'Cancel', value: false }, { label: 'Next', value: true, class: 'primary' }]);
    if (!ask) return;
    let begin;
    try { begin = await api('POST', '/api/me/mfa/totp/begin', { name: name.value.trim() }); } catch (e) { setBanner({ title: 'Could not start', error: e.message, running: false }); return; }
    const code = h('input', { inputmode: 'numeric', autocomplete: 'one-time-code', maxlength: 8, placeholder: '6 digits', required: true });
    const body = h('div', { style: 'display:grid;gap:10px;justify-items:start' }, h('p', { style: 'margin:0' }, 'Scan this with the app, or type the key by hand. Then type the code that the app shows, to prove that it works.'),
      qrImage(begin.qr), h('div', { class: 'small muted' }, 'Key by hand:'), h('code', { class: 'mono', style: 'word-break:break-all;user-select:all' }, begin.secret), field('Code from the app', code));
    let result = null;
    const ok = await formDialog('Add an authenticator app', body, 'Add it', async () => { result = await api('POST', '/api/me/mfa/totp/confirm', { code: code.value.trim() }); });
    if (ok && result) await afterAdd(result);
  }
  async function addPasskey() {
    if (!passkeysInBrowser()) { setBanner({ title: 'No passkeys here', error: 'This browser cannot make a passkey on this address (it needs https and the name of the server).', running: false }); return; }
    const name = h('input', { value: '', placeholder: 'for example: laptop fingerprint', maxlength: 40, required: true });
    const ask = await dialog('Add a passkey', h('div', {}, h('p', {}, 'A passkey is a fingerprint (Touch ID, Windows Hello), a phone or a security key. After you press Next the browser asks you to use it. It is asked after the password at every login.'), field('Name', name)),
      [{ label: 'Cancel', value: false }, { label: 'Next', value: true, class: 'primary' }]);
    if (!ask) return;
    try {
      const options = await api('POST', '/api/me/mfa/passkey/begin', { name: name.value.trim() });
      const answer = await passkeyCreate(options);
      await afterAdd(await api('POST', '/api/me/mfa/passkey/finish', answer));
    } catch (e) { setBanner({ title: 'Passkey not added', error: e instanceof ApiError ? e.message : passkeyError(e), running: false }); }
  }
  async function newCodes() {
    const ok = await confirmDialog('Make new recovery codes?', h('p', {}, 'The codes you have now stop working.'), 'Make new codes', false);
    if (!ok) return;
    try { const r = await api('POST', '/api/me/mfa/recovery', {}); st = r; await showRecoveryCodes(r.recoveryCodes); draw(); } catch (e) { setBanner({ title: 'Not made', error: e.message, running: false }); }
  }
  function draw() {
    const apps = st.factors.filter((f) => f.kind === 'totp'), keys = st.factors.filter((f) => f.kind === 'passkey');
    const row = (f) => h('tr', {}, h('td', { class: 'name' }, f.name), h('td', { class: 'muted' }, 'added ' + since(f.created)), h('td', { class: 'muted' }, f.lastUsed ? 'used ' + since(f.lastUsed) : 'not used yet'), h('td', { class: 'num' }, h('button', { class: 'small', onclick: () => remove(f) }, 'Remove')));
    const table = (list) => list.length ? h('div', { class: 'tablewrap', style: 'box-shadow:none;margin-bottom:10px' }, h('table', {}, h('tbody', {}, list.map(row)))) : h('p', { class: 'muted small' }, 'None yet.');
    const must = st.limited ? h('div', { class: 'notice bad', role: 'alert' }, 'The administrator requires a second step for the login. Add an authenticator app or a passkey to continue.')
      : (st.require && !st.factors.length ? h('div', { class: 'notice' }, 'The administrator requires a second step for the login.') : null);
    mount('security', h('div', { style: 'max-width:760px' }, h('h1', {}, 'Your login'), must,
      h('p', { class: 'muted' }, st.factors.length ? 'Second step: on. After the password you are asked for a code or a passkey.' : 'Second step: off. Anybody who knows your password can log in.'),
      h('div', { class: 'card', style: 'margin-bottom:14px' }, h('div', { style: 'display:flex;gap:12px;align-items:center;flex-wrap:wrap' }, h('h2', {}, 'Authenticator apps'), h('div', { class: 'spacer' }),
        h('button', { class: 'primary', disabled: apps.length >= st.max, onclick: addApp }, 'Add an authenticator app')),
        table(apps), h('p', { class: 'note' }, `Up to ${st.max} devices, each with its own key, so that a lost phone can be removed alone.`)),
      h('div', { class: 'card', style: 'margin-bottom:14px' }, h('div', { style: 'display:flex;gap:12px;align-items:center;flex-wrap:wrap' }, h('h2', {}, 'Passkeys (fingerprint, phone, security key)'), h('div', { class: 'spacer' }),
        h('button', { class: 'primary', disabled: keys.length >= st.max || !st.passkeys, onclick: addPasskey }, 'Add a passkey')),
        table(keys),
        !st.passkeys ? h('p', { class: 'note' }, 'Passkeys are not set up on this server: an administrator adds public_url: https://the-name-of-this-server to config.yaml and restarts it.') : h('p', { class: 'note' }, `Up to ${st.max}. Your laptop's fingerprint reader counts: choose “this device” in the browser's question.`)),
      h('div', { class: 'card', style: 'margin-bottom:14px' }, h('h2', {}, 'How you see CPU idle'),
        h('p', { class: 'muted' }, 'The hosts have hyper-threads, which add much less than a real core: a host that reports 50 % idle has no room left. “Threads discounted” shows 50 % reported as 0 % and 100 % as 100 % (90 % reported is 80 %). It is your choice only and applies to every host: the hosts list, the host page and the history bars.'),
        cpuModeControl(draw), h('p', { class: 'note' }, 'Colours: ' + idleLimits() + ' (the same state in both ways).')),
      h('div', { class: 'card' }, h('div', { style: 'display:flex;gap:12px;align-items:center;flex-wrap:wrap' }, h('h2', {}, 'Recovery codes'), h('div', { class: 'spacer' }),
        h('button', { disabled: !st.factors.length, onclick: newCodes }, 'Make new codes')),
        h('p', { class: st.factors.length && st.recovery < 3 ? 'c-warning' : 'muted' }, st.factors.length ? `${st.recovery} unused code${st.recovery === 1 ? '' : 's'}. They are the way in when a phone is lost.` : 'Made when you add the first device.'))));
  }
  S.session.limitedBefore = !!st.limited;
  draw();
}

// ---------------------------------------------------------------- settings (administrators)
// A form in a dialog that stays open (and shows the reason) when saving fails.
function formDialog(title, body, saveLabel, onSave, extraButtons) {
  const err = h('div', { class: 'error', role: 'alert', style: 'margin-top:8px' });
  const save = h('button', { class: 'primary', type: 'button' }, saveLabel);
  const cancel = h('button', { type: 'button' }, 'Cancel');
  const dlg = h('dialog', { class: 'wide' }, h('h2', {}, title), body, err, h('div', { class: 'buttons' }, ...(extraButtons || []), h('div', { class: 'spacer' }), cancel, save));
  return new Promise((resolve) => {
    cancel.addEventListener('click', () => { dlg.close(); resolve(false); });
    dlg.addEventListener('cancel', () => resolve(false));
    dlg.addEventListener('close', () => dlg.remove());
    save.addEventListener('click', async () => {
      save.disabled = true; err.textContent = '';
      try { await onSave(); dlg.close(); resolve(true); } catch (e) { err.textContent = e.message; } finally { save.disabled = false; }
    });
    document.body.append(dlg);
    dlg.showModal();
  });
}
function field(label, input, hint) { return h('label', { class: 'field' }, h('span', {}, label), input, hint ? h('span', { class: 'hint' }, hint) : null); }
const num = (v) => { const n = parseFloat(String(v).replace(',', '.')); return Number.isFinite(n) ? n : 0; };

async function viewAdmin(tab) {
  if (!isAdmin()) { mount('', h('div', { class: 'card' }, 'Only administrators can change the settings.')); return; }
  const tabs = [['racks', 'Racks'], ['pdus', 'PDUs'], ['hosts', 'Hosts'], ['names', 'Names'], ['settings', 'Settings'], ['users', 'Users'], ['software', 'Software'], ['import', 'Import'], ['audit', 'Log']];
  if (!tabs.some((t) => t[0] === tab)) tab = 'racks';
  const nav = h('div', { class: 'tabs' }, tabs.map(([k, v]) => h('a', { href: '#/admin/' + k, class: k === tab ? 'on' : '' }, v)));
  const body = h('div');
  mount('admin', h('div', {}, h('h1', {}, 'Settings'), nav, body));
  const reload = () => viewAdmin(tab).catch((e) => failed(e));
  if (tab === 'audit') return adminAudit(body);
  if (tab === 'names') return adminNames(body);
  if (tab === 'software') return adminSoftware(body);
  if (tab === 'users') return adminUsers(body);
  if (tab === 'import') return adminImport(body, reload);
  const cfg = await api('GET', '/api/admin/config');
  if (tab === 'racks') adminRacks(body, cfg, reload);
  else if (tab === 'pdus') adminPDUs(body, cfg, reload);
  else if (tab === 'hosts') adminHosts(body, cfg, reload);
  else adminSettings(body, cfg, reload);
}

function adminRacks(body, cfg, reload) {
  const edit = (rack) => {
    const name = h('input', { value: rack ? rack.name : '', placeholder: 'e.g. 200', required: true });
    const maxAmps = h('input', { value: rack ? String(rack.maxAmps) : '24', inputmode: 'decimal', size: 6 });
    formDialog(rack ? 'Edit rack ' + rack.name : 'New rack', h('div', {}, field('Name', name), field('Limit (A)', maxAmps, 'The most the whole rack may draw; the overview warns when it is near.')), 'Save',
      () => api(rack ? 'PUT' : 'POST', rack ? '/api/admin/racks/' + rack.id : '/api/admin/racks', { name: name.value.trim(), maxAmps: num(maxAmps.value), sort: rack ? rack.sort : 0 }).then(reload));
  };
  const remove = async (rack) => {
    const used = cfg.pdus.filter((p) => p.rackId === rack.id).length;
    const ok = await confirmDialog('Delete rack ' + rack.name + '?', h('p', {}, used ? `Its ${used} PDU${used === 1 ? '' : 's'} and their names are deleted too.` : 'The rack is empty.'), 'Delete', true);
    if (ok) api('DELETE', '/api/admin/racks/' + rack.id).then(reload).catch((e) => setBanner({ title: 'Not deleted', error: e.message, running: false }));
  };
  const rows = cfg.racks.map((r) => h('tr', {}, h('td', { class: 'name' }, r.name), h('td', { class: 'num' }, lim(r.maxAmps) + ' A'),
    h('td', { class: 'num' }, cfg.pdus.filter((p) => p.rackId === r.id).length), h('td', { class: 'num' }, h('button', { class: 'small', onclick: () => edit(r) }, 'Edit'), ' ', h('button', { class: 'small danger', onclick: () => remove(r) }, 'Delete'))));
  body.append(h('div', { class: 'actionbar' }, h('button', { class: 'primary', onclick: () => edit(null) }, 'Add rack')),
    cfg.racks.length ? h('div', { class: 'tablewrap' }, h('table', {}, h('thead', {}, h('tr', {}, h('th', {}, 'Rack'), h('th', { class: 'num' }, 'Limit'), h('th', { class: 'num' }, 'PDUs'), h('th'))), h('tbody', {}, rows)))
      : h('p', { class: 'muted' }, 'There are no racks yet: add the first one.'));
}

function adminPDUs(body, cfg, reload) {
  const edit = (pdu) => {
    const rack = h('select', {}, cfg.racks.map((r) => h('option', { value: r.id }, r.name)));
    rack.value = pdu ? pdu.rackId : (cfg.racks[0] && cfg.racks[0].id);
    const name = h('input', { value: pdu ? pdu.name : '', placeholder: 'e.g. A or Left', required: true });
    const vendor = h('select', {}, h('option', { value: 'cyberpower' }, 'CyberPower'), h('option', { value: 'apc' }, 'APC'));
    vendor.value = pdu ? pdu.vendor : 'cyberpower';
    const host = h('input', { value: pdu ? pdu.host : '', placeholder: '10.0.0.21', required: true });
    const port = h('input', { value: pdu ? String(pdu.port) : '161', inputmode: 'numeric', size: 6 });
    const maxAmps = h('input', { value: pdu ? String(pdu.maxAmps) : '24', inputmode: 'decimal', size: 6 });
    const bank = h('input', { value: pdu && pdu.bankMaxAmps != null ? String(pdu.bankMaxAmps) : '', inputmode: 'decimal', size: 6, placeholder: 'none' });
    const read = h('input', { type: 'password', autocomplete: 'off', placeholder: pdu && pdu.hasReadCommunity ? '(kept as it is)' : 'public' });
    const write = h('input', { type: 'password', autocomplete: 'off', placeholder: pdu && pdu.hasWriteCommunity ? '(kept as it is)' : 'empty = read only' });
    const clearWrite = h('input', { type: 'checkbox' });
    const result = h('div', { class: 'small', style: 'margin-top:8px;min-height:18px' });
    const payload = () => ({ rackId: +rack.value, name: name.value.trim(), vendor: vendor.value, host: host.value.trim(), port: parseInt(port.value, 10) || 0, maxAmps: num(maxAmps.value),
      bankMaxAmps: bank.value.trim() === '' ? null : num(bank.value), readCommunity: read.value, writeCommunity: write.value, clearWrite: clearWrite.checked });
    const test = h('button', { type: 'button', onclick: async (ev) => {
      ev.target.disabled = true; result.className = 'small muted'; result.textContent = 'Asking the PDU…';
      try {
        const body = payload(); if (pdu) body.id = pdu.id;
        const r = await api('POST', '/api/admin/pdus/test', body);
        if (r.ok) { result.className = 'small c-ok'; result.textContent = `Works: ${r.model || 'PDU'}, ${r.outlets} outlets, ${fA(r.amps)} A now${r.metered ? ', per-outlet watts available' : ''}.`; }
        else { result.className = 'small error'; result.textContent = r.error; }
      } catch (e) { result.className = 'small error'; result.textContent = e.message; } finally { ev.target.disabled = false; }
    } }, 'Test connection');
    const form = h('div', {}, field('Rack', rack), field('Name', name), field('Make', vendor), field('Address', host), field('SNMP port', port),
      field('Limit (A)', maxAmps, 'Total of the PDU.'), field('Bank limit (A)', bank, 'Optional: the limit of one bank/phase.'),
      field('Read community', read), field('Write community', write, 'Needed to switch outlets. SNMP v1 sends it unencrypted: keep the PDUs on the management network.'),
      pdu && pdu.hasWriteCommunity ? field('', h('span', {}, clearWrite, ' Remove the write community (read only)')) : null, result);
    formDialog(pdu ? 'Edit PDU ' + pdu.name : 'New PDU', form, 'Save', () => api(pdu ? 'PUT' : 'POST', pdu ? '/api/admin/pdus/' + pdu.id : '/api/admin/pdus', payload()).then(reload), [test]);
  };
  const remove = async (p) => { if (await confirmDialog('Delete PDU ' + p.name + '?', h('p', {}, 'Its history is deleted too.'), 'Delete', true)) api('DELETE', '/api/admin/pdus/' + p.id).then(reload).catch((e) => setBanner({ title: 'Not deleted', error: e.message, running: false })); };
  const rackName = (id) => (cfg.racks.find((r) => r.id === id) || {}).name || '?';
  add(body, h('div', { class: 'actionbar' }, h('button', { class: 'primary', disabled: !cfg.racks.length, onclick: () => edit(null) }, 'Add PDU'), !cfg.racks.length ? h('span', { class: 'muted' }, 'Add a rack first.') : null),
    cfg.pdus.length ? h('div', { class: 'tablewrap' }, h('table', {}, h('thead', {}, h('tr', {}, ['Rack', 'PDU', 'Make', 'Address', 'Limit', 'Control'].map((t) => h('th', {}, t)), h('th'))),
      h('tbody', {}, cfg.pdus.map((p) => h('tr', {}, h('td', {}, rackName(p.rackId)), h('td', { class: 'name' }, p.name), h('td', {}, p.vendor === 'apc' ? 'APC' : 'CyberPower'), h('td', {}, `${p.host}:${p.port}`),
        h('td', { class: 'num' }, lim(p.maxAmps) + ' A'), h('td', {}, p.hasWriteCommunity ? 'on / off' : h('span', { class: 'muted' }, 'read only')),
        h('td', { class: 'num' }, h('button', { class: 'small', onclick: () => edit(p) }, 'Edit'), ' ', h('button', { class: 'small danger', onclick: () => remove(p) }, 'Delete'))))))) : h('p', { class: 'muted' }, 'There are no PDUs yet.'));
}

function adminHosts(body, cfg, reload) {
  const rackName = (id) => (cfg.racks.find((r) => r.id === id) || {}).name || '—';
  const edit = (host) => {
    const viaCollector = !!(host && host.collector);
    const name = h('input', { value: host ? host.name : '', placeholder: 'e.g. a54sf', required: true, readonly: viaCollector });
    const address = h('input', { value: host ? host.address : '', placeholder: 'host name or address', required: true });
    const port = h('input', { value: host ? String(host.port) : '22', inputmode: 'numeric', size: 6 });
    const user = h('input', { value: host ? host.user : 'hostinfo-ro', required: true });
    const rack = h('select', {}, h('option', { value: '' }, '—'), cfg.racks.map((r) => h('option', { value: r.id }, r.name)));
    rack.value = host && host.rackId != null ? host.rackId : '';
    const key = h('input', { value: host ? host.outletKey : '', placeholder: 'e.g. 200U31' });
    const enabled = h('input', { type: 'checkbox', checked: host ? host.enabled : true });
    const form = h('div', {},
      viaCollector ? h('p', { class: 'note', style: 'margin:0 0 8px' }, `Read by the collector ${host.collector}: its address and SSH key are in the configuration of the collector.`) : null,
      field('Name', name),
      viaCollector ? null : [field('Address', address), field('SSH port', port), field('SSH user', user, 'The read-only account made by the agent installer.')],
      field('Rack', rack), field('Power outlet name', key, 'The server name on the PDUs (e.g. 200U31): it joins the host to its power.'), field('Collect', h('span', {}, enabled, ' enabled')));
    formDialog(host ? 'Edit host ' + host.name : 'New host', form, 'Save', () => api(host ? 'PUT' : 'POST', host ? '/api/admin/hosts/' + host.id : '/api/admin/hosts',
      { name: name.value.trim(), address: address.value.trim(), port: parseInt(port.value, 10) || 22, user: user.value.trim(), rackId: rack.value === '' ? null : +rack.value, outletKey: key.value.trim(), enabled: enabled.checked, hostKey: host ? host.hostKey : '' }).then(reload));
  };
  // the host key must be compared by a person with the one on the host before it is trusted
  const trust = async (host) => {
    let info;
    try { info = await api('POST', `/api/admin/hosts/${host.id}/fetchkey`, {}); } catch (e) { setBanner({ title: 'Could not reach the host', error: e.message, running: false }); return; }
    const confirm = h('input', { size: 52, placeholder: 'paste the fingerprint shown on the host', autocomplete: 'off', spellcheck: 'false' });
    const body2 = h('div', {}, h('p', {}, host.hostKey ? 'The host offers this key (it replaces the trusted one if you confirm):' : `${host.name} offers this key:`),
      h('p', { class: 'mono' }, info.fingerprint), h('p', { class: 'muted small' }, `Compare it with the host: `, h('code', {}, info.howToCheck)),
      h('p', { class: 'small' }, 'Type or paste the fingerprint from the host to confirm that they are the same:'), confirm);
    await formDialog('Trust the key of ' + host.name + '?', body2, 'Trust this key', async () => {
      if (confirm.value.trim() !== info.fingerprint) throw new Error('The fingerprint does not match the one the server saw.');
      await api('POST', `/api/admin/hosts/${host.id}/trust`, { fingerprint: info.fingerprint });
    });
    reload();
  };
  const remove = async (x) => { if (await confirmDialog('Delete host ' + x.name + '?', h('p', {}, 'The data collected on the server is deleted; the agent on the host stays.'), 'Delete', true)) api('DELETE', '/api/admin/hosts/' + x.id).then(reload).catch((e) => setBanner({ title: 'Not deleted', error: e.message, running: false })); };
  const keyBox = h('div', { class: 'card', style: 'margin-bottom:14px' }, h('h2', {}, 'Server key'),
    h('p', { class: 'muted small' }, 'To let this server read a hypervisor itself, install the agent on it with this key (it lets this server read the data and nothing else). A collector has its own key: pdu-collector pubkey.'),
    h('pre', { class: 'key' }, `sudo ./install.sh --key '${cfg.serverKey}'`));
  const collectors = cfg.collectors || [];
  const collectorBox = collectors.length ? h('div', { class: 'card', style: 'margin-bottom:14px' }, h('h2', {}, 'Collectors'),
    h('p', { class: 'muted small' }, 'Passive readers that push the readings of their hypervisors here. A host they report is added by itself: link it to a rack and a power outlet name with Edit.'),
    h('div', { class: 'tablewrap', style: 'box-shadow:none' }, h('table', {}, h('thead', {}, h('tr', {}, ['Collector', 'Last report', 'Hosts', 'Version'].map((t) => h('th', {}, t)))),
      h('tbody', {}, collectors.map((c) => {
        const age = c.lastSeen ? Math.round((Date.now() - new Date(c.lastSeen).getTime()) / 1000) : null;
        const silent = age == null || age > 180;
        return h('tr', {}, h('td', { class: 'name' }, c.name),
          h('td', {}, age == null ? h('span', { class: 'c-over' }, 'never') : h('span', { class: silent ? 'c-over' : 'c-ok' }, ago(Math.max(age, 0)) + ' ago')),
          h('td', { class: 'num' }, c.lastSeen ? c.hosts : '—'), h('td', {}, c.version || '—'));
      }))))) : null;
  add(body, keyBox, collectorBox, h('div', { class: 'actionbar' }, h('button', { class: 'primary', onclick: () => edit(null) }, 'Add host')),
    cfg.hosts.length ? h('div', { class: 'tablewrap' }, h('table', {}, h('thead', {}, h('tr', {}, ['Host', 'Address', 'User', 'Rack', 'Outlet', 'Key'].map((t) => h('th', {}, t)), h('th'))),
      h('tbody', {}, cfg.hosts.map((x) => h('tr', { class: x.enabled ? '' : 'dim' }, h('td', { class: 'name' }, x.name), h('td', {}, x.collector ? '—' : `${x.address}:${x.port}`), h('td', {}, x.collector ? '' : x.user), h('td', {}, rackName(x.rackId)), h('td', {}, x.outletKey || '—'),
        h('td', {}, x.collector ? h('span', { class: 'muted' }, 'via ' + x.collector) : x.hostKey ? h('span', { class: 'c-ok' }, 'trusted') : h('button', { class: 'small primary', onclick: () => trust(x) }, 'Check the key'), !x.collector && x.hostKey ? h('button', { class: 'link small', onclick: () => trust(x) }, 'renew') : null),
        h('td', { class: 'num' }, h('button', { class: 'small', onclick: () => edit(x) }, 'Edit'), ' ', h('button', { class: 'small danger', onclick: () => remove(x) }, 'Delete'))))))) : h('p', { class: 'muted' }, 'No hosts yet.'));
}

function adminSettings(body, cfg, reload) {
  const s = cfg.settings;
  const inp = (v) => h('input', { value: String(v), inputmode: 'numeric', size: 6 });
  const poll = inp(s.pollSeconds), hostPoll = inp(s.hostPollSeconds), warn = inp(s.warnPercent), delay = inp(s.restartDelaySeconds), keep = inp(s.sampleRetentionDays);
  const smPoll = inp(s.servmanPollMinutes), smSync = inp(s.servmanSyncMinutes);
  const stay = inp(s.sessionHours == null ? 72 : s.sessionHours), requireMfa = h('input', { type: 'checkbox', checked: !!s.requireMfa });
  const smFormat = h('select', {}, h('option', { value: 'ccid' }, 'Rack unit id only: 200U13'), h('option', { value: 'ccid_name' }, 'Id and machine name: 200U13 a42sf'));
  smFormat.value = s.servmanNameFormat || 'ccid';
  const saved = h('span', { class: 'c-ok small' });
  const save = h('button', { class: 'primary', onclick: async () => {
    saved.textContent = '';
    try {
      const r = await api('PUT', '/api/admin/settings', { pollSeconds: +poll.value, hostPollSeconds: +hostPoll.value, warnPercent: +warn.value, restartDelaySeconds: +delay.value, sampleRetentionDays: +keep.value,
        servmanPollMinutes: +smPoll.value, servmanSyncMinutes: +smSync.value, servmanNameFormat: smFormat.value, sessionHours: +stay.value, requireMfa: requireMfa.checked });
      S.settings = { warnPercent: r.warnPercent, restartDelaySeconds: r.restartDelaySeconds, pollSeconds: r.pollSeconds, requireMfa: r.requireMfa };
      saved.textContent = 'Saved.';
    } catch (e) { saved.className = 'error small'; saved.textContent = e.message; }
  } }, 'Save');
  body.append(h('div', { class: 'card', style: 'max-width:640px' },
    field('PDU reading (s)', poll, 'How often the PDUs are asked.'), field('Host reading (s)', hostPoll, 'How often the agents of the hosts are read. An agent takes a new sample once a minute: reading more often only repeats the same data.'),
    field('Warn at (%)', warn, 'Of the limit; above it a rack or PDU is shown as high.'), field('Restart pause (s)', delay, 'Between switching off and on again (5–10 is usual).'),
    field('Keep history (days)', keep),
    h('h3', { style: 'margin:18px 0 4px' }, 'Login'),
    field('Stay logged in (hours)', stay, 'How long a login lasts, in hours (72 = three days). Applies at once, also to people who are logged in.'),
    field('Second step for everybody', requireMfa, 'Anybody without an authenticator app or a passkey must add one right after the password, before they can see anything. The emergency local account is not forced.'),
    h('h3', { style: 'margin:18px 0 4px' }, 'Names of the outlets (from servman)'),
    field('Ask servman every (min)', smPoll, 'Compares servman with the PDUs, shows the plan under Names and the conflicts on the overview. 0 = never by itself.'),
    field('Write to the PDUs every (min)', smSync, 'Writes the planned names to the PDUs by itself. 0 = only when you press the button under Names.'),
    field('Outlet name', smFormat, 'What an outlet is called. Both forms join the two PDUs of a server and the host by the rack unit id.'),
    h('div', { class: 'actionbar' }, save, saved)));
}



// Who has a second step; the administrator removes it for somebody who lost every device (and who has no recovery codes left).
async function adminUsers(body) {
  const r = await api('GET', '/api/admin/mfa');
  const users = r.users || [];
  async function reset(u) {
    const ok = await confirmDialog(`Remove the second step of ${u.user}?`, h('div', {}, h('p', {}, 'Their authenticator apps, passkeys and recovery codes are deleted and they are logged out everywhere. At the next login the password is enough' + (S.settings && S.settings.requireMfa ? ' (and a new second step is asked for)' : '') + '.'),
      h('div', { class: 'effect' }, 'Do this only when you are sure that the person is who they say they are.')), 'Remove', true);
    if (!ok) return;
    try { await api('DELETE', '/api/admin/mfa?user=' + encodeURIComponent(u.user)); adminUsers(body).catch(() => {}); } catch (e) { setBanner({ title: 'Not removed', error: e.message, running: false }); }
  }
  body.replaceChildren(h('div', { class: 'card' }, h('h2', {}, 'Second step of the login'),
    users.length ? h('div', { class: 'tablewrap', style: 'box-shadow:none' }, h('table', {}, h('thead', {}, h('tr', {}, ['Person', 'Authenticator apps', 'Passkeys', 'Recovery codes left', ''].map((t) => h('th', {}, t)))),
      h('tbody', {}, users.map((u) => h('tr', {}, h('td', { class: 'name' }, u.user), h('td', { class: 'num' }, u.totp), h('td', { class: 'num' }, u.passkeys), h('td', { class: 'num' }, u.recovery), h('td', { class: 'num' }, h('button', { class: 'small', onclick: () => reset(u) }, 'Remove it'))))))) : h('p', { class: 'muted' }, 'Nobody has set up a second step yet. People do it under their name at the top right.'),
    h('p', { class: 'note' }, 'When nobody can log in: on this server, pdumonitord mfa-list and pdumonitord mfa-reset -user ldap:NAME (see the README).')));
}

// ---- names of the outlets: servman says which server is on which outlet, the PDUs are named after it
async function adminNames(body) {
  let st = await api('GET', '/api/admin/servman');
  let busy = false;
  const when = (iso) => (iso ? datetime(iso) : 'never');
  async function refreshNow() {
    busy = true; draw();
    try { st = await api('POST', '/api/admin/servman/refresh', {}); } catch (e) { st = { ...st, planError: e.message }; }
    busy = false; draw();
  }
  async function write(sel, what, count) {
    const list = h('ul', {}, (sel.changes || []).filter(Boolean).slice(0, 10).map((c) => h('li', {}, `${c.pdu} outlet ${c.outlet}: ${c.current || '—'} → ${c.desired}`)), (sel.changes || []).length > 10 ? h('li', { class: 'muted' }, `… and ${sel.changes.length - 10} more`) : null);
    const ok = await confirmDialog(`Write ${count} outlet name${count === 1 ? '' : 's'}?`, h('div', {}, h('div', {}, what), list,
      h('div', { class: 'effect' }, 'The names are written to the PDUs (SNMP). The PDUs are read again afterwards and the names are compared with what was meant; every writing is in the log.')), 'Write', false);
    if (!ok) return;
    try { st = await api('POST', '/api/admin/servman/apply', { all: !!sel.all, pdu: sel.pdu || '', outlet: sel.outlet || 0, outlets: sel.outlets || [], confirm: true }); } catch (e) { setBanner({ title: 'Not written', error: e.message, running: false }); return; }
    draw();
  }
  function draw() {
    if (dialogOpen()) return;
    if (!st.configured) {
      body.replaceChildren(h('div', { class: 'card', style: 'max-width:720px' }, h('h2', {}, 'Servman is not configured'),
        h('p', { class: 'muted' }, 'The outlets of the PDUs are named after the servers of servman (200U13). Add the connection to config.yaml on the server and restart it:'),
        h('pre', { class: 'key' }, 'servman:\n  url: "https://replace_me.example.com"\n  token_file: /etc/pdu-monitor/servman-token   # one line, chmod 640 root:pdumonitor\n  ca_file: /etc/pdu-monitor/servman-ca.pem     # or: insecure_skip_verify: true'),
        h('p', { class: 'note' }, 'How often it is asked and written, and the form of the names, are under Settings.')));
      return;
    }
    const plan = st.plan;
    const changes = plan ? plan.changes : [];
    const ready = changes.filter((c) => !c.blocked);
    const groups = {};
    for (const c of changes) (groups[c.pdu] = groups[c.pdu] || []).push(c);
    const head = h('div', { class: 'card', style: 'margin-bottom:14px' },
      h('div', { style: 'display:flex;gap:14px;align-items:center;flex-wrap:wrap' },
        h('div', {}, h('div', {}, `Compared: ${when(st.planAt)}`), h('div', { class: 'muted small' }, st.syncAt ? `Last writing: ${when(st.syncAt)} — ${st.syncResult || ''}` : 'Nothing written yet')),
        h('div', { class: 'spacer' }),
        st.writing ? h('div', { style: 'display:flex;gap:8px;align-items:center' }, h('div', { class: 'spinner' }), 'Writing…') : null,
        h('button', { disabled: busy || st.writing, onclick: refreshNow }, busy ? 'Asking servman…' : 'Compare now'),
        h('button', { class: 'primary', disabled: busy || st.writing || !ready.length, onclick: () => write({ all: true, changes: ready }, 'Everything in the plan:', ready.length) }, `Write all (${ready.length})`)),
      st.planError ? h('p', { class: 'error', style: 'margin:10px 0 0' }, st.planError) : null,
      st.resultError ? h('p', { class: 'error', style: 'margin:10px 0 0' }, st.resultError) : null,
      plan ? h('p', { class: 'note' }, `${plan.devices} devices of servman have cables · ${plan.unchanged} outlets already have the right name · ${changes.length} to change · name form: ${plan.format === 'ccid_name' ? 'id and machine name' : 'rack unit id'}`) : null);
    // what failed the last time: the ones that are still different now can be tried again; the others got their name after all
    const pending = new Set(changes.map((c) => c.pdu + '#' + c.outlet));
    const lastFailed = st.result && st.result.failed ? st.result.failed : [];
    const stillFailed = lastFailed.filter((f) => pending.has(f.pdu + '#' + f.outlet));
    const retryable = stillFailed.filter((f) => !(changes.find((c) => c.pdu === f.pdu && c.outlet === f.outlet) || {}).blocked);
    const failed = stillFailed.length ? h('div', { class: 'card', style: 'margin-bottom:14px' },
      h('div', { style: 'display:flex;gap:12px;align-items:center;flex-wrap:wrap' }, h('h3', {}, `Not written (${stillFailed.length})`), h('div', { class: 'spacer' }),
        h('button', { class: 'primary', disabled: busy || st.writing || !retryable.length, onclick: () => write({ outlets: retryable.map((f) => ({ pdu: f.pdu, outlet: f.outlet })), changes: retryable.map((f) => changes.find((c) => c.pdu === f.pdu && c.outlet === f.outlet)) }, 'Trying these again:', retryable.length) }, `Try again (${retryable.length})`)),
      h('ul', {}, stillFailed.map((f) => h('li', {}, h('span', { class: 'error' }, `${f.pdu} outlet ${f.outlet} → ${f.desired}: `), f.error))),
      lastFailed.length > stillFailed.length ? h('p', { class: 'note' }, `${lastFailed.length - stillFailed.length} more failed to answer but have the right name now.`) : null) : null;
    const conflicts = plan && plan.conflicts && plan.conflicts.length ? h('div', { class: 'notice bad conflicts', role: 'alert', style: 'margin-bottom:14px' },
      h('strong', {}, `${plan.conflicts.length} conflict${plan.conflicts.length === 1 ? '' : 's'} in servman: two devices on one outlet. Fix them in servman, then press "Compare now".`),
      h('ul', { style: 'margin:8px 0 0;padding-left:20px' }, plan.conflicts.map((c) => h('li', {}, c.text))),
      h('div', { class: 'small muted', style: 'margin-top:4px' }, 'Nothing is written to those outlets. The overview shows this list to everybody until it is empty.')) : null;
    const clean = plan && !(plan.conflicts && plan.conflicts.length) ? h('p', { class: 'c-ok', style: 'margin:0 0 12px' }, '✓ No conflicts: every outlet of servman has one device.') : null;
    const table = changes.length ? h('div', { class: 'tablewrap', style: 'margin-bottom:14px' }, h('table', {}, h('thead', {}, h('tr', {}, ['PDU', 'Outlet', 'Now', 'Will be', 'Server', ''].map((t) => h('th', {}, t)), h('th'))),
      h('tbody', {}, Object.keys(groups).sort().flatMap((pduName) => {
        const g = groups[pduName], rdy = g.filter((c) => !c.blocked);
        return [h('tr', { class: 'group-row' }, h('td', { colspan: 6, class: 'name' }, `${pduName} · ${g.length} to change`),
          h('td', { class: 'num' }, h('button', { class: 'small', disabled: busy || st.writing || !rdy.length, onclick: () => write({ pdu: pduName, changes: rdy }, `All planned names of ${pduName}:`, rdy.length) }, `Write ${rdy.length}`))),
          ...g.map((c) => h('tr', { class: c.blocked ? 'dim' : '' }, h('td', {}, c.pdu), h('td', { class: 'num' }, c.outlet), h('td', { class: 'muted' }, c.current || '—'), h('td', { class: 'name' }, c.desired),
            h('td', { class: 'muted' }, c.host ? c.host : ''),
            h('td', {}, h('span', { class: 'pill ' + (c.kind === 'fill' ? 'ok' : 'warning'), title: c.kind === 'fill' ? 'the outlet still has the default name' : 'replaces another name' }, c.kind === 'fill' ? 'new' : 'rename'), c.blocked ? h('span', { class: 'c-over small' }, '  ' + c.blocked) : null, c.note ? h('span', { class: 'muted small' }, '  ' + c.note) : null),
            h('td', { class: 'num' }, h('button', { class: 'small', disabled: busy || st.writing || !!c.blocked, onclick: () => write({ pdu: c.pdu, outlet: c.outlet, changes: [c] }, 'One outlet:', 1) }, 'Write'))))];
      })))) : (plan ? h('p', { class: 'c-ok' }, 'The names on the PDUs match servman: nothing to write.') : h('p', { class: 'muted' }, 'Press "Compare now" to see what would change.'));
    const warnings = plan && plan.warnings.length ? h('div', { class: 'card', style: 'margin-bottom:14px' }, h('h3', {}, `Things that do not fit (${plan.warnings.length}): nothing is written for them`),
      h('ul', {}, plan.warnings.slice(0, 50).map((w) => h('li', {}, w.text))), plan.warnings.length > 50 ? h('p', { class: 'muted small' }, `… and ${plan.warnings.length - 50} more`) : null) : null;
    const unmapped = plan && plan.unmapped.length ? h('details', { class: 'card' }, h('summary', {}, `${plan.unmapped.length} outlets have a name that servman does not know (never changed)`),
      h('p', { class: 'muted small' }, plan.unmapped.slice(0, 200).map((u) => `${u.pdu} #${u.outlet}: ${u.name}`).join('   ·   '))) : null;
    body.replaceChildren(...[head, conflicts, clean, failed, table, warnings, unmapped].filter(Boolean));
  }
  draw();
  // while it writes the page follows the progress
  stopPolling();
  S.refresh = async () => { try { st = await api('GET', '/api/admin/servman'); draw(); } catch (e) { /* the next round */ } };
  S.timer = setInterval(() => { if (!document.hidden && !dialogOpen()) S.refresh(); }, 3000);
}

// The versions of this server and of the collectors, and the buttons that update them. Nothing connects anywhere from here: the server
// asks the root side of its own machine (a file), the collectors hear it in the answer to their next report (up to a minute).
async function adminSoftware(body) {
  let st = await api('GET', '/api/admin/update');
  let hosts = (await api('GET', '/api/hosts')).hosts || [];
  let showAllHosts = false, rounds = 0;
  const loadedVersion = (S.session && S.session.version) || st.version;
  let waiting = false, down = false;
  const stateText = (sv) => {
    if (sv.pending) return ['Waiting for the updater to start…', ''];
    switch (sv.state) {
      case 'running': return ['Updating: ' + (sv.detail || ''), ''];
      case 'ok': return ['Updated: ' + sv.detail, 'c-ok'];
      case 'uptodate': return ['Nothing to do: ' + sv.detail, 'c-ok'];
      case 'failed': return ['Not updated: ' + sv.detail, 'c-over'];
      default: return ['No update has been made from here yet.', 'muted'];
    }
  };
  async function updateServer() {
    const ok = await confirmDialog('Update this server?', h('div', {}, h('p', {}, 'The newest version of the repository clone on this machine is built and installed, and the server restarts. The web interface is away for about half a minute.'),
      h('div', { class: 'effect' }, 'If the new version does not stay up, the old one is put back by itself. Everything is in the log.')), 'Update', false);
    if (!ok) return;
    try { await api('POST', '/api/admin/update/server', { confirm: true }); waiting = true; } catch (e) { setBanner({ title: 'Not started', error: e.message, running: false }); }
    refresh();
  }
  async function updateCollectors(name) {
    const ok = await confirmDialog(name ? `Update ${name}?` : 'Update all collectors?', h('div', {}, h('p', {}, 'Each collector hears it in the answer to its next report (up to a minute), builds the newest version on its own machine and restarts.'),
      h('div', { class: 'effect' }, 'Nothing here connects to the collectors. A collector without the updater installed (re-run its deploy/collector/install.sh once) does nothing; the request is dropped after 15 minutes.')), 'Update', false);
    if (!ok) return;
    try { await api('POST', '/api/admin/update/collectors', { confirm: true, collector: name || '' }); } catch (e) { setBanner({ title: 'Not started', error: e.message, running: false }); }
    refresh();
  }
  function draw() {
    if (dialogOpen()) return;
    const sv = st.server;
    const [text, cls] = stateText(sv);
    const running = sv.pending || sv.state === 'running';
    const newer = st.version !== loadedVersion;
    const serverCard = h('div', { class: 'card', style: 'margin-bottom:14px' }, h('h2', {}, 'This server'),
      h('div', { style: 'display:flex;gap:14px;align-items:center;flex-wrap:wrap' },
        h('div', {}, h('div', {}, 'Version ', h('strong', { class: 'mono' }, st.version)), h('div', { class: 'small ' + cls }, text + (sv.at && sv.state ? ' · ' + datetime(sv.at) : ''))),
        h('div', { class: 'spacer' }),
        down ? h('div', { style: 'display:flex;gap:8px;align-items:center' }, h('div', { class: 'spinner' }), 'The server is restarting…') : (running ? h('div', { class: 'spinner' }) : null),
        h('button', { class: 'primary', disabled: running || down || !sv.available, title: sv.available ? '' : 'The updater is not installed on this machine', onclick: updateServer }, 'Update the server')),
      !sv.available ? h('p', { class: 'note' }, 'The updater is not installed here yet. Once: sudo deploy/install.sh --binary ./pdumonitord (from the clone), which also puts REPO_DIR (the clone) into /etc/pdu-monitor/update.conf.') : null,
      newer ? h('p', { class: 'notice', style: 'margin:10px 0 0' }, 'The server has a new version. ', h('button', { class: 'link', onclick: () => location.reload() }, 'Reload this page'), ' (or Ctrl+F5) to get the new screens.') : null,
      sv.log && sv.log.length ? h('details', { style: 'margin-top:10px' }, h('summary', {}, 'Output of the last update'), h('pre', { class: 'key' }, sv.log.join('\n'))) : null);
    const cols = st.collectors || [];
    const colCard = cols.length ? h('div', { class: 'card' }, h('div', { style: 'display:flex;gap:14px;align-items:center;flex-wrap:wrap' }, h('h2', {}, 'Relays (pdu-collector)'), h('div', { class: 'spacer' }),
        h('button', { class: 'primary', disabled: cols.some((c) => c.requested), onclick: () => updateCollectors('') }, 'Update all relays')),
      h('div', { class: 'tablewrap', style: 'box-shadow:none;margin-top:8px' }, h('table', {}, h('thead', {}, h('tr', {}, ['Relay', 'Version', 'Last report', ''].map((t) => h('th', {}, t)))),
        h('tbody', {}, cols.map((c) => {
          const age = c.lastSeen ? Math.round((Date.now() - new Date(c.lastSeen).getTime()) / 1000) : null;
          const same = c.version && c.version === st.version;
          return h('tr', {}, h('td', { class: 'name' }, c.name), h('td', { class: 'mono' }, c.version || '—', c.version ? h('span', { class: 'small ' + (same ? 'c-ok' : 'muted'), style: 'margin-left:8px' }, same ? 'same as the server' : 'differs from the server') : null),
            h('td', {}, age == null ? h('span', { class: 'c-over' }, 'never') : h('span', { class: age > 180 ? 'c-over' : 'c-ok' }, ago(Math.max(age, 0)) + ' ago')),
            h('td', { class: 'num' }, c.requested ? h('span', { class: 'muted small' }, 'asked, waiting for its next report…') : h('button', { class: 'small', onclick: () => updateCollectors(c.name) }, 'Update')));
        })))),
      h('p', { class: 'note' }, 'A collector tells its version with every report. A different version after the update means it worked; the same one means it was already the newest (or the updater is not installed there: see its log with journalctl -u pdu-collector-update).')) : h('div', { class: 'card' }, h('h2', {}, 'Relays (pdu-collector)'), h('p', { class: 'muted' }, 'No pdu-collector relay is configured on this server.'));
    // the agents on the hypervisors: they update themselves every night (hostinfo-update); the newest version that any host reports is "current"
    const vkey = (v) => (v || '').split('.').map((n) => parseInt(n, 10) || 0);
    const vcmp = (a, b) => { const x = vkey(a), y = vkey(b); for (let i = 0; i < Math.max(x.length, y.length); i++) { const d = (x[i] || 0) - (y[i] || 0); if (d) return d; } return 0; };
    const withVersion = hosts.filter((x) => x.agent);
    const newest = withVersion.reduce((m, x) => (m === '' || vcmp(x.agent, m) > 0 ? x.agent : m), '');
    const behind = (x) => !x.agent || vcmp(x.agent, newest) < 0;
    const sorted = hosts.slice().sort((a, b) => (behind(b) - behind(a)) || (vcmp(a.agent, b.agent)) || a.name.localeCompare(b.name));
    const shown = showAllHosts || sorted.length <= 15 ? sorted : sorted.filter(behind);
    const hostCard = h('div', { class: 'card', style: 'margin-top:14px' }, h('h2', {}, 'Agents on the hosts'),
      !hosts.length ? h('p', { class: 'muted' }, 'No host yet.') : [
        h('p', { class: 'small', style: 'margin:0 0 8px' }, newest ? [`${hosts.length - hosts.filter(behind).length} of ${hosts.length} hosts run the newest version seen, `, h('strong', { class: 'mono' }, newest), '.'] : 'No host has told its version yet.'),
        shown.length ? h('div', { class: 'tablewrap', style: 'box-shadow:none' }, h('table', {}, h('thead', {}, h('tr', {}, ['Host', 'Agent', 'Data', 'Read by'].map((t) => h('th', {}, t)))),
          h('tbody', {}, shown.map((x) => h('tr', {}, h('td', { class: 'name' }, h('a', { href: '#/host/' + x.id }, x.name)),
            h('td', { class: 'mono' }, x.agent || '—', x.agent && behind(x) ? h('span', { class: 'small c-warning', style: 'margin-left:8px' }, 'older') : null),
            h('td', {}, x.ageSeconds == null ? h('span', { class: 'muted' }, 'none') : h('span', { class: x.online ? 'c-ok' : 'c-over' }, ago(x.ageSeconds) + ' old')),
            h('td', { class: 'muted' }, x.collector ? 'via ' + x.collector : 'this server')))))) : h('p', { class: 'c-ok' }, 'Every host runs the newest version seen.'),
        sorted.length > 15 ? h('button', { class: 'link', style: 'margin-top:8px', onclick: () => { showAllHosts = !showAllHosts; draw(); } }, showAllHosts ? 'Show only the ones that are behind' : `Show all ${sorted.length}`) : null,
        h('p', { class: 'note' }, 'The agents update themselves every night from the main branch (hostinfo-update). At once, on one host: hostinfo-update (as root). The server cannot know the newest version of GitHub: "older" means older than the newest one that a host reports.')]);
    body.replaceChildren(serverCard, colCard, hostCard);
  }
  async function refresh() {
    if (++rounds % 10 === 0) { try { hosts = (await api('GET', '/api/hosts')).hosts || hosts; } catch (e) { /* the next round */ } }
    try { st = await api('GET', '/api/admin/update'); down = false; if (st.server.state && st.server.state !== 'running' && !st.server.pending) waiting = false; } catch (e) { if (e instanceof ApiError && e.status === 401) return; down = waiting; }
    draw();
  }
  draw();
  stopPolling();
  S.refresh = refresh;
  S.timer = setInterval(() => { if (!document.hidden && !dialogOpen()) refresh(); }, 3000);
}

function adminImport(body, reload) {
  const out = h('div');
  const file = h('input', { type: 'file', accept: '.json,application/json' });
  const go = h('button', { class: 'primary', onclick: async () => {
    out.replaceChildren();
    if (!file.files[0]) { out.append(h('p', { class: 'error' }, 'Choose the file first.')); return; }
    try {
      const r = await api('POST', '/api/admin/import', await file.files[0].text());
      add(out, h('p', { class: 'c-ok' }, `Added ${r.racks} racks, ${r.pdus} PDUs and ${r.labels} names.`),
        r.skipped && r.skipped.length ? h('p', { class: 'muted small' }, 'Already there, not touched: ' + r.skipped.join(', ')) : null,
        r.withoutCommunity && r.withoutCommunity.length ? h('p', { class: 'notice' }, 'These PDUs came without a community: open them under PDUs and enter it: ' + r.withoutCommunity.join(', ')) : null);
    } catch (e) { out.append(h('p', { class: 'error' }, e.message)); }
  } }, 'Import');
  body.append(h('div', { class: 'card', style: 'max-width:640px' }, h('h2', {}, 'Settings of the Mac application'),
    h('p', { class: 'muted' }, 'Choose the file made by “Export settings” in the Mac application. Racks and PDUs that are already here are not changed.'), h('div', { class: 'actionbar' }, file, go), out));
}

async function adminAudit(body) {
  const r = await api('GET', '/api/admin/audit?limit=300');
  body.append(r.entries.length ? h('div', { class: 'tablewrap' }, h('table', {}, h('thead', {}, h('tr', {}, ['Time', 'User', 'What', 'Details'].map((t) => h('th', {}, t)))),
    h('tbody', {}, r.entries.map((e) => h('tr', { class: e.ok ? '' : 'bad' }, h('td', {}, datetime(e.time)), h('td', {}, e.user), h('td', { class: e.ok ? '' : 'c-over' }, e.action), h('td', { style: 'white-space:normal' }, e.detail))))))
    : h('p', { class: 'muted' }, 'Nothing has been logged yet.'));
}

// ---------------------------------------------------------------- start
(async () => {
  try {
    const r = await fetch('/api/session', { credentials: 'same-origin' });
    const data = await r.json().catch(() => ({}));
    S.demoHint = !!data.demo;
    if (r.ok) { S.session = data; S.csrf = data.csrf; S.settings = data.settings || {}; route(); } else showLogin();
  } catch (e) { app.replaceChildren(h('p', { class: 'boot error' }, 'The server does not answer: ' + e.message)); }
})();
})();
