// Emberwire editor.
//
// Vanilla TypeScript and native DOM. Node-RED's editor is roughly 40,000 lines
// of jQuery and D3 plus a vendored Monaco, none of which can be themed to look
// like ours, and all of which would have to be served from the binary.

import './theme.css';
import './canvas.css';
import { Api, ApiError, type Descriptor, type Settings } from './api';
import { mountEditor, type EditorHandles } from './editor';

const api = new Api('');
const app = document.getElementById('app')!;
let editor: EditorHandles | null = null;

// ── Theme ────────────────────────────────────────────────────────────────────

/**
 * Applies a theme and remembers it.
 *
 * Mirrors the dashboard exactly: a class on body, a localStorage key, and a
 * prefers-color-scheme fallback. The pending class the inline head script sets
 * is cleared here once the real class is on.
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
    let hasChoice = false;
    try {
      hasChoice = localStorage.getItem('theme') !== null;
    } catch {
      /* ignore */
    }
    // Only follow the OS when the user has not chosen for themselves.
    if (!hasChoice) applyTheme(e.matches, false);
  });
}

/**
 * Follows the dashboard's theme when embedded in its iframe.
 *
 * Emberwire opens inside a fullscreen overlay in the Industrial Dashboard.
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
    if (data?.type === 'embernet:theme' && (data.theme === 'dark' || data.theme === 'light')) {
      applyTheme(data.theme === 'dark', false);
    }
  });
  window.parent.postMessage({ type: 'embernet:ready', app: 'emberwire' }, window.location.origin);
}

// ── Login ────────────────────────────────────────────────────────────────────

function renderLogin(message?: string): void {
  editor?.destroy();
  editor = null;
  app.replaceChildren();
  app.className = '';

  const err = document.createElement('div');
  err.className = 'error';
  if (message) err.textContent = message;

  const user = Object.assign(document.createElement('input'), {
    type: 'text', id: 'u', autocomplete: 'username', value: 'admin',
  });
  const pass = Object.assign(document.createElement('input'), {
    type: 'password', id: 'p', autocomplete: 'current-password',
  });
  const submit = Object.assign(document.createElement('button'), {
    type: 'submit', textContent: 'Sign in',
  });

  const title = document.createElement('h1');
  title.append('Ember');
  const mark = document.createElement('span');
  mark.className = 'mark';
  mark.textContent = 'wire';
  title.append(mark);

  const sub = document.createElement('p');
  sub.textContent = 'Sign in to the runtime.';

  const labelU = document.createElement('label');
  labelU.htmlFor = 'u';
  labelU.textContent = 'Username';
  const labelP = document.createElement('label');
  labelP.htmlFor = 'p';
  labelP.textContent = 'Password';

  const form = document.createElement('form');
  form.append(title, sub, err, labelU, user, labelP, pass, submit);

  form.onsubmit = async (e) => {
    e.preventDefault();
    err.textContent = '';
    submit.disabled = true;
    submit.textContent = 'Signing in…';
    try {
      await api.login(user.value, pass.value);
      await start();
    } catch (ex) {
      err.textContent = ex instanceof ApiError ? ex.message : 'could not reach the runtime';
      submit.disabled = false;
      submit.textContent = 'Sign in';
      pass.focus();
    }
  };

  const card = document.createElement('div');
  card.className = 'login';
  card.append(form);
  const wrap = document.createElement('div');
  wrap.className = 'login-wrap';
  wrap.append(card);
  app.append(wrap);
  pass.focus();
}

// ── Boot ─────────────────────────────────────────────────────────────────────

async function start(): Promise<void> {
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

  editor = mountEditor(
    app,
    api,
    descriptors,
    settings.version,
    () => { api.logout(); renderLogin(); },
    () => renderLogin('Session expired. Sign in again.'),
  );
}

applyTheme(initialTheme(), false);
watchSystemTheme();
followParentTheme();

if (api.authenticated) {
  void start();
} else {
  renderLogin();
}
