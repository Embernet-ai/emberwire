// Emberwire editor.
//
// Vanilla TypeScript and native DOM. No jQuery, no D3, no framework — Node-RED's
// editor is roughly 40,000 lines of jQuery and D3 plus a vendored Monaco, and
// none of it can be themed to look like ours.
//
// This is the shell: login, live runtime state, the palette, and the event
// stream. The flow canvas goes in next, on top of the same API client and the
// same theme.

import './theme.css';
import { Api, ApiError, type Descriptor, type NodeStat, type RuntimeEvent, type Settings } from './api';

const api = new Api('');
const app = document.getElementById('app')!;

// ── Theme ────────────────────────────────────────────────────────────────────

/**
 * Applies a theme and remembers it.
 *
 * Mirrors the dashboard exactly: a class on body, a localStorage key, and a
 * prefers-color-scheme fallback. The pending class set by the inline head
 * script is cleared here once the real class is on.
 */
function applyTheme(dark: boolean, remember = true): void {
  document.body.classList.toggle('dark-mode', dark);
  document.documentElement.classList.remove('dark-mode-pending');
  if (remember) {
    try {
      localStorage.setItem('theme', dark ? 'dark' : 'light');
    } catch {
      /* private mode */
    }
  }
  const icon = document.getElementById('theme-icon');
  if (icon) icon.textContent = dark ? '☀️' : '🌙';
}

function initialTheme(): boolean {
  try {
    const stored = localStorage.getItem('theme');
    if (stored) return stored === 'dark';
  } catch {
    /* fall through */
  }
  return window.matchMedia('(prefers-color-scheme: dark)').matches;
}

function watchSystemTheme(): void {
  window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', (e) => {
    // Only follow the OS when the user has not chosen for themselves.
    let hasChoice = false;
    try {
      hasChoice = localStorage.getItem('theme') !== null;
    } catch {
      /* ignore */
    }
    if (!hasChoice) applyTheme(e.matches, false);
  });
}

/**
 * Follows the dashboard's theme when embedded in its iframe.
 *
 * Emberwire is opened inside a fullscreen overlay in the Industrial Dashboard.
 * Without this, toggling the dashboard's theme leaves the embedded app on the
 * other one, which looks broken even though both are individually correct.
 */
function followParentTheme(): void {
  if (window.parent === window) return;
  window.addEventListener('message', (ev) => {
    // Same-origin only: the dashboard proxies this app so the browser sees one
    // origin. A message from anywhere else is not ours to trust.
    if (ev.origin !== window.location.origin) return;
    const data = ev.data as { type?: string; theme?: string } | null;
    if (data && data.type === 'embernet:theme' && (data.theme === 'dark' || data.theme === 'light')) {
      applyTheme(data.theme === 'dark', false);
    }
  });
  window.parent.postMessage({ type: 'embernet:ready', app: 'emberwire' }, window.location.origin);
}

// ── Rendering helpers ────────────────────────────────────────────────────────

function el<K extends keyof HTMLElementTagNameMap>(
  tag: K,
  attrs: Record<string, string> = {},
  ...children: (Node | string)[]
): HTMLElementTagNameMap[K] {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === 'class') node.className = v;
    else node.setAttribute(k, v);
  }
  for (const c of children) node.append(c);
  return node;
}

function fmt(n: number): string {
  return n.toLocaleString();
}

// ── Login ────────────────────────────────────────────────────────────────────

function renderLogin(message?: string): void {
  app.replaceChildren();

  const err = el('div', { class: 'error' });
  if (message) err.textContent = message;

  const user = el('input', { type: 'text', id: 'u', autocomplete: 'username', value: 'admin' });
  const pass = el('input', { type: 'password', id: 'p', autocomplete: 'current-password' });
  const submit = el('button', { type: 'submit' }, 'Sign in');

  const form = el('form', {});
  form.append(
    el('h1', {}, 'Ember', el('span', { class: 'mark' }, 'wire')),
    el('p', {}, 'Sign in to the runtime.'),
    err,
    el('label', { for: 'u' }, 'Username'),
    user,
    el('label', { for: 'p' }, 'Password'),
    pass,
    submit,
  );

  form.onsubmit = async (e) => {
    e.preventDefault();
    err.textContent = '';
    submit.disabled = true;
    submit.textContent = 'Signing in…';
    try {
      await api.login(user.value, pass.value);
      await renderApp();
    } catch (ex) {
      err.textContent = ex instanceof ApiError ? ex.message : 'could not reach the runtime';
      submit.disabled = false;
      submit.textContent = 'Sign in';
      pass.focus();
    }
  };

  const wrap = el('div', { class: 'login-wrap' });
  const card = el('div', { class: 'login' });
  card.append(form);
  wrap.append(card);
  app.append(wrap);
  (message ? pass : pass).focus();
}

// ── Main view ────────────────────────────────────────────────────────────────

let disconnect: (() => void) | null = null;
let statsTimer: number | undefined;

