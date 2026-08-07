// The editor view: palette, tabs, canvas, deploy, and the live sidebar.

import type { Api, Descriptor, NodeStat, RuntimeEvent } from './api';
import { ApiError } from './api';
import { Canvas } from './canvas';
import { editNode } from './dialog';
import { Graph, type FlowEntry } from './graph';

export interface EditorHandles {
  destroy(): void;
}

export function mountEditor(
  root: HTMLElement,
  api: Api,
  descriptors: Descriptor[],
  version: string,
  onSignOut: () => void,
  onSessionLost: () => void,
): EditorHandles {
  const byType = new Map(descriptors.map((d) => [d.type, d]));
  const graph = new Graph(byType);

  root.replaceChildren();
  root.className = 'editor';

  // ── Chrome ─────────────────────────────────────────────────────────────────
  const connDot = el('span', { class: 'dot grey' });
  const connText = el('span', {}, 'connecting');
  const deployBtn = el('button', { disabled: 'true' }, 'Deploy') as HTMLButtonElement;
  const themeBtn = el('button', { class: 'icon', title: 'Toggle theme' },
    el('span', { id: 'theme-icon' }, '🌙'));
  const fitBtn = el('button', { class: 'icon', title: 'Fit to view' }, '⤢');
  const signOut = el('button', { class: 'ghost' }, 'Sign out');

  const topbar = el('div', { class: 'topbar' },
    el('div', { class: 'brand' }, 'Ember', el('span', { class: 'mark' }, 'wire'),
      el('span', { class: 'version' }, version)),
    el('div', { class: 'spacer' }),
    el('div', { class: 'conn mono' }, connDot, connText),
    fitBtn, themeBtn, deployBtn, signOut,
  );

  const tabBar = el('div', { class: 'tabbar' });
  const palette = el('aside', { class: 'palette' });
  const canvasHost = el('main', { class: 'canvas-host' });
  const sidebar = el('aside', { class: 'sidebar' });

  const workspace = el('div', { class: 'workspace' }, palette, canvasHost, sidebar);
  root.append(topbar, tabBar, workspace);

  // ── Canvas ─────────────────────────────────────────────────────────────────
  const canvas = new Canvas(canvasHost, graph, {
    onEditNode: (entry) => openEditor(entry),
    onSelectionChange: () => renderTabs(),
  });

  async function openEditor(entry: FlowEntry): Promise<void> {
    const result = await editNode(graph, entry, byType.get(entry.type));
    if (result) graph.updateNode(entry.id, result.props);
  }

  // ── Palette ────────────────────────────────────────────────────────────────
  function renderPalette(): void {
    palette.replaceChildren(el('h3', {}, 'Palette'));

    const search = el('input', { type: 'search', placeholder: 'Filter…' }) as HTMLInputElement;
    palette.append(search);

    const list = el('div', { class: 'palette-list' });
    palette.append(list);

    const draw = (filter: string) => {
      list.replaceChildren();
      const groups = new Map<string, Descriptor[]>();
      for (const d of descriptors) {
        if (d.isConfig) continue;
        const hay = `${d.type} ${d.paletteLabel ?? ''} ${d.category}`.toLowerCase();
        if (filter && !hay.includes(filter.toLowerCase())) continue;
        const g = groups.get(d.category) ?? [];
        g.push(d);
        groups.set(d.category, g);
      }

      for (const cat of [...groups.keys()].sort()) {
        list.append(el('div', { class: 'palette-cat' }, cat));
        for (const d of groups.get(cat)!) {
          const item = el('div', {
            class: 'palette-item',
            draggable: 'true',
            title: d.help ?? d.type,
          },
            el('span', { class: 'swatch', style: `background:${d.color}` }),
            el('span', {}, d.paletteLabel ?? d.type),
          );
          item.addEventListener('dragstart', (e) => {
            (e as DragEvent).dataTransfer?.setData('text/emberwire-node', d.type);
          });
          // Double-click drops it at the centre of the view, for anyone who
          // would rather not drag.
          item.addEventListener('dblclick', () => {
            const entry = graph.addNode(d.type, { x: 200, y: 120 });
            void openEditor(entry);
          });
          list.append(item);
        }
      }
      if (list.childElementCount === 0) {
        list.append(el('div', { class: 'empty' }, 'Nothing matches.'));
      }
    };

    search.addEventListener('input', () => draw(search.value));
    draw('');
  }

  // ── Tabs ───────────────────────────────────────────────────────────────────
  function renderTabs(): void {
    tabBar.replaceChildren();
    for (const t of graph.tabs()) {
      const active = t.id === graph.activeTab;
      const tab = el('button', { class: `tab${active ? ' active' : ''}` },
        String(t.label ?? 'Flow'));
      tab.onclick = () => {
        graph.activeTab = t.id;
        graph.selection.clear();
        canvas.render();
        renderTabs();
        canvas.fit();
      };
      tabBar.append(tab);
    }
    const add = el('button', { class: 'tab tab-add', title: 'Add a flow' }, '+');
    add.onclick = () => {
      const label = prompt('Name for the new flow', `Flow ${graph.tabs().length + 1}`);
      if (label) {
        graph.addTab(label);
        renderTabs();
        canvas.render();
      }
    };
    tabBar.append(add, el('div', { class: 'spacer' }));

    const state = el('div', { class: 'tab-state mono' },
      graph.dirty ? 'unsaved changes' : 'saved');
    tabBar.append(state);
    deployBtn.disabled = !graph.dirty;
  }

  // ── Sidebar ────────────────────────────────────────────────────────────────
  const logBody = el('div', { class: 'log' }, el('div', { class: 'empty' }, 'Waiting for events…'));
  const statsBody = el('tbody');

  function renderSidebar(): void {
    const clear = el('button', { class: 'ghost' }, 'Clear');
    clear.onclick = () => logBody.replaceChildren(el('div', { class: 'empty' }, 'Cleared.'));

    sidebar.replaceChildren(
      el('div', { class: 'side-head' }, el('h3', {}, 'Debug'), el('div', { class: 'spacer' }), clear),
      logBody,
      el('div', { class: 'side-head' }, el('h3', {}, 'Runtime')),
      el('div', { class: 'scroll-x' },
        el('table', {},
          el('thead', {}, (() => {
            const tr = el('tr');
            for (const h of ['Node', 'In', 'Out', 'Err', 'Queue']) {
              tr.append(el('th', h === 'Node' ? {} : { class: 'num' }, h));
            }
            return tr;
          })()),
          statsBody)),
    );
  }

  function updateStats(nodes: NodeStat[]): void {
    statsBody.replaceChildren();
    for (const n of nodes) {
      const ratio = n.queueCap > 0 ? n.queueLen / n.queueCap : 0;
      const meter = el('span', { class: 'meter' },
        el('span', {
          class: ratio > 0.8 ? 'hot' : ratio > 0.4 ? 'warn' : '',
          style: `width:${Math.min(100, Math.round(ratio * 100))}%`,
        }));

      const tr = el('tr');
      tr.append(
        el('td', { class: 'mono' }, graph.byId(n.nodeId) ? graph.label(graph.byId(n.nodeId)!) : n.nodeId),
        el('td', { class: 'num' }, String(n.received)),
        el('td', { class: 'num' }, String(n.sent)),
        el('td', { class: 'num' }, n.errors > 0
          ? el('strong', { style: 'color:var(--danger)' }, String(n.errors))
          : '0'),
        (() => { const td = el('td', { class: 'num' }, `${n.queueLen}`); td.append(meter); return td; })(),
      );
      // Clicking a row selects the node on the canvas, which is how you get
      // from "this node is erroring" to the node itself.
      tr.onclick = () => {
        const entry = graph.byId(n.nodeId);
        if (!entry) return;
        if (entry.z) graph.activeTab = entry.z;
        graph.selection = new Set([n.nodeId]);
        renderTabs();
        canvas.render();
      };
      statsBody.append(tr);
    }
  }

  const MAX_LOG = 250;
  function appendEvent(e: RuntimeEvent): void {
    logBody.querySelector('.empty')?.remove();

    // Status events paint the canvas rather than filling the log — a chatty
    // node would otherwise drown out the debug output that was asked for.
    if (e.topic === 'status') {
      const id = String(e.data.nodeId ?? '');
      if (e.data.cleared) canvas.setStatus(id, null);
      else canvas.setStatus(id, {
        fill: String(e.data.fill ?? 'grey'),
        shape: String(e.data.shape ?? 'dot'),
        text: String(e.data.text ?? ''),
      });
      return;
    }

    let text: string;
    switch (e.topic) {
      case 'debug': text = `${e.data.name || e.data.id}  ${e.data.msg}`; break;
      case 'error': text = `${e.data.nodeId ?? ''} ${e.data.error}`.trim(); break;
      case 'dropped':
        text = `${e.data.nodeId} dropped a message (${e.data.policy})`;
        break;
      default: text = JSON.stringify(e.data);
    }

    logBody.append(el('div', { class: 'log-line' },
      el('span', { class: 'log-time' }, new Date(e.at).toLocaleTimeString()),
      el('span', { class: `log-topic ${e.topic}` }, e.topic),
      el('span', { class: 'log-body' }, text)));

    while (logBody.children.length > MAX_LOG) logBody.firstChild?.remove();
    logBody.scrollTop = logBody.scrollHeight;
  }

  // ── Deploy ─────────────────────────────────────────────────────────────────
  deployBtn.onclick = async () => {
    deployBtn.disabled = true;
    deployBtn.textContent = 'Deploying…';
    try {
      const res = await api.deploy(graph.all(), graph.rev);
      graph.rev = res.rev;
      graph.dirty = false;
      renderTabs();

      if (res.failures?.length) {
        // Per-node failures are not a failed deploy — the rest of the flow is
        // running. Reporting them as errors rather than swallowing them is what
        // stops somebody wondering why one node does nothing.
        for (const f of res.failures) {
          appendEvent({
            topic: 'error',
            data: { nodeId: f.id, error: `${f.type}: ${f.error}` },
            at: new Date().toISOString(),
          });
        }
      }
    } catch (ex) {
      if (ex instanceof ApiError && ex.status === 409) {
        // Somebody else deployed in between. Overwriting silently would throw
        // away their work, so it is the operator's call.
        const overwrite = confirm(
          'The flows were changed by someone else since you loaded them.\n\n' +
          'OK to overwrite their version with yours, or Cancel to reload theirs and lose your changes.');
        if (overwrite) {
          const res = await api.deploy(graph.all(), '');
          graph.rev = res.rev;
          graph.dirty = false;
          renderTabs();
        } else {
          await load();
        }
      } else if (ex instanceof ApiError && ex.status === 401) {
        onSessionLost();
        return;
      } else {
        alert(`Deploy failed: ${ex instanceof Error ? ex.message : ex}`);
      }
    } finally {
      deployBtn.textContent = 'Deploy';
      deployBtn.disabled = !graph.dirty;
    }
  };

  // ── Wiring ─────────────────────────────────────────────────────────────────
  fitBtn.onclick = () => canvas.fit();
  themeBtn.onclick = () => {
    const dark = !document.body.classList.contains('dark-mode');
    document.body.classList.toggle('dark-mode', dark);
    try { localStorage.setItem('theme', dark ? 'dark' : 'light'); } catch { /* private mode */ }
    const icon = document.getElementById('theme-icon');
    if (icon) icon.textContent = dark ? '☀️' : '🌙';
  };
  signOut.onclick = () => { teardown(); onSignOut(); };

  graph.onChange(() => renderTabs());

  // A close guard, because losing an afternoon of wiring to a stray Cmd-W is
  // not a mistake anyone should be able to make once.
  const beforeUnload = (e: BeforeUnloadEvent) => {
    if (graph.dirty) e.preventDefault();
  };
  window.addEventListener('beforeunload', beforeUnload);

  async function load(): Promise<void> {
    const doc = await api.flows();
    graph.load(doc.flows as FlowEntry[], doc.rev);
    renderTabs();
    canvas.render();
    canvas.fit();
    for (const w of doc.warnings ?? []) {
      appendEvent({ topic: 'error', data: { error: w }, at: new Date().toISOString() });
    }
  }

  let statsTimer: number | undefined;
  let disconnect: (() => void) | null = null;

  function teardown(): void {
    window.removeEventListener('beforeunload', beforeUnload);
    if (statsTimer) window.clearInterval(statsTimer);
    disconnect?.();
  }

  renderPalette();
  renderSidebar();
  renderTabs();

  void load().catch((ex) => {
    if (ex instanceof ApiError && ex.status === 401) onSessionLost();
    else alert(`Could not load flows: ${ex}`);
  });

  const refresh = async () => {
    try {
      const { nodes } = await api.stats();
      updateStats(nodes);
    } catch (ex) {
      if (ex instanceof ApiError && ex.status === 401) {
        teardown();
        onSessionLost();
      }
    }
  };
  void refresh();
  statsTimer = window.setInterval(refresh, 2000);

  disconnect = api.connectEvents(appendEvent, (up) => {
    connDot.className = `dot ${up ? 'green' : 'red'}`;
    connText.textContent = up ? 'live' : 'reconnecting';
  });

  return { destroy: teardown };
}

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
