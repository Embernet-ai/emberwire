// The admin API client.
//
// Deliberately dependency-free. The dashboard's proxy enforces a CSP that blocks
// every external host, and an edge box has no internet, so anything not bundled
// simply would not load.

/**
 * A node type, as served by GET /nodes.
 *
 * This mirrors node.Descriptor in Go exactly. It is the entire contract between
 * the runtime and the editor: there is no per-node HTML anywhere, so a node type
 * that declares itself correctly here gets a working edit dialog with no
 * front-end change at all.
 */
export interface Descriptor {
  type: string;
  category: string;
  color: string;
  icon: string;
  inputs: number;
  outputs: number;
  /** Names a property that determines the output count, e.g. a Switch node's rules. */
  outputsProp?: string;
  /** Names the property used as the node's canvas label. */
  labelProp?: string;
  paletteLabel?: string;
  align?: string;
  isConfig?: boolean;
  hasButton?: boolean;
  inputLabels?: string[];
  outputLabels?: string[];
  props?: PropDef[];
  help?: string;
  compatibility: { level: string; notes?: string; unsupportedProps?: string[] };
}

export interface PropDef {
  name: string;
  label?: string;
  kind: string;
  default?: unknown;
  required?: boolean;
  placeholder?: string;
  help?: string;
  options?: { value: unknown; label: string }[];
  /** Narrows the type selector for a typedInput. */
  typedInputTypes?: string[];
  /**
   * Names the companion property holding the selected type for a typedInput.
   * Node-RED spells these inconsistently — pt, tot, vt — so it is explicit.
   */
  typeProp?: string;
  fields?: PropDef[];
  configType?: string;
  language?: string;
}

export interface NodeStat {
  nodeId: string;
  type: string;
  received: number;
  sent: number;
  errors: number;
  dropped: number;
  blocked: number;
  queueLen: number;
  queueCap: number;
  queueHigh: number;
}

export interface Settings {
  version: string;
  adminRoot: string;
  runtime: { inboxCapacity: number; overflow: string };
  discovery: { enabled: boolean };
  metrics: { enabled: boolean; path: string };
}

export interface RuntimeEvent {
  topic: string;
  data: Record<string, unknown>;
  at: string;
}

const TOKEN_KEY = 'emberwire.token';

export class ApiError extends Error {
  constructor(readonly status: number, message: string) {
    super(message);
  }
}

export class Api {
  private token: string | null = null;

  constructor(private readonly base: string = '') {
    try {
      this.token = sessionStorage.getItem(TOKEN_KEY);
    } catch {
      // Private mode. The session simply will not survive a reload.
      this.token = null;
    }
  }

  get authenticated(): boolean {
    return this.token !== null;
  }

  /**
   * Session storage rather than local storage: a token is a bearer credential
   * for something that can run commands on a plant floor, and it should not
   * outlive the tab it was issued to.
   */
  private setToken(token: string | null): void {
    this.token = token;
    try {
      if (token) sessionStorage.setItem(TOKEN_KEY, token);
      else sessionStorage.removeItem(TOKEN_KEY);
    } catch {
      /* ignore */
    }
  }

  async login(username: string, password: string): Promise<void> {
    const res = await fetch(`${this.base}/auth/token`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ username, password }),
    });
    if (!res.ok) {
      const body = await res.json().catch(() => ({ error: res.statusText }));
      throw new ApiError(res.status, body.error ?? 'login failed');
    }
    const body = await res.json();
    this.setToken(body.access_token);
  }

  logout(): void {
    // Best effort: the token is dropped locally regardless of whether the
    // server round-trip succeeds.
    if (this.token) {
      void fetch(`${this.base}/auth/revoke`, {
        method: 'POST',
        headers: { Authorization: `Bearer ${this.token}` },
      }).catch(() => undefined);
    }
    this.setToken(null);
  }

  private async get<T>(path: string): Promise<T> {
    const res = await fetch(this.base + path, {
      headers: this.token ? { Authorization: `Bearer ${this.token}` } : {},
    });
    if (res.status === 401) {
      // Expired or revoked. Drop it so the UI falls back to the login screen
      // rather than looping on failed requests.
      this.setToken(null);
      throw new ApiError(401, 'session expired');
    }
    if (!res.ok) {
      const body = await res.json().catch(() => ({ error: res.statusText }));
      throw new ApiError(res.status, body.error ?? res.statusText);
    }
    return res.json() as Promise<T>;
  }

  settings(): Promise<Settings> {
    return this.get<Settings>('/settings');
  }

  nodes(): Promise<Descriptor[]> {
    return this.get<Descriptor[]>('/nodes');
  }

  stats(): Promise<{ nodes: NodeStat[] }> {
    return this.get<{ nodes: NodeStat[] }>('/runtime/stats');
  }

  flows(): Promise<{ rev: string; flows: unknown[]; warnings?: string[] }> {
    return this.get('/flows');
  }

  /**
   * Deploys a flow set.
   *
   * The revision travels in a header rather than the body so the payload stays
   * a plain v1 array — exactly what the runtime persists and what an operator
   * can paste into a file. Passing an empty rev forces the write, which is the
   * "overwrite theirs" branch of a conflict.
   */
  async deploy(
    flows: unknown[],
    rev: string,
  ): Promise<{ rev: string; warnings?: string[]; failures?: { id: string; type: string; error: string }[] }> {
    const res = await fetch(`${this.base}/flows`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'Emberwire-Deployment-Rev': rev,
        ...(this.token ? { Authorization: `Bearer ${this.token}` } : {}),
      },
      body: JSON.stringify(flows),
    });
    if (res.status === 401) {
      this.setToken(null);
      throw new ApiError(401, 'session expired');
    }
    if (!res.ok) {
      const body = await res.json().catch(() => ({ error: res.statusText }));
      throw new ApiError(res.status, body.error ?? res.statusText);
    }
    return res.json();
  }

  /**
   * Opens the event stream.
   *
   * The token goes in the query string because a browser cannot set headers on
   * a WebSocket handshake. It is bounded by the session and the connection is
   * same-origin, so it does not leave the origin it was issued for.
   */
  connectEvents(onEvent: (e: RuntimeEvent) => void, onState: (up: boolean) => void): () => void {
    let socket: WebSocket | null = null;
    let closed = false;
    let retry = 1000;
    let timer: number | undefined;

    const open = () => {
      if (closed || !this.token) return;
      const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
      const url = `${proto}//${location.host}${this.base}/comms?access_token=${encodeURIComponent(this.token)}`;

      socket = new WebSocket(url);

      socket.onopen = () => {
        retry = 1000;
        onState(true);
      };

      socket.onmessage = (ev) => {
        try {
          // The server batches up to 64 events per frame, so this is always an
          // array — one frame per event would lock the browser under load.
          const batch = JSON.parse(ev.data as string) as RuntimeEvent[];
          for (const e of batch) onEvent(e);
        } catch {
          /* a malformed frame must not kill the stream */
        }
      };

      socket.onclose = () => {
        onState(false);
        if (closed) return;
        // Capped exponential backoff. An edge link drops constantly and a tight
        // reconnect loop would hammer the runtime it is trying to observe.
        timer = window.setTimeout(open, retry);
        retry = Math.min(retry * 2, 30000);
      };

      socket.onerror = () => socket?.close();
    };

    open();

    return () => {
      closed = true;
      if (timer) window.clearTimeout(timer);
      socket?.close();
    };
  }
}
