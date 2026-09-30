import { parseMessage, type Component } from "./protocol";

export type SurfaceState = {
  surfaceId: string;
  root: string;
  components: Map<string, Component>;
  data: Map<string, string>;
  /** Durable cursor of the last fully applied frame group. */
  cursor: string;
  /** Instance that produced the current approvals; a change drops them. */
  instanceId: string;
};

/**
 * A2UIStore holds one surface. Components and data keys are overwritten by ID,
 * so replaying frames after a reconnect is idempotent. The cursor advances only
 * on frames that carry an SSE id (the last frame of a durable group). A store
 * is bound to one session "generation"; frames for an older binding are ignored.
 */
export class A2UIStore {
  private state: SurfaceState;
  private binding = 0;
  private listeners = new Set<() => void>();
  private version = 0;

  constructor(surfaceId = "") {
    this.state = empty(surfaceId);
  }

  subscribe = (fn: () => void) => {
    this.listeners.add(fn);
    return () => this.listeners.delete(fn);
  };

  getVersion = () => this.version;
  get snapshot(): SurfaceState {
    return this.state;
  }

  /** reset starts a new binding (session switch or re-render); returns its token. */
  reset(surfaceId: string, cursor = ""): number {
    this.binding++;
    this.state = empty(surfaceId);
    this.state.cursor = cursor;
    this.emit();
    return this.binding;
  }

  setCursor(token: number, cursor: string) {
    if (token !== this.binding) return;
    this.state.cursor = cursor;
  }

  /** apply one frame for binding token; returns false for stale or invalid frames. */
  apply(token: number, text: string, id?: string): boolean {
    if (token !== this.binding) return false;
    const msg = parseMessage(text);
    if (!msg) return false;
    const s = this.state;
    if ("beginRendering" in msg) {
      if (s.surfaceId && msg.beginRendering.surfaceId !== s.surfaceId) return false;
      s.surfaceId = msg.beginRendering.surfaceId;
      s.root = msg.beginRendering.root;
    } else if ("surfaceUpdate" in msg) {
      if (msg.surfaceUpdate.surfaceId !== s.surfaceId) return false;
      for (const c of msg.surfaceUpdate.components) {
        if ("Approval" in c.component) {
          const inst = c.component.Approval.instanceId;
          if (s.instanceId && inst !== s.instanceId) this.dropApprovals();
          s.instanceId = inst;
        }
        s.components.set(c.id, c);
      }
    } else if ("dataModelUpdate" in msg) {
      if (msg.dataModelUpdate.surfaceId !== s.surfaceId) return false;
      for (const c of msg.dataModelUpdate.contents) s.data.set(c.key, c.valueString);
    } else {
      if (msg.deleteSurface.surfaceId !== s.surfaceId) return false;
      s.components.clear();
      s.data.clear();
      s.root = "";
    }
    if (id) s.cursor = id;
    this.emit();
    return true;
  }

  private dropApprovals() {
    for (const [id, c] of this.state.components) if ("Approval" in c.component) this.state.components.delete(id);
  }

  private emit() {
    this.version++;
    for (const fn of this.listeners) fn();
  }
}

function empty(surfaceId: string): SurfaceState {
  return { surfaceId, root: "", components: new Map(), data: new Map(), cursor: "", instanceId: "" };
}
