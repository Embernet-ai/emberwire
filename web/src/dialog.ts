// Edit dialogs, rendered from a node's descriptor.
//
// This is the file that replaces Node-RED's per-node HTML. There, every node
// type ships a hand-written .html twin containing its edit form, its help text
// and a registerType call — so adding a property means editing markup in a
// second language and keeping two files in step. Here a node declares typed
// properties in Go, the admin API serves them as JSON, and this renders a form
// from that. One definition, one place, and a new node type gets a working
// dialog for free.

import type { Descriptor, PropDef } from './api';
import type { FlowEntry, Graph } from './graph';

export interface DialogResult {
  props: Record<string, unknown>;
}

/** Opens the edit dialog for a node. Resolves with the edits, or null if cancelled. */
export function editNode(
  graph: Graph,
  entry: FlowEntry,
  descriptor: Descriptor | undefined,
): Promise<DialogResult | null> {
  return new Promise((resolve) => {
    const backdrop = document.createElement('div');
    backdrop.className = 'dialog-backdrop';

    const dialog = document.createElement('div');
    dialog.className = 'dialog';
    dialog.setAttribute('role', 'dialog');
    dialog.setAttribute('aria-modal', 'true');

    // Header
    const header = document.createElement('div');
    header.className = 'dialog-head';
    const title = document.createElement('h2');
    title.textContent = descriptor?.paletteLabel ?? entry.type;
    header.append(title);

    if (descriptor) {
      const badge = document.createElement('span');
      const level = descriptor.compatibility.level;
      badge.className = `badge ${level === 'emberwire-only' ? 'only' : level}`;
      badge.textContent = level;
      if (descriptor.compatibility.notes) badge.title = descriptor.compatibility.notes;
      header.append(badge);
    }
    dialog.append(header);

    // A node type the runtime does not have is shown rather than hidden: its
    // stored properties are still editable as raw JSON so a flow can be
    // repaired instead of silently losing them.
    if (!descriptor) {
      const warn = document.createElement('div');
      warn.className = 'banner';
      warn.textContent =
        `${entry.type} is not installed in this runtime. Its configuration is shown as raw ` +
        `JSON and will be preserved exactly as it is.`;
      dialog.append(warn);
    }

    const body = document.createElement('div');
    body.className = 'dialog-body';
    dialog.append(body);

    const controls: Control[] = [];

    // Every node has a name, whether or not its descriptor lists one.
    if (!descriptor?.props?.some((p) => p.name === 'name')) {
      controls.push(renderProp(body, { name: 'name', kind: 'string', label: 'Name' }, entry, graph));
    }
    for (const p of descriptor?.props ?? []) {
      controls.push(renderProp(body, p, entry, graph));
    }
    if (!descriptor) {
      controls.push(renderRawJSON(body, entry));
    }

    // Help
    if (descriptor?.help) {
      const help = document.createElement('details');
      help.className = 'dialog-help';
      const summary = document.createElement('summary');
      summary.textContent = 'About this node';
      const text = document.createElement('p');
      text.textContent = descriptor.help;
      help.append(summary, text);
      if (descriptor.compatibility.notes) {
        const compat = document.createElement('p');
        compat.className = 'muted';
        compat.textContent = descriptor.compatibility.notes;
        help.append(compat);
      }
      dialog.append(help);
    }

    // Footer
    const footer = document.createElement('div');
    footer.className = 'dialog-foot';
    const err = document.createElement('div');
    err.className = 'dialog-error';
    const cancel = document.createElement('button');
    cancel.className = 'ghost';
    cancel.textContent = 'Cancel';
    const save = document.createElement('button');
    save.textContent = 'Done';
    footer.append(err, cancel, save);
    dialog.append(footer);

    const close = (result: DialogResult | null) => {
      document.removeEventListener('keydown', onKey);
      backdrop.remove();
      resolve(result);
    };

    const commit = () => {
      const props: Record<string, unknown> = {};
      const problems: string[] = [];
      for (const c of controls) {
        const outcome = c.read();
        if (outcome.error) problems.push(outcome.error);
        else Object.assign(props, outcome.values);
      }
      if (problems.length > 0) {
        err.textContent = problems[0]!;
        return;
      }
      close({ props });
    };

    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        e.preventDefault();
        close(null);
      }
      // Ctrl+Enter saves. Plain Enter would submit while somebody is typing a
      // multi-line function body.
      if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) {
        e.preventDefault();
        commit();
      }
    };

    cancel.onclick = () => close(null);
    save.onclick = commit;
    backdrop.onclick = (e) => { if (e.target === backdrop) close(null); };
    document.addEventListener('keydown', onKey);

    backdrop.append(dialog);
    document.body.append(backdrop);

    const first = dialog.querySelector<HTMLElement>('input, textarea, select');
    first?.focus();
  });
}

