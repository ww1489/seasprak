import { useEffect, useMemo, useRef, useState, useSyncExternalStore } from "react";
import { APIError, errorText, newIdempotencyKey, type Client, type WorkflowDecision, type WorkflowInteraction, type WorkflowOperationReceipt, type WorkflowSnapshot } from "./api/client";
import type { SyncStatus } from "./api/session";
import { syncWorkflow, wait, workflowSnapshotDTO } from "./api/workflow";
import ApprovalCard from "./components/ApprovalCard";
import LoadingState from "./components/bui/LoadingState";
import { stateLabel } from "./components/bui/TaskRow";

const STATUS_TEXT: Record<SyncStatus, string> = { idle: "", loading: "同步中", live: "实时", ended: "只读浏览（无运行中的写入者）", reconnecting: "重新连接中", error: "同步失败" };
const CONTROL_TEXT = { pause: "暂停", cancel: "取消", resume: "恢复", approval: "审批" };
const TERMINAL = new Set(["completed", "failed", "cancelled"]);
const btn = "rounded-control px-2 py-0.5 text-[12px] shadow-btn hover:bg-hover disabled:opacity-50";
type ControlRequest =
  | { kind: "pause" | "cancel" | "resume"; body: { expectedRevision: number } }
  | { kind: "approval"; interactionId: string; body: { decision: WorkflowDecision; expectedRevision: number; instanceId: string } };
type PendingControl = { id: string; key: string; request: ControlRequest; busy: boolean };
type PendingControls = { entries: Map<string, PendingControl>; subscribe: (fn: () => void) => () => void; getVersion: () => number; changed: () => void };
// Separate from Code receipts. Only unconfirmed HTTP requests survive navigation,
// grouped by the Client and run ID; no business state or bearer is persisted.
const requests = new WeakMap<Client, Map<string, PendingControls>>();
function retainedControls(client: Client, rid: string) {
  let runs = requests.get(client);
  if (!runs) { runs = new Map(); requests.set(client, runs); }
  let controls = runs.get(rid);
  if (!controls) {
    let version = 0;
    const listeners = new Set<() => void>();
    controls = {
      entries: new Map(), subscribe: (fn) => { listeners.add(fn); return () => { listeners.delete(fn); }; }, getVersion: () => version,
      changed: () => { version++; for (const fn of listeners) fn(); },
    };
    runs.set(rid, controls);
  }
  return controls;
}

export default function WorkflowView(props: { client: Client; rid: string }) {
  const binding = useMemo(() => crypto.randomUUID(), [props.client]);
  return <WorkflowControls key={binding + ":" + props.rid} {...props} />;
}

