// The flow graph model.
//
// Holds the Node-RED v1 entries the editor is working on, plus the selection and
// undo history. Deliberately separate from rendering: the canvas reads this and
// draws, and every mutation goes through here so undo is a single mechanism
// rather than something each interaction has to remember to do.

import type { Descriptor } from './api';

/** A flow-file entry. Unknown properties are preserved verbatim. */
export interface FlowEntry {
  id: string;
  type: string;
  z?: string;
  g?: string;
  name?: string;
  x?: number;
  y?: number;
  wires?: string[][];
  d?: boolean;
  [key: string]: unknown;
}

export interface Point { x: number; y: number; }

/** Canvas geometry. Node-RED's proportions, because they are well judged. */
export const NODE_WIDTH = 130;
export const NODE_HEIGHT = 34;
export const PORT_RADIUS = 5;
export const GRID = 10;

/** Where an output port sits on a node. */
export function outputPort(node: FlowEntry, port: number, total: number): Point {
  const x = (node.x ?? 0) + NODE_WIDTH / 2;
  const y = (node.y ?? 0) + portOffset(port, total);
  return { x, y };
}

/** Where the input port sits. A node has at most one. */
export function inputPort(node: FlowEntry): Point {
  return { x: (node.x ?? 0) - NODE_WIDTH / 2, y: node.y ?? 0 };
}

/**
 * Vertical offset of one port when a node has several.
 *
 * Ports are spread evenly around the node's centre so a three-output Switch
 * reads symmetrically rather than hanging off the top edge.
 */
function portOffset(port: number, total: number): number {
  if (total <= 1) return 0;
  const spacing = Math.min(NODE_HEIGHT / total, 13);
  return (port - (total - 1) / 2) * spacing;
}

/** Snaps a coordinate to the grid. */
export function snap(v: number): number {
  return Math.round(v / GRID) * GRID;
}

/**
 * The wire path between two points.
 *
 * A cubic bézier whose control points push horizontally, so a wire leaves a
 * node's output travelling right and arrives at an input travelling right —
 * which is what makes a graph readable when nodes are stacked vertically. The
 * control offset grows with distance and is clamped, so a short backward wire
 * still bows out enough to be followed rather than collapsing into a straight
 * line through the node it came from.
 */
export function wirePath(from: Point, to: Point): string {
  const dx = to.x - from.x;
  const dy = to.y - from.y;
  const dist = Math.sqrt(dx * dx + dy * dy);

  let offset = Math.max(40, Math.min(dist * 0.5, 120));
  // Wiring backwards is normal — a loop, a retry path — and needs a wider bow
  // or the curve doubles back through both nodes.
  if (dx < 0) offset = Math.max(offset, Math.abs(dx) * 0.6 + 60);

  return `M ${from.x} ${from.y} C ${from.x + offset} ${from.y}, ${to.x - offset} ${to.y}, ${to.x} ${to.y}`;
}

/** One undoable change. */
interface Snapshot {
  entries: FlowEntry[];
  label: string;
}

export type ChangeListener = () => void;

/** The editor's working copy of a flow set. */
export class Graph {
  private entries: FlowEntry[] = [];
  private undoStack: Snapshot[] = [];
  private redoStack: Snapshot[] = [];
  private listeners: ChangeListener[] = [];

  /** The revision the entries were loaded at, for deploy conflict detection. */
  rev = '';
  /** Whether there are unsaved changes. */
  dirty = false;
  /** Selected entry ids. */
  selection = new Set<string>();
  /** The tab currently being edited. */
  activeTab = '';

  constructor(private readonly descriptors: Map<string, Descriptor>) {}

  onChange(fn: ChangeListener): void {
    this.listeners.push(fn);
  }

  private emit(): void {
    for (const fn of this.listeners) fn();
  }

  load(entries: FlowEntry[], rev: string): void {
    // Deep-copied on load so that editing never mutates what was fetched, which
    // is what lets a failed deploy be retried against the original.
    this.entries = structuredClone(entries);
    this.rev = rev;
    this.dirty = false;
    this.undoStack = [];
    this.redoStack = [];
    this.selection.clear();
    if (!this.activeTab || !this.tabs().some((t) => t.id === this.activeTab)) {
      this.activeTab = this.tabs()[0]?.id ?? '';
    }
    this.emit();
  }