interface Control {
  read(): { values?: Record<string, unknown>; error?: string };
}

function field(parent: HTMLElement, p: PropDef): HTMLElement {
  const wrap = document.createElement('div');
  wrap.className = 'field';
  const label = document.createElement('label');
  label.textContent = p.label ?? p.name;
  if (p.required) {
    const star = document.createElement('span');
    star.className = 'required';
    star.textContent = ' *';
    label.append(star);
  }
  wrap.append(label);
  parent.append(wrap);
  return wrap;
}

function helpText(wrap: HTMLElement, p: PropDef): void {
  if (!p.help) return;
  const h = document.createElement('div');
  h.className = 'field-help';
  h.textContent = p.help;
  wrap.append(h);
}

function renderProp(parent: HTMLElement, p: PropDef, entry: FlowEntry, graph: Graph): Control {
  switch (p.kind) {
    case 'bool':
      return renderBool(parent, p, entry);
    case 'select':
      return renderSelect(parent, p, entry);
    case 'number':
      return renderNumber(parent, p, entry);
    case 'text':
    case 'js':
    case 'json':
    case 'jsonata':
      return renderTextArea(parent, p, entry);
    case 'typedInput':
      return renderTypedInput(parent, p, entry);
    case 'configRef':
      return renderConfigRef(parent, p, entry, graph);
    case 'credential':
      return renderCredential(parent, p, entry);
    case 'list':
      return renderList(parent, p, entry);
    default:
      return renderString(parent, p, entry);
  }
}

function renderString(parent: HTMLElement, p: PropDef, entry: FlowEntry): Control {
  const wrap = field(parent, p);
  const input = document.createElement('input');
  input.type = 'text';
  input.value = String(entry[p.name] ?? p.default ?? '');
  if (p.placeholder) input.placeholder = p.placeholder;
  wrap.append(input);
  helpText(wrap, p);

  return {
    read: () => {
      const v = input.value;
      if (p.required && !v) return { error: `${p.label ?? p.name} is required` };
      return { values: { [p.name]: v } };
    },
  };
}

function renderNumber(parent: HTMLElement, p: PropDef, entry: FlowEntry): Control {
  const wrap = field(parent, p);
  const input = document.createElement('input');
  input.type = 'number';
  input.value = String(entry[p.name] ?? p.default ?? '');
  wrap.append(input);
  helpText(wrap, p);

  return {
    read: () => {
      if (input.value === '') {
        if (p.required) return { error: `${p.label ?? p.name} is required` };
        return { values: {} };
      }
      const n = Number(input.value);
      if (!Number.isFinite(n)) return { error: `${p.label ?? p.name} must be a number` };
      return { values: { [p.name]: n } };
    },
  };
}

function renderBool(parent: HTMLElement, p: PropDef, entry: FlowEntry): Control {
  const wrap = document.createElement('div');
  wrap.className = 'field field-inline';
  const input = document.createElement('input');
  input.type = 'checkbox';
  input.checked = Boolean(entry[p.name] ?? p.default ?? false);
  const label = document.createElement('label');
  label.append(input, document.createTextNode(' ' + (p.label ?? p.name)));
  wrap.append(label);
  helpText(wrap, p);
  parent.append(wrap);

  return { read: () => ({ values: { [p.name]: input.checked } }) };
}

function renderSelect(parent: HTMLElement, p: PropDef, entry: FlowEntry): Control {
  const wrap = field(parent, p);
  const select = document.createElement('select');
  const current = String(entry[p.name] ?? p.default ?? '');

  for (const opt of p.options ?? []) {
    const o = document.createElement('option');
    o.value = String(opt.value);
    o.textContent = opt.label;
    if (o.value === current) o.selected = true;
    select.append(o);
  }
  wrap.append(select);
  helpText(wrap, p);

  return {
    read: () => {
      const raw = select.value;
      // A select whose options are numbers must store numbers, or a node
      // reading it as a number silently falls back to its default.
      const match = (p.options ?? []).find((o) => String(o.value) === raw);
      return { values: { [p.name]: match ? match.value : raw } };
    },
  };
}