function WorkflowControls({ client, rid }: { client: Client; rid: string }) {
  const [snap, setSnap] = useState<WorkflowSnapshot | null>(null);
  const [status, setStatus] = useState<SyncStatus>("idle");
  const [syncError, setSyncError] = useState("");
  const [actionError, setActionError] = useState("");
  const [opening, setOpening] = useState(false);
  const [epoch, setEpoch] = useState(0);
  const [operations, setOperations] = useState<Record<string, string>>({});
  const lifetime = useRef<AbortController | null>(null);
  const openRequest = useRef(false);
  // All snapshot sources share this view-local observation/request boundary.
  const observation = useRef(0);
  const snapshotRequest = useRef(0);
  const pending = useMemo(() => retainedControls(client, rid), [client, rid]);
  useSyncExternalStore(pending.subscribe, pending.getVersion);
  useEffect(() => {
    const ctrl = new AbortController();
    lifetime.current = ctrl;
    return () => ctrl.abort();
  }, []);
  const current = (signal: AbortSignal, generation: number) => !signal.aborted && generation === observation.current;
  const apply = (raw: unknown, signal: AbortSignal, generation: number, request: number) => {
    if (!current(signal, generation) || request !== snapshotRequest.current) return;
    const fresh = workflowSnapshotDTO(raw, rid);
    setSnap((old) => !old || fresh.revision >= old.revision ? fresh : old);
  };
  useEffect(() => {
    const ctrl = new AbortController();
    const generation = ++observation.current;
    void syncWorkflow(client, rid, ctrl.signal, (fresh, request) => apply(fresh, ctrl.signal, generation, request), (state, err) => {
      if (!current(ctrl.signal, generation)) return;
      setStatus(state); setSyncError(err ? errorText(err) : "");
    }, () => current(ctrl.signal, generation) ? ++snapshotRequest.current : -1);
    return () => ctrl.abort();
  }, [client, rid, epoch]);
  const writable = status === "live" && !opening;
  const restartObservation = () => {
    observation.current++; snapshotRequest.current++;
    setStatus("loading"); setEpoch((n) => n + 1);
  };
  const refresh = async (signal: AbortSignal, generation: number) => {
    if (!current(signal, generation)) return;
    const request = ++snapshotRequest.current;
    try { apply(await client.workflowSnapshot(rid, signal), signal, generation, request); }
    catch (err) { if (current(signal, generation) && request === snapshotRequest.current) setActionError(errorText(err)); }
  };
  const open = async () => {
    if (openRequest.current || writable || !lifetime.current) return;
    const signal = lifetime.current.signal;
    const generation = ++observation.current;
    const request = ++snapshotRequest.current;
    openRequest.current = true;
    setOpening(true); setActionError("");
    try {
      const fresh = await client.openWorkflowRun(rid, signal);
      if (!current(signal, generation)) return;
      apply(fresh, signal, generation, request); restartObservation();
    } catch (err) { if (current(signal, generation)) setActionError(errorText(err)); }
    finally { openRequest.current = false; if (!signal.aborted) setOpening(false); }
  };

  // A command receipt is not a run terminal state. Only queried snapshots may
  // say that execution stopped; polling never resumes or cancels another run.
  const watchOperation = async (receipt: WorkflowOperationReceipt, signal: AbortSignal, generation: number) => {
    await refresh(signal, generation);
    for (let n = 0; n < 120 && current(signal, generation); n++) {
      try {
        const op = await client.workflowOperation(rid, receipt.operationId, signal);
        if (!current(signal, generation)) return;
        if (op.operationId !== receipt.operationId || !Number.isSafeInteger(op.revision) || op.revision < 0) throw new APIError(0, "invalid_argument");
        setOperations((old) => ({ ...old, [op.operationId]: op.state }));
        if (TERMINAL.has(op.state)) {
          if (op.error) setActionError(errorText(new APIError(0, op.error)));
          await refresh(signal, generation); return;
        }
      } catch (err) { if (current(signal, generation)) setActionError(errorText(err)); return; }
      await wait(500, signal);
    }
  };
  const retry = async (entry: PendingControl) => {
    if (!writable || entry.busy || !lifetime.current || lifetime.current.signal.aborted) return;
    const signal = lifetime.current.signal;
    const generation = observation.current;
    entry.busy = true; pending.changed(); setActionError("");
    const r = entry.request;
    try {
      const receipt = r.kind === "approval" ? await client.respondWorkflowInteraction(rid, r.interactionId, r.body.decision, r.body.expectedRevision, r.body.instanceId, entry.key, signal)
        : r.kind === "pause" ? await client.pauseWorkflowRun(rid, r.body.expectedRevision, entry.key, signal)
        : r.kind === "cancel" ? await client.cancelWorkflowRun(rid, r.body.expectedRevision, entry.key, undefined, signal)
        : await client.resumeWorkflowRun(rid, r.body.expectedRevision, entry.key, signal);
      if (signal.aborted) return;
      if (!receipt || typeof receipt !== "object" || Array.isArray(receipt) || typeof receipt.operationId !== "string" || !receipt.operationId.trim()
        || receipt.state !== "accepted" || !Number.isSafeInteger(receipt.acceptedCommit) || receipt.acceptedCommit < 0
        || receipt.target !== (r.kind === "approval" ? r.interactionId : rid) || receipt.scope !== (r.kind === "approval" ? "instance" : "durable")
        || (r.kind === "approval" ? receipt.instanceId !== r.body.instanceId || receipt.acceptedCommit !== 0 : receipt.acceptedCommit === 0 || receipt.instanceId !== undefined)) throw new APIError(0, "resource_unavailable");
      pending.entries.delete(entry.id);
      setOperations((old) => ({ ...old, [receipt.operationId]: receipt.state }));
      void watchOperation(receipt, signal, generation);
    } catch (err) {
      // Refused requests can be replaced on a new explicit click. Lost/5xx/JSON
      // receipts remain frozen even when a fresh snapshot removes eligibility.
      if (!signal.aborted) {
        if (err instanceof APIError && err.status >= 400 && err.status < 500) pending.entries.delete(entry.id);
        setActionError(errorText(err));
      }
    } finally { entry.busy = false; pending.changed(); }
  };
  const control = (request: ControlRequest) => {
    if (!writable) return;
    const id = request.kind === "approval" ? "workflow:approval:" + request.interactionId : "workflow:" + request.kind;
    let entry = pending.entries.get(id);
    if (!entry) {
      Object.freeze(request.body);
      entry = { id, key: newIdempotencyKey(), request, busy: false };
      pending.entries.set(id, entry);
    }
    void retry(entry);
  };
  const decide = (i: WorkflowInteraction, decision: WorkflowDecision) => {
    if (!snap || i.instanceId !== snap.instanceId || !i.options.includes(decision) || pending.entries.has("workflow:approval:" + i.interactionId)) return;
    control({ kind: "approval", interactionId: i.interactionId, body: { decision, expectedRevision: snap.revision, instanceId: i.instanceId } });
  };

  return (
    <section aria-label="工作流运行" data-surface-id={"workflow:" + rid} className="mx-auto flex w-full max-w-3xl min-w-0 flex-col gap-4">
      <header className="flex min-w-0 flex-wrap items-center gap-3">
        <h2 className="min-w-0 truncate font-mono text-[13px] font-semibold">{rid}</h2>
        {status === "loading" || status === "reconnecting" ? <LoadingState label={STATUS_TEXT[status]} /> : <span className="text-[12px] text-ink-3">{STATUS_TEXT[status]}</span>}
        {!writable && <button type="button" disabled={opening || !["ended", "error"].includes(status)} onClick={() => void open()} className={btn}>打开工作流控制</button>}
        <button type="button" onClick={restartObservation} className={btn}>刷新工作流</button>
      </header>
      {syncError && <p role="alert" className="text-[12px] text-red">{syncError}</p>}
      {snap && <>
        <section aria-label="运行状态" className="flex min-w-0 flex-col gap-2 rounded-card bg-surface p-3 text-[12.5px] shadow-card">
          <h3 className="break-words font-medium">{snap.definitionName}</h3>
          <p className="break-words text-ink-2">版本 {snap.definitionVersion} · 修订 {snap.revision} · 持久位置 {snap.durableSeq}</p>
          <p>{snap.state === "pausing" ? "暂停中" : stateLabel(snap.state)}</p>
          <p className="text-ink-3">{snap.executionStopped ? "执行已停止" : "执行尚未停止"}</p>
          {snap.errorCode && <p role="alert" className="text-red">{errorText(new APIError(0, snap.errorCode))}</p>}
          {snap.failedNode && <p className="break-words text-ink-2">失败节点：{snap.failedNode}</p>}
          <div className="flex flex-wrap gap-2">
            <button type="button" disabled={!writable || snap.state !== "running" || snap.executionStopped || pending.entries.has("workflow:pause")} onClick={() => control({ kind: "pause", body: { expectedRevision: snap.revision } })} className={btn}>暂停工作流</button>
            <button type="button" disabled={!writable || TERMINAL.has(snap.state) || snap.state === "cancelling" || pending.entries.has("workflow:cancel")} onClick={() => control({ kind: "cancel", body: { expectedRevision: snap.revision } })} className={btn}>取消工作流</button>
            <button type="button" disabled={!writable || !snap.canResume || !snap.executionStopped || pending.entries.has("workflow:resume")} onClick={() => control({ kind: "resume", body: { expectedRevision: snap.revision } })} className={btn}>恢复工作流</button>
          </div>
        </section>
        <section aria-label="工作流节点" className="flex min-w-0 flex-col gap-2">
          <h3 className="text-[12.5px] font-medium">工作流节点</h3>
          <ul className="flex flex-col gap-2">
            {snap.workflowNodes.map((node) => <li key={node.nodeExecutionId} className="flex min-w-0 flex-wrap items-center gap-2 rounded-card bg-surface p-3 text-[12px] shadow-card">
              <span className="break-words font-mono">{node.nodeId}</span><span className="text-ink-2">{node.kind}</span><span>{stateLabel(node.state)}</span>
            </li>)}
            {snap.workflowNodes.length === 0 && <li className="text-[12px] text-ink-3">暂无节点执行记录</li>}
          </ul>
        </section>
        {snap.interactions.map((i) => {
          const busy = !writable || pending.entries.has("workflow:approval:" + i.interactionId);
          return <div key={i.instanceId + ":" + i.interactionId}>
            <ApprovalCard question={i.question} options={i.options} busy={busy} error="" onDecide={(decision) => decide(i, decision as WorkflowDecision)} />
            {i.options.includes("cancelled") && <button type="button" disabled={busy} onClick={() => decide(i, "cancelled")} className={btn}>取消审批</button>}
          </div>;
        })}
        {snap.result !== undefined && <section aria-label="工作流结果" className="rounded-card bg-surface p-3 shadow-card">
          <h3 className="text-[12.5px] font-medium">工作流结果</h3>
          <pre className="mt-2 whitespace-pre-wrap break-words text-[12px]">{typeof snap.result === "string" ? snap.result : JSON.stringify(snap.result, null, 2)}</pre>
        </section>}
      </>}
      {pending.entries.size > 0 && <section aria-label="未确认工作流请求" className="flex flex-col gap-2 rounded-card bg-surface p-3 text-[12px] shadow-card">
        <h3 className="font-medium">未确认工作流请求</h3>
        <p className="text-ink-2">重试保留原请求。放弃只清除页面记录，不撤销服务端可能已接纳的操作。</p>
        {[...pending.entries.values()].map((entry) => <div key={entry.id} className="flex min-w-0 flex-wrap items-center gap-2">
          <span>{CONTROL_TEXT[entry.request.kind]}：{entry.busy ? "等待回执" : "响应未确认"}</span>
          <button type="button" disabled={!writable || entry.busy} onClick={() => void retry(entry)} className={btn}>重试{CONTROL_TEXT[entry.request.kind]}</button>
          <button type="button" disabled={entry.busy} onClick={() => { pending.entries.delete(entry.id); pending.changed(); setActionError(""); }} className={btn}>放弃{CONTROL_TEXT[entry.request.kind]}</button>
        </div>)}
      </section>}
      {Object.entries(operations).length > 0 && <ul aria-label="工作流操作回执" className="text-[12px] text-ink-2">
        {Object.entries(operations).map(([id, state]) => <li key={id} className="break-words">操作 {id}：{stateLabel(state)}</li>)}
      </ul>}
      {actionError && <p role="alert" className="text-[12.5px] text-red">{actionError}</p>}
    </section>
  );
}