  all(): FlowEntry[] { return this.entries; }

  tabs(): FlowEntry[] { return this.entries.filter((e) => e.type === 'tab'); }

  /** Nodes on a tab, excluding config nodes and structural entries. */
  nodesOn(tabId: string): FlowEntry[] {
    return this.entries.filter(
      (e) => e.z === tabId && e.type !== 'tab' && e.type !== 'subflow' && e.type !== 'group'
        && !this.isConfig(e),
    );
  }

  /**
   * Config nodes: referenced by id from other nodes rather than wired in.
   *
   * Detected structurally — no x, no y, no wires — which is the same rule the
   * runtime's parser applies, so the editor and the runtime never disagree
   * about what a node is.
   */
  isConfig(e: FlowEntry): boolean {
    if (e.type === 'tab' || e.type === 'subflow' || e.type === 'group') return false;
    const d = this.descriptors.get(e.type);
    if (d?.isConfig) return true;
    return e.x === undefined && e.y === undefined && e.wires === undefined;
  }

  byId(id: string): FlowEntry | undefined {
    return this.entries.find((e) => e.id === id);
  }

  descriptor(type: string): Descriptor | undefined {
    return this.descriptors.get(type);
  }

  /** How many outputs a node has, honouring a configurable count. */
  outputCount(e: FlowEntry): number {
    const d = this.descriptors.get(e.type);
    if (d?.outputsProp) {
      const v = e[d.outputsProp];
      const n = typeof v === 'number' ? v : Number(v);
      if (Number.isFinite(n) && n >= 0) return n;
    }
    if (e.wires) return e.wires.length;
    return d?.outputs ?? 0;
  }

  inputCount(e: FlowEntry): number {
    return this.descriptors.get(e.type)?.inputs ?? 0;
  }

  /** The label drawn on a node. */
  label(e: FlowEntry): string {
    if (e.name) return e.name;
    const d = this.descriptors.get(e.type);
    if (d?.labelProp) {
      const v = e[d.labelProp];
      if (typeof v === 'string' && v) return v;
    }
    return d?.paletteLabel ?? e.type;
  }

  // ── Mutation ───────────────────────────────────────────────────────────────

  /**
   * Records the current state before a change.
   *
   * Called explicitly at the start of an interaction rather than after each
   * mutation, so dragging twenty nodes is one undo step instead of twenty.
   */
  checkpoint(label: string): void {
    this.undoStack.push({ entries: structuredClone(this.entries), label });
    // Bounded: an editor left open for a day should not hold every state it
    // ever passed through.
    if (this.undoStack.length > 100) this.undoStack.shift();
    this.redoStack = [];
  }

  /** Marks the graph changed and notifies listeners. */
  touch(): void {
    this.dirty = true;
    this.emit();
  }

  undo(): boolean {
    const snap = this.undoStack.pop();
    if (!snap) return false;
    this.redoStack.push({ entries: structuredClone(this.entries), label: snap.label });
    this.entries = snap.entries;
    this.pruneSelection();
    this.dirty = true;
    this.emit();
    return true;
  }

  redo(): boolean {
    const snap = this.redoStack.pop();
    if (!snap) return false;
    this.undoStack.push({ entries: structuredClone(this.entries), label: snap.label });
    this.entries = snap.entries;
    this.pruneSelection();
    this.dirty = true;
    this.emit();
    return true;
  }

  canUndo(): boolean { return this.undoStack.length > 0; }
  canRedo(): boolean { return this.redoStack.length > 0; }

  private pruneSelection(): void {
    for (const id of [...this.selection]) {
      if (!this.byId(id)) this.selection.delete(id);
    }
  }

  addNode(type: string, at: Point): FlowEntry {
    this.checkpoint('add node');
    const d = this.descriptors.get(type);

    const entry: FlowEntry = {
      id: generateId(),
      type,
      z: this.activeTab,
      x: snap(at.x),
      y: snap(at.y),
      wires: Array.from({ length: d?.outputs ?? 0 }, () => []),
    };

    // Seed declared defaults so a freshly dropped node behaves as its dialog
    // says it will, rather than only after somebody opens and saves it.
    for (const p of d?.props ?? []) {
      if (p.default !== undefined) entry[p.name] = p.default;
    }

    this.entries.push(entry);
    this.selection = new Set([entry.id]);
    this.touch();
    return entry;
  }