function renderTextArea(parent: HTMLElement, p: PropDef, entry: FlowEntry): Control {
  const wrap = field(parent, p);
  const ta = document.createElement('textarea');
  ta.className = `code lang-${p.kind}`;
  ta.spellcheck = false;
  ta.rows = p.kind === 'js' ? 12 : 5;
  ta.value = String(entry[p.name] ?? p.default ?? '');
  // Tab indents rather than moving focus, because a code field that ejects you
  // on Tab is unusable.
  ta.addEventListener('keydown', (e) => {
    if (e.key !== 'Tab') return;
    e.preventDefault();
    const start = ta.selectionStart;
    const end = ta.selectionEnd;
    ta.value = ta.value.slice(0, start) + '    ' + ta.value.slice(end);
    ta.selectionStart = ta.selectionEnd = start + 4;
  });
  wrap.append(ta);
  helpText(wrap, p);

  return {
    read: () => {
      const v = ta.value;
      if (p.kind === 'json' && v.trim() !== '') {
        try {
          JSON.parse(v);
        } catch (ex) {
          return { error: `${p.label ?? p.name} is not valid JSON: ${(ex as Error).message}` };
        }
      }
      if (p.required && !v.trim()) return { error: `${p.label ?? p.name} is required` };
      return { values: { [p.name]: v } };
    },
  };
}

/** The typedInput control: a value paired with a type selector. */
const TYPE_LABELS: Record<string, string> = {
  msg: 'msg.', flow: 'flow.', global: 'global.', str: 'string', num: 'number',
  bool: 'boolean', json: 'JSON', bin: 'buffer', re: 'regex', date: 'timestamp',
  jsonata: 'JSONata', env: 'env',
};

function renderTypedInput(parent: HTMLElement, p: PropDef, entry: FlowEntry): Control {
  const wrap = field(parent, p);
  const row = document.createElement('div');
  row.className = 'typed-input';

  const typeKey = p.typeProp ?? `${p.name}Type`;
  const types = p.typedInputTypes ?? ['msg', 'flow', 'global', 'str', 'num', 'bool', 'json', 'env'];
  const currentType = String(entry[typeKey] ?? types[0] ?? 'str');

  const select = document.createElement('select');
  for (const t of types) {
    const o = document.createElement('option');
    o.value = t;
    o.textContent = TYPE_LABELS[t] ?? t;
    if (t === currentType) o.selected = true;
    select.append(o);
  }

  const input = document.createElement('input');
  input.type = 'text';
  input.value = String(entry[p.name] ?? p.default ?? '');
  if (p.placeholder) input.placeholder = p.placeholder;

  // A timestamp takes no value, so the field is disabled rather than left
  // looking editable and being ignored.
  const sync = () => { input.disabled = select.value === 'date'; };
  select.addEventListener('change', sync);
  sync();

  row.append(select, input);
  wrap.append(row);
  helpText(wrap, p);

  return {
    read: () => {
      const t = select.value;
      if (p.required && t !== 'date' && !input.value) {
        return { error: `${p.label ?? p.name} is required` };
      }
      return { values: { [p.name]: input.value, [typeKey]: t } };
    },
  };
}

function renderConfigRef(parent: HTMLElement, p: PropDef, entry: FlowEntry, graph: Graph): Control {
  const wrap = field(parent, p);
  const select = document.createElement('select');

  const none = document.createElement('option');
  none.value = '';
  none.textContent = '— none —';
  select.append(none);

  const current = String(entry[p.name] ?? '');
  const candidates = graph.all().filter((e) => e.type === p.configType);
  for (const c of candidates) {
    const o = document.createElement('option');
    o.value = c.id;
    o.textContent = (c.name as string) || c.id;
    if (c.id === current) o.selected = true;
    select.append(o);
  }
  wrap.append(select);

  if (candidates.length === 0) {
    const h = document.createElement('div');
    h.className = 'field-help';
    h.textContent = `No ${p.configType} is configured yet. Add one from the palette first.`;
    wrap.append(h);
  }
  helpText(wrap, p);

  return {
    read: () => {
      if (p.required && !select.value) return { error: `${p.label ?? p.name} is required` };
      return { values: { [p.name]: select.value } };
    },
  };
}

/**
 * A credential field.
 *
 * A stored value is never sent back to the editor, so the field shows whether
 * one exists rather than its contents. Leaving it untouched sends the sentinel
 * the runtime understands as "keep what you have" — without which opening a
 * dialog and pressing Done would overwrite every password with a blank.
 */
