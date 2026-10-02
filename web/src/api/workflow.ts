import { APIError, type Client, type WorkflowInteraction, type WorkflowNode, type WorkflowSnapshot } from "./client";
import type { SyncStatus } from "./session";

const object = (v: unknown): v is Record<string, unknown> => typeof v === "object" && v !== null && !Array.isArray(v);
const text = (v: unknown): v is string => typeof v === "string";
const invalid = () => new APIError(0, "invalid_argument");

/** Decode only the canonical workflow cursor, retaining uint64 precision. */
export function workflowCursor(cursor: unknown, rid: string): string {
  if (!text(cursor) || cursor.length === 0 || cursor.length > 512 || !/^[A-Za-z0-9_-]+$/.test(cursor)) throw invalid();
  try {
    const bytes = Uint8Array.from(atob(cursor.replaceAll("-", "+").replaceAll("_", "/")), (c) => c.charCodeAt(0));
    const value: unknown = JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(bytes));
    if (!Array.isArray(value) || value.length !== 4 || value[0] !== 2 || value[1] !== "workflow" || value[2] !== rid
      || !text(value[3]) || !/^(0|[1-9]\d*)$/.test(value[3]) || BigInt(value[3]) > 18446744073709551615n) throw invalid();
    const canonical = btoa(String.fromCharCode(...new TextEncoder().encode(JSON.stringify(value)))).replaceAll("+", "-").replaceAll("/", "_").replace(/=+$/, "");
    if (canonical !== cursor) throw invalid();
    return value[3];
  } catch { throw invalid(); }
}

/** Select the public fields and refuse identities/revisions unsafe for controls. */
export function workflowSnapshotDTO(raw: unknown, rid: string): WorkflowSnapshot {
  if (!object(raw) || raw.runId !== rid || raw.sessionId || raw.traceId || !Number.isSafeInteger(raw.revision) || (raw.revision as number) < 0
    || ![raw.definitionName, raw.definitionVersion, raw.state, raw.cursor, raw.durableSeq, raw.instanceId].every(text)
    || !/^(0|[1-9]\d*)$/.test(raw.durableSeq as string) || typeof raw.executionStopped !== "boolean" || typeof raw.canResume !== "boolean"
    || !Array.isArray(raw.workflowNodes) || !Array.isArray(raw.interactions)) throw invalid();
  if (workflowCursor(raw.cursor, rid) !== raw.durableSeq) throw invalid();
  const workflowNodes: WorkflowNode[] = raw.workflowNodes.map((n: unknown) => {
    if (!object(n) || ![n.nodeExecutionId, n.nodeId, n.kind, n.state].every(text)) throw invalid();
    return { nodeExecutionId: n.nodeExecutionId as string, nodeId: n.nodeId as string, kind: n.kind as string, state: n.state as string };
  });
  const interactions: WorkflowInteraction[] = raw.interactions.flatMap((i: unknown) => {
    if (!object(i) || ![i.interactionId, i.nodeExecutionId, i.question, i.instanceId].every(text) || !Array.isArray(i.options) || !i.options.every(text)) throw invalid();
    // A reopened writer must not display or approve the previous instance's cards.
    if (i.instanceId !== raw.instanceId) return [];
    return [{ interactionId: i.interactionId as string, nodeExecutionId: i.nodeExecutionId as string, question: i.question as string, instanceId: i.instanceId as string, options: i.options as string[] }];
  });
  return {
    runId: rid, definitionName: raw.definitionName as string, definitionVersion: raw.definitionVersion as string, state: raw.state as string,
    revision: raw.revision as number, cursor: raw.cursor as string, durableSeq: raw.durableSeq as string, instanceId: raw.instanceId as string,
    executionStopped: raw.executionStopped, canResume: raw.canResume, workflowNodes, interactions,
    ...(text(raw.errorCode) ? { errorCode: raw.errorCode } : {}), ...(text(raw.failedNode) ? { failedNode: raw.failedNode } : {}),
    ...(Object.hasOwn(raw, "result") ? { result: raw.result } : {}),
  };
}

const FACTS = new Set([
  "workflow.input.accepted", "workflow.state_changed", "workflow.node.state_changed",
  "interaction.requested", "interaction.resolved", "approval.asked", "approval.decided",
  "tool.requested", "tool.started", "tool.finished", "tool.state_changed", "tool.output.delta", "tool.progress",
  "message.snapshot", "message.finalized",
]);
function belongsToRun(raw: unknown, rid: string) {
  if (!object(raw) || !text(raw.type) || !FACTS.has(raw.type) || raw.sessionId || raw.traceId || raw.turnId) return false;
  if (raw.runId !== undefined && raw.runId !== rid) return false;
  if (!object(raw.scope)) return raw.runId === rid;
  const scope = raw.scope;
  if (scope.sessionId || scope.traceId || scope.turnId || (scope.runId !== undefined && scope.runId !== rid) || (scope.workflowRunId !== undefined && scope.workflowRunId !== rid)) return false;
  return scope.runId === rid || scope.workflowRunId === rid;
}