async function renderApp(): Promise<void> {
  let settings: Settings;
  let descriptors: Descriptor[];
  try {
    [settings, descriptors] = await Promise.all([api.settings(), api.nodes()]);
  } catch (ex) {
    if (ex instanceof ApiError && ex.status === 401) {
      renderLogin('Session expired. Sign in again.');
      return;
    }
    renderLogin(ex instanceof Error ? ex.message : 'could not reach the runtime');
    return;
  }

  app.replaceChildren();

  // Topbar
  const connDot = el('span', { class: 'dot grey', id: 'conn-dot' });
  const connText = el('span', { id: 'conn-text' }, 'connecting');
  const themeBtn = el('button', { class: 'icon', title: 'Toggle theme' });
  themeBtn.append(el('span', { id: 'theme-icon' }, '🌙'));
  themeBtn.onclick = () => applyTheme(!document.body.classList.contains('dark-mode'));

  const logoutBtn = el('button', { class: 'ghost' }, 'Sign out');
  logoutBtn.onclick = () => {
    disconnect?.();
    if (statsTimer) window.clearInterval(statsTimer);
    api.logout();
    renderLogin();
  };

  const topbar = el('div', { class: 'topbar' });
  topbar.append(
    el('div', { class: 'brand' },
      'Ember', el('span', { class: 'mark' }, 'wire'),
      el('span', { class: 'version' }, settings.version)),
    el('div', { class: 'spacer' }),
    el('div', { class: 'mono', style: 'font-size:12px;color:var(--text-sub)' }, connDot, connText),
    themeBtn,
    logoutBtn,
  );

  const container = el('div', { class: 'container' });

  // Stat tiles
  const statNodes = el('div', { class: 'stat accent' }, '—');
  const statMsgs = el('div', { class: 'stat' }, '—');
  const statErrors = el('div', { class: 'stat' }, '—');
  const statQueue = el('div', { class: 'stat' }, '—');

  const tiles = el('div', { class: 'grid' });
  tiles.append(
    tile('Running nodes', statNodes, 'instances in this runtime'),
    tile('Messages', statMsgs, 'received since start'),
    tile('Errors', statErrors, 'raised since start'),
    tile('Queued', statQueue, `deepest inbox of ${fmt(settings.runtime.inboxCapacity)}`),
  );

  // Runtime table
  const statsBody = el('tbody');
  const statsCard = el('div', { class: 'card' });
  statsCard.append(
    el('h2', {}, 'Runtime'),
    el('div', { class: 'scroll-x' },
      (() => {
        const t = el('table');
        t.append(
          el('thead', {}, (() => {
            const tr = el('tr');
            for (const h of ['Node', 'Type', 'Received', 'Sent', 'Errors', 'Dropped', 'Queue']) {
              tr.append(el('th', h === 'Node' || h === 'Type' ? {} : { class: 'num' }, h));
            }
            return tr;
          })()),
          statsBody,
        );
        return t;
      })()),
  );

  // Event log
  const logBody = el('div', { class: 'log' });
  logBody.append(el('div', { class: 'empty' }, 'Waiting for events…'));
  const clearBtn = el('button', { class: 'ghost' }, 'Clear');
  clearBtn.onclick = () => logBody.replaceChildren(el('div', { class: 'empty' }, 'Cleared.'));

  const logHead = el('div', { style: 'display:flex;align-items:center;gap:12px' });
  logHead.append(el('h2', { style: 'margin:0;flex:1' }, 'Events'), clearBtn);

  const logCard = el('div', { class: 'card' });
  logCard.append(logHead, el('div', { style: 'height:12px' }), logBody);

  // Palette
  const paletteCard = el('div', { class: 'card' });
  paletteCard.append(el('h2', {}, `Palette — ${descriptors.length} node types`));
  paletteCard.append(renderPalette(descriptors));

  container.append(tiles, statsCard, el('div', { style: 'height:16px' }), logCard,
    el('div', { style: 'height:16px' }), paletteCard);
  app.append(topbar, container);

  // Live updates
  const refresh = async () => {
    try {
      const { nodes } = await api.stats();
      updateStats(statsBody, nodes, statNodes, statMsgs, statErrors, statQueue);
    } catch (ex) {
      if (ex instanceof ApiError && ex.status === 401) {
        disconnect?.();
        if (statsTimer) window.clearInterval(statsTimer);
        renderLogin('Session expired. Sign in again.');
      }
    }
  };
  await refresh();
  statsTimer = window.setInterval(refresh, 2000);

  disconnect = api.connectEvents(
    (e) => appendEvent(logBody, e),
    (up) => {
      connDot.className = `dot ${up ? 'green' : 'red'}`;
      connText.textContent = up ? 'live' : 'reconnecting';
    },
  );
}

function tile(label: string, value: HTMLElement, sub: string): HTMLElement {
  const card = el('div', { class: 'card' });
  card.append(el('h2', {}, label), value, el('div', { class: 'stat-label' }, sub));
  return card;
}