const UNCHANGED = '__PWRD__';

function renderCredential(parent: HTMLElement, p: PropDef, entry: FlowEntry): Control {
  const wrap = field(parent, p);
  const creds = (entry.credentials as Record<string, unknown> | undefined) ?? {};
  const hasExisting = creds[p.name] !== undefined;

  const input = document.createElement('input');
  input.type = 'password';
  input.autocomplete = 'new-password';
  input.placeholder = hasExisting ? '•••••••• (unchanged)' : '';
  wrap.append(input);

  const h = document.createElement('div');
  h.className = 'field-help';
  h.textContent = hasExisting
    ? 'A value is stored. Leave blank to keep it, or type to replace it.'
    : 'Stored encrypted, never written to the flow file.';
  wrap.append(h);

  return {
    read: () => {
      const typed = input.value;
      const value = typed === '' && hasExisting ? UNCHANGED : typed;
      return { values: { credentials: { ...creds, [p.name]: value } } };
    },
  };
}

/** A repeatable group of sub-properties: Change rules, Switch rules, columns. */
function renderList(parent: HTMLElement, p: PropDef, entry: FlowEntry): Control {
  const wrap = field(parent, p);
  const rows = document.createElement('div');
  rows.className = 'list-rows';
  wrap.append(rows);

  const initial = Array.isArray(entry[p.name]) ? (entry[p.name] as Record<string, unknown>[]) : [];
  const rowControls: { el: HTMLElement; controls: Control[] }[] = [];

  const addRow = (values: Record<string, unknown>) => {
    const row = document.createElement('div');
    row.className = 'list-row';

    const fields = document.createElement('div');
    fields.className = 'list-row-fields';
    const controls: Control[] = [];
    // A row is a small entry of its own, so each sub-field reuses the same
    // renderer rather than having a second, simpler implementation that drifts.
    const pseudo = values as unknown as FlowEntry;
    for (const sub of p.fields ?? []) {
      controls.push(renderProp(fields, sub, pseudo, undefined as unknown as Graph));
    }

    const remove = document.createElement('button');
    remove.className = 'icon';
    remove.type = 'button';
    remove.title = 'Remove';
    remove.textContent = '✕';
    remove.onclick = () => {
      const i = rowControls.findIndex((r) => r.el === row);
      if (i >= 0) rowControls.splice(i, 1);
      row.remove();
    };

    row.append(fields, remove);
    rows.append(row);
    rowControls.push({ el: row, controls });
  };

  for (const v of initial) addRow({ ...v });

  const add = document.createElement('button');
  add.className = 'ghost';
  add.type = 'button';
  add.textContent = `Add ${p.label ?? p.name}`;
  add.onclick = () => addRow({});
  wrap.append(add);
  helpText(wrap, p);

  return {
    read: () => {
      const out: Record<string, unknown>[] = [];
      for (const r of rowControls) {
        const row: Record<string, unknown> = {};
        for (const c of r.controls) {
          const outcome = c.read();
          if (outcome.error) return { error: outcome.error };
          Object.assign(row, outcome.values);
        }
        out.push(row);
      }
      return { values: { [p.name]: out } };
    },
  };
}

/** Raw JSON editing, for a node type this runtime does not implement. */
function renderRawJSON(parent: HTMLElement, entry: FlowEntry): Control {
  const wrap = field(parent, { name: '__raw', kind: 'json', label: 'Configuration' });

  // The structural keys are not shown: editing them here would let somebody
  // change a node's id or its wires by hand and produce a flow file the runtime
  // refuses to load.
  const structural = new Set(['id', 'type', 'z', 'g', 'x', 'y', 'wires', 'd']);
  const editable: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(entry)) {
    if (!structural.has(k)) editable[k] = v;
  }

  const ta = document.createElement('textarea');
  ta.className = 'code lang-json';
  ta.spellcheck = false;
  ta.rows = 10;
  ta.value = JSON.stringify(editable, null, 2);
  wrap.append(ta);

  return {
    read: () => {
      try {
        const parsed = JSON.parse(ta.value) as Record<string, unknown>;
        for (const k of Object.keys(parsed)) {
          if (structural.has(k)) {
            return { error: `${k} is managed by the editor and cannot be set here` };
          }
        }
        return { values: parsed };
      } catch (ex) {
        return { error: `Configuration is not valid JSON: ${(ex as Error).message}` };
      }
    },
  };
}