  deleteSelection(): void {
    if (this.selection.size === 0) return;
    this.checkpoint('delete');
    const doomed = new Set(this.selection);
    this.entries = this.entries.filter((e) => !doomed.has(e.id));

    // Drop wires pointing at anything deleted. Leaving them would produce a
    // flow file the runtime warns about on every load.
    for (const e of this.entries) {
      if (!e.wires) continue;
      e.wires = e.wires.map((port) => port.filter((id) => !doomed.has(id)));
    }
    this.selection.clear();
    this.touch();
  }

  moveSelection(dx: number, dy: number): void {
    for (const id of this.selection) {
      const e = this.byId(id);
      if (!e || e.x === undefined || e.y === undefined) continue;
      e.x += dx;
      e.y += dy;
    }
    this.touch();
  }

  snapSelection(): void {
    for (const id of this.selection) {
      const e = this.byId(id);
      if (!e || e.x === undefined || e.y === undefined) continue;
      e.x = snap(e.x);
      e.y = snap(e.y);
    }
    this.touch();
  }

  /** Connects an output port to a node's input. */
  connect(fromId: string, port: number, toId: string): boolean {
    const from = this.byId(fromId);
    const to = this.byId(toId);
    if (!from || !to || fromId === toId) return false;
    if (this.inputCount(to) === 0) return false;

    this.checkpoint('connect');
    if (!from.wires) from.wires = [];
    while (from.wires.length <= port) from.wires.push([]);

    const existing = from.wires[port]!;
    // A duplicate wire would deliver the message twice, which is almost never
    // what somebody dragging a second time intended.
    if (existing.includes(toId)) return false;
    existing.push(toId);
    this.touch();
    return true;
  }

  disconnect(fromId: string, port: number, toId: string): void {
    const from = this.byId(fromId);
    if (!from?.wires?.[port]) return;
    this.checkpoint('disconnect');
    from.wires[port] = from.wires[port]!.filter((id) => id !== toId);
    this.touch();
  }

  /** Applies edited properties to a node. */
  updateNode(id: string, props: Record<string, unknown>): void {
    const e = this.byId(id);
    if (!e) return;
    this.checkpoint('edit node');
    Object.assign(e, props);

    // Keep the wires array in step with a changed output count, or the node
    // renders ports it cannot send on.
    const outputs = this.outputCount(e);
    if (!e.wires) e.wires = [];
    while (e.wires.length < outputs) e.wires.push([]);
    if (e.wires.length > outputs) e.wires = e.wires.slice(0, outputs);

    this.touch();
  }

  addTab(label: string): FlowEntry {
    this.checkpoint('add flow');
    const tab: FlowEntry = { id: generateId(), type: 'tab', label, disabled: false, info: '' };
    this.entries.push(tab);
    this.activeTab = tab.id;
    this.touch();
    return tab;
  }

  /** Every wire on a tab, as drawable segments. */
  wiresOn(tabId: string): { from: FlowEntry; port: number; to: FlowEntry }[] {
    const out: { from: FlowEntry; port: number; to: FlowEntry }[] = [];
    for (const e of this.nodesOn(tabId)) {
      e.wires?.forEach((targets, port) => {
        for (const id of targets) {
          const to = this.byId(id);
          // A wire to a node on another tab is legal in the file but not
          // drawable here; the runtime reports it, the canvas skips it.
          if (to && to.z === tabId) out.push({ from: e, port, to });
        }
      });
    }
    return out;
  }
}

/**
 * A flow-file id: sixteen hex characters, matching what the runtime generates.
 *
 * crypto.getRandomValues rather than Math.random — ids end up in a file that is
 * merged and diffed, and a collision produces a duplicate-id parse failure that
 * is very hard to explain.
 */
export function generateId(): string {
  const bytes = new Uint8Array(8);
  crypto.getRandomValues(bytes);
  return Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('');
}
