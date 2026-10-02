import { useEffect, useMemo, useRef, useState, useSyncExternalStore } from "react";
import { A2UIStore } from "./a2ui/store";
import { Surface } from "./a2ui/renderer";
import type { ApprovalComp } from "./a2ui/protocol";
import {
  APIError,
  errorText,
  newIdempotencyKey,
  type Attachment,
  type Branch,
  type Capabilities,
  type Client,
  type ContentBlock,
  type ReconcileBody,
  type Snapshot,
} from "./api/client";
import { syncSession, type SyncStatus } from "./api/session";
import { Composer } from "./components/bui/Chat";
import LoadingState from "./components/bui/LoadingState";
import ApprovalCard from "./components/ApprovalCard";

const STATUS_TEXT: Record<SyncStatus, string> = {
  idle: "",
  loading: "同步中",
  live: "实时",
  ended: "只读浏览（无运行中的写入者）",
  reconnecting: "重新连接中",
  error: "同步失败",
};

// Media types the service stores (internal/storage/attachments.go allowlist).
const ATTACHMENT_TYPES = ["text/plain", "text/markdown", "application/json", "image/png", "image/jpeg"];

// The fork DTO accepts "summarize" only once the branch-summary work lands.
// Keep this false until internal/web/branches.go forkRequest has the field.
const FORK_SUMMARIZE_SUPPORTED = true;

/** keyed remembers one idempotency key per logical action until it succeeds. */
function useKeys() {
  const keys = useRef(new Map<string, string>());
  return {
    get(action: string) {
      let k = keys.current.get(action);
      if (!k) {
        k = newIdempotencyKey();
        keys.current.set(action, k);
      }
      return k;
    },
    done(action: string) {
      keys.current.delete(action);
    },
  };
}

type ControlRequest =
  | { kind: "resume"; traceId: string; body: { expectedRevision: number } }
  | { kind: "reconcile"; traceId: string; body: ReconcileBody & { expectedRevision: number } }
  | { kind: "approval"; interactionId: string; body: { decision: string; expectedRevision: number; instanceId: string } };
type PendingControl = { id: string; key: string; request: ControlRequest; busy: boolean };
type PendingControls = {
  entries: Map<string, PendingControl>;
  subscribe: (listener: () => void) => () => void;
  getVersion: () => number;
  changed: () => void;
};
// Page-memory HTTP receipts survive browsing another session. No business
// state or credentials are persisted, and separate clients never share them.
const controlRequests = new WeakMap<Client, Map<string, PendingControls>>();
function retainedControls(client: Client, sid: string): PendingControls {
  let sessions = controlRequests.get(client);
  if (!sessions) { sessions = new Map(); controlRequests.set(client, sessions); }
  let controls = sessions.get(sid);
  if (!controls) {
    let version = 0;
    const listeners = new Set<() => void>();
    controls = {
      entries: new Map(),
      subscribe: (listener) => { listeners.add(listener); return () => { listeners.delete(listener); }; },
      getVersion: () => version,
      changed: () => { version++; for (const listener of listeners) listener(); },
    };
    sessions.set(sid, controls);
  }
  return controls;
}
const CONTROL_TEXT = { resume: "恢复", reconcile: "核对", approval: "审批" };

function controlID(request: ControlRequest) {
  if (request.kind === "resume") return "resume:" + request.traceId;
  if (request.kind === "approval") return `approval:${request.interactionId}:${request.body.decision}`;
  const b = request.body;
  return `reconcile:${request.traceId}:${b.invocationId}:${b.toolCallId}:${b.observationId}:${b.observationVersion ?? 0}`;
}

type PendingUpload = { file: File; mimeType: string; key: string; error: string };

function mimeOf(file: File): string {
  if (ATTACHMENT_TYPES.includes(file.type)) return file.type;
  if (file.type === "" && /\.md$/i.test(file.name)) return "text/markdown";
  return "";
}

const btn = "rounded-control px-2 py-0.5 text-[12px] shadow-btn hover:bg-hover disabled:opacity-50";

export default function SessionView(props: { client: Client; sid: string }) {
  // Both a client replacement and a session switch isolate late UI responses.
  const clientBinding = useMemo(() => crypto.randomUUID(), [props.client]);
  return <SessionControls key={clientBinding + ":" + props.sid} {...props} />;
}