function updateStats(
  tbody: HTMLElement,
  nodes: NodeStat[],
  tNodes: HTMLElement,
  tMsgs: HTMLElement,
  tErrs: HTMLElement,
  tQueue: HTMLElement,
): void {
  let received = 0;
  let errors = 0;
  let deepest = 0;

  tbody.replaceChildren();
  for (const n of nodes) {
    received += n.received;
    errors += n.errors;
    deepest = Math.max(deepest, n.queueLen);

    const ratio = n.queueCap > 0 ? n.queueLen / n.queueCap : 0;
    const bar = el('span', {
      class: ratio > 0.8 ? 'hot' : ratio > 0.4 ? 'warn' : '',
      style: `width:${Math.min(100, Math.round(ratio * 100))}%`,
    });
    const meter = el('span', { class: 'meter' });
    meter.append(bar);

    const tr = el('tr');
    tr.append(
      el('td', { class: 'mono' }, n.nodeId),
      el('td', {}, n.type),
      el('td', { class: 'num' }, fmt(n.received)),
      el('td', { class: 'num' }, fmt(n.sent)),
      el('td', { class: 'num' }, n.errors > 0 ? el('strong', { style: 'color:var(--danger)' }, fmt(n.errors)) : '0'),
      el('td', { class: 'num' }, n.dropped > 0 ? el('strong', { style: 'color:var(--warning)' }, fmt(n.dropped)) : '0'),
      (() => {
        const td = el('td', { class: 'num' }, `${n.queueLen} / ${n.queueCap}`);
        td.append(meter);
        return td;
      })(),
    );
    tbody.append(tr);
  }

  if (nodes.length === 0) {
    const tr = el('tr');
    tr.append(el('td', { colspan: '7' }, el('div', { class: 'empty' }, 'No flows are running.')));
    tbody.append(tr);
  }

  tNodes.textContent = fmt(nodes.length);
  tMsgs.textContent = fmt(received);
  tErrs.textContent = fmt(errors);
  tQueue.textContent = fmt(deepest);
  tErrs.className = errors > 0 ? 'stat' : 'stat';
  tErrs.style.color = errors > 0 ? 'var(--danger)' : '';
}

const MAX_LOG_LINES = 300;

function appendEvent(container: HTMLElement, e: RuntimeEvent): void {
  const empty = container.querySelector('.empty');
  if (empty) empty.remove();

  const time = new Date(e.at).toLocaleTimeString();
  let body: string;
  switch (e.topic) {
    case 'debug':
      body = `${e.data.name || e.data.id}  ${e.data.msg}`;
      break;
    case 'error':
      body = `${e.data.nodeId ?? ''} ${e.data.error}`.trim();
      break;
    case 'status':
      body = `${e.data.nodeId}  ${e.data.text ?? '(cleared)'}`;
      break;
    case 'dropped':
      body = `${e.data.nodeId} dropped a message (${e.data.policy}, queue ${e.data.queueCap})`;
      break;
    default:
      body = JSON.stringify(e.data);
  }

  const line = el('div', { class: 'log-line' });
  line.append(
    el('span', { class: 'log-time' }, time),
    el('span', { class: `log-topic ${e.topic}` }, e.topic),
    el('span', { class: 'log-body' }, body),
  );
  container.append(line);

  // Bounded, because an unbounded log is how a browser tab left open overnight
  // ends up consuming a gigabyte.
  while (container.children.length > MAX_LOG_LINES) {
    container.firstChild?.remove();
  }
  container.scrollTop = container.scrollHeight;
}

function renderPalette(descriptors: Descriptor[]): HTMLElement {
  const byCategory = new Map<string, Descriptor[]>();
  for (const d of descriptors) {
    const list = byCategory.get(d.category) ?? [];
    list.push(d);
    byCategory.set(d.category, list);
  }

  const wrap = el('div', { class: 'scroll-x' });
  const table = el('table');
  const tbody = el('tbody');

  const head = el('tr');
  for (const h of ['Type', 'Category', 'In', 'Out', 'Compatibility']) {
    head.append(el('th', h === 'In' || h === 'Out' ? { class: 'num' } : {}, h));
  }
  table.append(el('thead', {}, head));

  for (const category of [...byCategory.keys()].sort()) {
    for (const d of byCategory.get(category)!) {
      const badgeClass = d.compatibility.level === 'emberwire-only' ? 'only' : d.compatibility.level;
      const badge = el('span', { class: `badge ${badgeClass}` }, d.compatibility.level);
      if (d.compatibility.notes) badge.title = d.compatibility.notes;

      const tr = el('tr');
      tr.append(
        el('td', {}, el('span', { class: 'dot', style: `background:${d.color}` }), el('span', { class: 'mono' }, d.type)),
        el('td', {}, d.category),
        el('td', { class: 'num' }, d.isConfig ? '—' : String(d.inputs)),
        el('td', { class: 'num' }, d.isConfig ? '—' : String(d.outputs)),
        el('td', {}, badge),
      );
      tbody.append(tr);
    }
  }

  table.append(tbody);
  wrap.append(table);
  return wrap;
}

// ── Boot ─────────────────────────────────────────────────────────────────────

applyTheme(initialTheme(), false);
watchSystemTheme();
followParentTheme();

if (api.authenticated) {
  void renderApp();
} else {
  renderLogin();
}