const RETRY_MS = [500, 1000, 2000, 5000];
/** Queries/observes one run only. Product facts invalidate its DTO, never a Code surface. */
export async function syncWorkflow(client: Client, rid: string, signal: AbortSignal, onSnapshot: (s: WorkflowSnapshot, request: number) => void, onStatus: (s: SyncStatus, err?: unknown) => void, beginSnapshot: () => number = () => 0) {
  let attempt = 0;
  let revision = -1;
  while (!signal.aborted) {
    onStatus(attempt === 0 ? "loading" : "reconnecting");
    const ctrl = new AbortController();
    const abort = () => ctrl.abort();
    signal.addEventListener("abort", abort, { once: true });
    let live = false;
    let ended = false;
    let resync = false;
    let failure: unknown;
    let dirty = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let refreshing: Promise<void> | undefined;
    const apply = (raw: unknown, request: number) => {
      if (ctrl.signal.aborted || signal.aborted) return null;
      const snap = workflowSnapshotDTO(raw, rid);
      if (snap.revision >= revision) { revision = snap.revision; onSnapshot(snap, request); }
      return snap;
    };
    const refresh = (): Promise<void> => {
      if (refreshing) return refreshing;
      if (!dirty || ctrl.signal.aborted) return Promise.resolve();
      dirty = false;
      const request = beginSnapshot();
      refreshing = client.workflowSnapshot(rid, ctrl.signal).then((raw) => { apply(raw, request); }, (err) => { throw err; }).catch((err: unknown) => {
        if (!ctrl.signal.aborted) { failure = err; ctrl.abort(); }
      }).finally(() => { refreshing = undefined; if (dirty && !ctrl.signal.aborted) schedule(); });
      return refreshing;
    };
    const schedule = () => {
      if (timer !== undefined || refreshing || ctrl.signal.aborted) return;
      timer = setTimeout(() => { timer = undefined; void refresh(); }, 100);
    };
    try {
      const request = beginSnapshot();
      const snap = apply(await client.workflowSnapshot(rid, ctrl.signal), request);
      if (!snap) return;
      const start = BigInt(workflowCursor(snap.cursor, rid));
      let position = start;
      await client.workflowEvents(rid, snap.cursor, (frame) => {
        if (ctrl.signal.aborted || signal.aborted) return;
        if (frame.event === "ready") {
          try {
            const ready: unknown = JSON.parse(frame.data);
            if (!object(ready) || typeof ready.live !== "boolean" || BigInt(workflowCursor(ready.handoff, rid)) < start) throw invalid();
            live = ready.live;
            onStatus(live ? "live" : "loading");
            attempt = 0;
          } catch { live = false; failure = invalid(); ctrl.abort(); }
        } else if (frame.event === "event") {
          let raw: unknown;
          try { raw = JSON.parse(frame.data); } catch { return; }
          if (!belongsToRun(raw, rid) || !object(raw)) return;
          if (raw.cursor !== undefined && raw.cursor !== frame.id) return;
          if (frame.id !== undefined) {
            try {
              const next = BigInt(workflowCursor(frame.id, rid));
              if (next <= position) return;
              position = next;
            } catch { return; }
          }
          dirty = true; schedule();
        } else if (frame.event === "end") ended = true;
        else if (frame.event === "resync") { resync = true; ctrl.abort(); }
      }, ctrl.signal);
      // EOF/end can arrive while a refresh is in flight. Drain its one dirty
      // flag before reporting historical observation complete, rather than
      // cancelling the scheduled refresh and leaving the last facts unseen.
      while (!ctrl.signal.aborted && (dirty || refreshing)) {
        clearTimeout(timer); timer = undefined;
        await refresh();
      }
    } catch (err) { if (!signal.aborted && !resync) failure ??= err; }
    finally {
      clearTimeout(timer);
      ctrl.abort();
      signal.removeEventListener("abort", abort);
    }
    if (signal.aborted) return;
    if (failure instanceof APIError && ((failure.status >= 400 && failure.status < 500 && failure.status !== 410) || failure.code === "invalid_argument")) {
      onStatus("error", failure);
      return;
    }
    if (ended || (!live && !resync && !failure)) { onStatus("ended"); return; }
    onStatus("reconnecting", failure);
    await wait(resync || (failure instanceof APIError && failure.status === 410) ? 0 : RETRY_MS[Math.min(attempt, RETRY_MS.length - 1)], signal);
    attempt++;
  }
}

export function wait(ms: number, signal: AbortSignal) {
  return new Promise<void>((resolve) => {
    if (signal.aborted) { resolve(); return; }
    const done = () => { clearTimeout(timer); signal.removeEventListener("abort", done); resolve(); };
    const timer = setTimeout(done, ms);
    signal.addEventListener("abort", done, { once: true });
  });
}