function SessionControls({ client, sid }: { client: Client; sid: string }) {
  const store = useMemo(() => new A2UIStore("session:" + sid), [sid]);
  const version = useSyncExternalStore(store.subscribe, store.getVersion);
  const [status, setStatus] = useState<SyncStatus>("idle");
  const [syncError, setSyncError] = useState("");
  const [caps, setCaps] = useState<Capabilities | null>(null);
  const [branches, setBranches] = useState<Branch[]>([]);
  const [snap, setSnap] = useState<Snapshot | null>(null);
  const [draft, setDraft] = useState("");
  const [target, setTarget] = useState("");
  const [actionError, setActionError] = useState("");
  const [approvalError, setApprovalError] = useState<Record<string, string>>({});
  const [forkName, setForkName] = useState("");
  const [forkFrom, setForkFrom] = useState("");
  const [forkSummarize, setForkSummarize] = useState(false);
  const [attached, setAttached] = useState<Attachment[]>([]);
  const [upload, setUpload] = useState<PendingUpload | null>(null);
  const [uploading, setUploading] = useState(false);
  const [evidence, setEvidence] = useState<Record<string, string>>({});
  // Bumping epoch re-runs render + subscribe for the current session.
  const [epoch, setEpoch] = useState(0);
  const [opening, setOpening] = useState(false);
  const openRequest = useRef<AbortController | null>(null);
  // These are unconfirmed HTTP requests, never a copy of session business state.
  const pendingControls = useMemo(() => retainedControls(client, sid), [client, sid]);
  useSyncExternalStore(pendingControls.subscribe, pendingControls.getVersion);
  const pendingChanged = pendingControls.changed;
  const keys = useKeys();

  useEffect(() => () => openRequest.current?.abort(), [client, sid]);

  useEffect(() => {
    const ctrl = new AbortController();
    void syncSession(client, sid, store, ctrl.signal, (s, err) => {
      setStatus(s);
      setSyncError(err ? errorText(err) : "");
    });
    // Switching sessions unmounts this view and a re-render bumps epoch: both
    // abort the old stream, and the store binding discards its late frames.
    return () => ctrl.abort();
  }, [client, sid, store, epoch]);

  useEffect(() => {
    const ctrl = new AbortController();
    client.capabilities(sid, ctrl.signal).then((c) => { if (!ctrl.signal.aborted) setCaps({ ...c, agents: c.agents.filter((a) => a.kind !== "workflow") }); }, () => { if (!ctrl.signal.aborted) setCaps(null); });
    client.branches(sid, ctrl.signal).then((b) => { if (!ctrl.signal.aborted) setBranches(b.branches); }, () => { if (!ctrl.signal.aborted) setBranches([]); });
    return () => ctrl.abort();
  }, [client, sid, epoch]);

  // The snapshot drives resume/queue/reconcile controls; refetch shortly after
  // the surface changes instead of on every streamed chunk.
  useEffect(() => {
    const ctrl = new AbortController();
    const t = setTimeout(() => {
      client.snapshot(sid, ctrl.signal).then((fresh) => { if (!ctrl.signal.aborted) setSnap(fresh); }, () => undefined);
    }, 250);
    return () => {
      clearTimeout(t);
      ctrl.abort();
    };
  }, [client, sid, version, epoch]);

  const surface = store.snapshot;
  const messageIds = [...surface.components.values()].flatMap((c) => ("ChatMessage" in c.component && c.component.ChatMessage.status === "final" ? [c.component.ChatMessage.messageId] : []));
  const running = [...surface.components.values()].some((c) => "Task" in c.component && ["queued", "running", "cancelling"].includes(c.component.Task.state));
  const traces = snap?.traces ?? [];
  const held = traces.filter((t) => t.state === "queued" && t.hold);
  const resumable = traces.filter((t) => t.canResume);
  const reconciliations = snap?.pendingReconciliations ?? [];
  const writable = status === "live" && !opening;

  const openControl = async () => {
    if (openRequest.current || writable) return;
    const ctrl = new AbortController();
    openRequest.current = ctrl;
    setOpening(true);
    setActionError("");
    try {
      const fresh = await client.openSession(sid, ctrl.signal);
      if (ctrl.signal.aborted) return;
      setSnap(fresh);
      setStatus("loading");
      setEpoch((n) => n + 1);
    } catch (err) {
      if (!ctrl.signal.aborted) setActionError(errorText(err));
    } finally {
      if (openRequest.current === ctrl) openRequest.current = null;
      if (!ctrl.signal.aborted) setOpening(false);
    }
  };

  const run = async (action: string, fn: (key: string) => Promise<unknown>) => {
    setActionError("");
    try {
      await fn(keys.get(action));
      keys.done(action);
      return true;
    } catch (err) {
      setActionError(errorText(err));
      return false;
    }
  };

  const send = async () => {
    const text = draft.trim();
    if (!text) return;
    const content: ContentBlock[] = [{ type: "text", text }, ...attached.map((a) => ({ type: "attachment" as const, artifactId: a.artifactId }))];
    const action = "prompt:" + JSON.stringify([text, target, attached.map((a) => a.artifactId)]);
    if (await run(action, (key) => client.submitPrompt(sid, content, target, key))) {
      setDraft("");
      setAttached([]);
    }
  };

  const doUpload = async (u: PendingUpload) => {
    setUploading(true);
    try {
      const saved = await client.uploadAttachment(sid, u.file, u.mimeType, u.file.name, u.key);
      setAttached((list) => (list.some((a) => a.artifactId === saved.artifactId) ? list : [...list, { ...saved, name: saved.name || u.file.name }]));
      setUpload(null);
    } catch (err) {
      // The same key is kept so a retry cannot store the file twice.
      setUpload({ ...u, error: errorText(err) });
    } finally {
      setUploading(false);
    }
  };

  const pickFile = (file: File | undefined) => {
    if (!file) return;
    const mimeType = mimeOf(file);
    if (!mimeType) {
      setUpload(null);
      setActionError("不支持的文件类型，仅支持文本、Markdown、JSON、PNG 和 JPEG。");
      return;
    }
    setActionError("");
    const u = { file, mimeType, key: newIdempotencyKey(), error: "" };
    setUpload(u);
    void doUpload(u);
  };

  const retryControl = async (pending: PendingControl) => {
    if (!writable || pending.busy) return;
    pending.busy = true;
    pendingChanged();
    setActionError("");
    const r = pending.request;
    try {
      if (r.kind === "resume") await client.resumeTrace(sid, r.traceId, r.body.expectedRevision, pending.key);
      else if (r.kind === "reconcile") await client.reconcile(sid, r.traceId, r.body, r.body.expectedRevision, pending.key);
      else await client.respond(sid, r.interactionId, r.body.decision, r.body.expectedRevision, r.body.instanceId, pending.key);
      pendingControls.entries.delete(pending.id);
    } catch (err) {
      // A 4xx response is a definite refusal, so the next explicit click may
      // start a new request. Network/5xx/lost JSON receipts remain unconfirmed.
      if (err instanceof APIError && ((err.status >= 400 && err.status < 500) || (err.status === 0 && err.code === "invalid_argument"))) pendingControls.entries.delete(pending.id);
      setActionError(errorText(err));
    } finally {
      pending.busy = false;
      pendingChanged();
    }
  };

  const control = (request: ControlRequest) => {
    if (!writable) return;
    const id = controlID(request);
    let pending = pendingControls.entries.get(id);
    if (!pending) {
      pending = { id, key: newIdempotencyKey(), request, busy: false };
      pendingControls.entries.set(id, pending);
    }
    // Once retained, only the original normalized body may be sent again.
    void retryControl(pending);
  };

  const decide = (a: ApprovalComp, decision: string) => {
    if (!snap || !writable || [...pendingControls.entries.values()].some((p) => p.request.kind === "approval" && p.request.interactionId === a.interactionId)) return;
    if (snap.instanceId !== a.instanceId) {
      setApprovalError((m) => ({ ...m, [a.interactionId]: "服务实例已变化，请刷新后重新审批。" }));
      return;
    }
    setApprovalError((m) => ({ ...m, [a.interactionId]: "" }));
    control({ kind: "approval", interactionId: a.interactionId, body: { decision, expectedRevision: snap.revision, instanceId: a.instanceId } });
  };

  // Branch changes replace the visible history: re-render and resubscribe.
  const rerender = () => setEpoch((n) => n + 1);

  // A committed compaction has no live UI event; wait for the operation to
  // settle, then re-render so the summary replaces the covered history.
  const compact = async () => {
    let oid = "";
    const ok = await run("compact", async (key) => {
      oid = (await client.compact(sid, key)).operationId;
    });
    if (!ok || !oid) return;
    for (let i = 0; i < 120; i++) {
      const op = await client.operation(sid, oid).catch(() => null);
      if (op && ["completed", "failed", "cancelled"].includes(op.state)) {
        if (op.state !== "completed") setActionError("上下文压缩未完成，原有历史保持不变。");
        rerender();
        return;
      }
      await new Promise((r) => setTimeout(r, 500));
    }
  };

  return (
    <div className="mx-auto flex w-full max-w-3xl min-w-0 flex-col gap-4">
      <header className="flex min-w-0 flex-wrap items-center gap-3">
        <h2 className="min-w-0 truncate font-mono text-[13px] font-semibold">{sid}</h2>
        {status === "loading" || status === "reconnecting" ? <LoadingState label={STATUS_TEXT[status]} /> : <span className="text-[12px] text-ink-3">{STATUS_TEXT[status]}</span>}
        {!writable && (
          <button type="button" disabled={opening || !["ended", "error"].includes(status)} onClick={() => void openControl()} className={btn}>
            打开会话控制
          </button>
        )}
        {syncError && (
          <span role="alert" className="text-[12px] text-red">
            {syncError}
          </span>
        )}
      </header>

      <section aria-label="对话" className="flex min-h-40 min-w-0 flex-col gap-2.5">
        <Surface
          state={surface}
          controls={{
            taskAction: (traceId, state) =>
              ["queued", "running", "paused"].includes(state) && writable ? (
                <button type="button" onClick={() => void run("cancel:" + traceId, (key) => client.cancelTrace(sid, traceId, key))} className={"shrink-0 text-ink-2 " + btn}>
                  取消
                </button>
              ) : null,
            approval: (a) => (
              <ApprovalCard
                key={a.interactionId}
                question={a.question}
                options={a.options}
                busy={!writable || !snap || [...pendingControls.entries.values()].some((p) => p.request.kind === "approval" && p.request.interactionId === a.interactionId)}
                error={approvalError[a.interactionId] ?? ""}
                onDecide={(d) => void decide(a, d)}
              />
            ),
          }}
        />
        {running && writable && <LoadingState label="任务执行中" />}
      </section>

      {(held.length > 0 || resumable.length > 0 || reconciliations.length > 0) && (
        <section aria-label="待处理操作" className="flex flex-col gap-2 rounded-card bg-surface p-3 text-[12px] shadow-card">
          <h3 className="font-medium">待处理操作</h3>
          {held.length > 0 && (
            <div className="flex flex-wrap items-center gap-2">
              <span className="text-ink-2">{held.length} 个排队任务已暂停调度</span>
              <button
                type="button"
                disabled={!writable}
                onClick={() => void run("continue:" + held.map((t) => t.traceId).join(","), (key) => client.continueQueue(sid, held.map((t) => t.traceId), key))}
                className={btn}
              >
                继续队列
              </button>
            </div>
          )}
          {resumable.map((t) => (
            <div key={t.traceId} className="flex min-w-0 flex-wrap items-center gap-2">
              <span className="min-w-0 truncate font-mono">{t.traceId}</span>
              <span className="text-ink-2">可恢复</span>
              <button
                type="button"
                disabled={!writable || !snap || pendingControls.entries.get("resume:" + t.traceId)?.busy}
                onClick={() => snap && control({ kind: "resume", traceId: t.traceId, body: { expectedRevision: snap.revision } })}
                className={btn}
              >
                恢复
              </button>
            </div>
          ))}
          {reconciliations.map((r) => {
            const id = `${r.traceId}:${r.invocationId}:${r.toolCallId}:${r.observationId}:${r.observationVersion ?? 0}`;
            const pending = pendingControls.entries.has("reconcile:" + id);
            const ref = (evidence[id] ?? "").trim();
            return (
              <form
                key={id}
                className="flex min-w-0 flex-wrap items-center gap-2"
                onSubmit={(e) => {
                  e.preventDefault();
                  if (!snap || !ref || pending) return;
                  control({ kind: "reconcile", traceId: r.traceId, body: { invocationId: r.invocationId, toolCallId: r.toolCallId, observationId: r.observationId, observationVersion: r.observationVersion, evidenceRef: ref, expectedRevision: snap.revision } });
                }}
              >
                <span className="min-w-0 truncate">工具调用 <span className="font-mono">{r.toolCallId}</span> 的执行结果未知，需要核对</span>
                <input
                  value={evidence[id] ?? ""}
                  disabled={pending}
                  onChange={(e) => setEvidence((m) => ({ ...m, [id]: e.target.value }))}
                  aria-label="核对证据引用"
                  placeholder="证据引用"
                  className="min-w-0 rounded-control border border-line bg-surface px-1.5 py-0.5"
                />
                <button type="submit" disabled={!writable || !ref || pending} className={btn}>
                  提交核对
                </button>
              </form>
            );
          })}
        </section>
      )}

      {pendingControls.entries.size > 0 && (
        <section aria-label="未确认请求" className="flex flex-col gap-2 rounded-card bg-surface p-3 text-[12px] shadow-card">
          <h3 className="font-medium">未确认请求</h3>
          <p className="text-ink-2">重试将发送原请求以取得回执。放弃只清除页面记录，不会撤销服务端可能已接纳的操作。</p>
          {[...pendingControls.entries.values()].map((p) => {
            const r = p.request;
            const label = CONTROL_TEXT[r.kind];
            return (
              <div key={p.id} className="flex min-w-0 flex-wrap items-center gap-2">
                <span className="break-words">{label} {r.kind === "approval" ? r.interactionId : r.traceId}：{p.busy ? "等待回执" : "响应未确认"}</span>
                {r.kind === "reconcile" && <span className="break-words text-ink-2">原证据：{r.body.evidenceRef}</span>}
                {r.kind === "approval" && <span className="text-ink-2">原决定：{r.body.decision === "allowed-once" ? "批准一次" : "拒绝"}</span>}
                <button type="button" disabled={!writable || p.busy} onClick={() => void retryControl(p)} className={btn}>重试{label}</button>
                <button type="button" disabled={p.busy} onClick={() => { pendingControls.entries.delete(p.id); setActionError(""); pendingChanged(); }} className={btn}>放弃{label}</button>
              </div>
            );
          })}
        </section>
      )}

      {actionError && (
        <p role="alert" className="text-[12.5px] text-red">
          {actionError}
        </p>
      )}

      <Composer
        disabled={!writable}
        value={draft}
        onChange={setDraft}
        onSend={() => void send()}
        placeholder="输入消息，Enter 发送，Shift+Enter 换行"
        extra={
          <div className="flex min-w-0 flex-wrap items-center gap-2">
            {caps && caps.agents.length > 0 && (
              <label className="flex items-center gap-1 text-[12px] text-ink-2">
                目标 Agent
                <select value={target} onChange={(e) => setTarget(e.target.value)} className="max-w-36 rounded-chip bg-surface px-1 py-0.5 text-[12px] shadow-hairline">
                  <option value="">默认</option>
                  {caps.agents.map((a) => (
                    <option key={a.name} value={a.name}>
                      {a.name}
                    </option>
                  ))}
                </select>
              </label>
            )}
            <label className="flex items-center gap-1 text-[12px] text-ink-2">
              附件
              <input
                type="file"
                aria-label="添加附件"
                accept={ATTACHMENT_TYPES.join(",") + ",.md"}
                disabled={!writable || uploading}
                onChange={(e) => {
                  pickFile(e.target.files?.[0]);
                  e.target.value = "";
                }}
                className="max-w-40 text-[12px] file:mr-1 file:rounded-chip file:border-0 file:bg-surface file:px-1.5 file:py-0.5 file:shadow-hairline"
              />
            </label>
          </div>
        }
      />

      {(attached.length > 0 || upload) && (
        <ul aria-label="待发送附件" className="flex flex-col gap-1 text-[12px]">
          {attached.map((a) => (
            <li key={a.artifactId} className="flex min-w-0 items-center gap-2">
              <span className="min-w-0 truncate">{a.name || a.artifactId}</span>
              <span className="shrink-0 text-ink-3">{a.mimeType}</span>
              <button type="button" onClick={() => setAttached((l) => l.filter((x) => x.artifactId !== a.artifactId))} className={btn}>
                移除
              </button>
            </li>
          ))}
          {upload && (
            <li className="flex min-w-0 items-center gap-2">
              <span className="min-w-0 truncate">{upload.file.name}</span>
              {upload.error ? (
                <>
                  <span role="alert" className="text-red">
                    {upload.error}
                  </span>
                  <button type="button" disabled={!writable || uploading} onClick={() => void doUpload(upload)} className={btn}>
                    重试上传
                  </button>
                </>
              ) : (
                <span className="text-ink-3">上传中</span>
              )}
            </li>
          )}
        </ul>
      )}

      {caps && (
        <section aria-label="能力" className="rounded-card bg-surface p-3 text-[12px] shadow-card">
          <h3 className="font-medium">能力</h3>
          <p className="mt-1 break-words text-ink-2">
            Agent：
            {caps.agents.map((a) => (
              <span key={a.name} className="mr-2 font-mono text-ink">
                {a.name}
              </span>
            ))}
          </p>
          <p className="mt-1 break-words text-ink-2">
            工具：{caps.tools.length === 0 ? "无" : caps.tools.map((t) => <span key={t} className="mr-2 font-mono text-ink">{t}</span>)}
          </p>
        </section>
      )}

      <details className="rounded-card bg-surface p-3 shadow-card">
        <summary className="cursor-pointer text-[12.5px] font-medium">分支与压缩</summary>
        <ul aria-label="分支列表" className="mt-2 flex flex-col gap-1">
          {branches.map((b) => (
            <li key={b.branchId} className="flex min-w-0 items-center gap-2 text-[12px]">
              <span className="min-w-0 truncate font-mono">{b.branchId}</span>
              {b.active ? (
                <span className="text-green">当前</span>
              ) : (
                <button
                  type="button"
                  disabled={!writable}
                  onClick={() =>
                    void run("activate:" + b.branchId, () => client.activate(sid, b.branchId)).then((ok) => {
                      if (ok) rerender();
                    })
                  }
                  className={"text-ink-2 " + btn}
                >
                  切换
                </button>
              )}
            </li>
          ))}
        </ul>
        <form
          className="mt-2 flex flex-wrap items-center gap-2 text-[12px]"
          onSubmit={(e) => {
            e.preventDefault();
            const summarize = FORK_SUMMARIZE_SUPPORTED && forkSummarize;
            void run("fork:" + forkName, () => client.fork(sid, forkName.trim(), forkFrom, summarize)).then((ok) => {
              if (ok) {
                setForkName("");
                rerender();
              }
            });
          }}
        >
          <input value={forkName} onChange={(e) => setForkName(e.target.value)} placeholder="新分支名" aria-label="新分支名" className="min-w-0 rounded-control border border-line bg-surface px-1.5 py-0.5" />
          <select value={forkFrom} onChange={(e) => setForkFrom(e.target.value)} aria-label="分叉起点消息" className="max-w-48 min-w-0 rounded-control border border-line bg-surface px-1 py-0.5 font-mono">
            <option value="">选择起点消息</option>
            {messageIds.map((id) => (
              <option key={id} value={id}>
                {id}
              </option>
            ))}
          </select>
          {FORK_SUMMARIZE_SUPPORTED && (
            <label className="flex items-center gap-1 text-ink-2">
              <input type="checkbox" checked={forkSummarize} onChange={(e) => setForkSummarize(e.target.checked)} />
              为离开的分支生成摘要
            </label>
          )}
          <button type="submit" disabled={!writable || !forkName.trim() || !forkFrom} className={btn}>
            创建分支
          </button>
          <button type="button" disabled={!writable} onClick={() => void compact()} className={btn}>
            手动压缩上下文
          </button>
        </form>
      </details>
    </div>
  );
}
