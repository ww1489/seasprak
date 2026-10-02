import { useEffect, useRef, useState } from "react";
import { APIError, Client, errorText, newIdempotencyKey, type SessionEntry, type WorkflowInfo, type WorkflowRunEntry } from "./api/client";
import { workflowSnapshotDTO } from "./api/workflow";
import SessionView from "./SessionView";
import WorkflowView from "./WorkflowView";
import { buildInput, formFromSchema } from "./workflowForm";

function TokenForm({ onToken }: { onToken: (t: string) => void }) {
  const [value, setValue] = useState("");
  return (
    <form
      aria-label="访问令牌"
      className="mx-auto mt-24 flex w-full max-w-md flex-col gap-3 rounded-card bg-surface p-5 shadow-card"
      onSubmit={(e) => {
        e.preventDefault();
        const t = value.trim();
        if (t) {
          setValue("");
          onToken(t);
        }
      }}
    >
      <h1 className="text-[15px] font-semibold">连接本地服务</h1>
      <p className="text-[12.5px] leading-normal text-ink-2">
        粘贴启动时写入“Bearer file”中的令牌。令牌只保存在当前页面内存中，刷新页面后需要重新输入。
      </p>
      <label className="flex flex-col gap-1 text-[12.5px] text-ink-2">
        令牌
        <input
          type="password"
          autoComplete="off"
          spellCheck={false}
          value={value}
          onChange={(e) => setValue(e.target.value)}
          className="rounded-control border border-line bg-field px-2.5 py-1.5 font-mono text-[12.5px] text-ink outline-none focus:border-line-strong"
        />
      </label>
      <button type="submit" disabled={!value.trim()} className="self-start rounded-control bg-ink px-3 py-1.5 text-[12.5px] font-medium text-surface disabled:opacity-50">
        连接
      </button>
    </form>
  );
}

export default function App() {
  const [connection, setConnection] = useState<{ client: Client; sessions: SessionEntry[] } | null>(null);
  const [error, setError] = useState("");
  const connecting = useRef<AbortController | null>(null);
  useEffect(() => () => connecting.current?.abort(), []);
  if (connection) return <ResourceBrowser client={connection.client} initialSessions={connection.sessions} />;
  return <main className="min-h-screen px-4">
    <TokenForm onToken={async (token) => {
      connecting.current?.abort();
      const ctrl = new AbortController();
      connecting.current = ctrl;
      const client = new Client(token);
      try {
        const list = await client.listSessions(ctrl.signal);
        if (!ctrl.signal.aborted) { setConnection({ client, sessions: list.sessions }); setError(""); }
      } catch (err) { if (!ctrl.signal.aborted) setError(errorText(err)); }
    }} />
    {error && <p role="alert" className="mx-auto mt-3 max-w-md text-[12.5px] text-red">{error}</p>}
  </main>;
}

type Pane = "code" | "workflow";
type PendingSessionCreate = { key: string; workspace: string; busy: boolean };
type PendingWorkflowCreate = { key: string; body: { workspace: string; workflow: string; version: string; input: Record<string, unknown> }; busy: boolean };
const inputClass = "min-w-0 rounded-control border border-line bg-surface px-2 py-1 text-[12px] text-ink outline-none focus:border-line-strong";
const buttonClass = "rounded-control bg-ink px-2.5 py-1 text-[12.5px] font-medium text-surface disabled:opacity-50";
const absoluteWorkspace = (path: string) => path.startsWith("/") || /^[a-z]:[\\/]/i.test(path) || /^\\\\[^\\]+\\[^\\]+/.test(path);
const refused = (err: unknown) => err instanceof APIError && err.status >= 400 && err.status < 500;
const definitionKey = (w: WorkflowInfo) => JSON.stringify([w.name, w.version]);
const uniqueRuns = (runs: WorkflowRunEntry[]) => [...new Map(runs.map((run) => [run.runId, run])).values()];

function ResourceBrowser({ client, initialSessions }: { client: Client; initialSessions: SessionEntry[] }) {
  const [pane, setPane] = useState<Pane>("code");
  const [sessions, setSessions] = useState(initialSessions);
  const [workflows, setWorkflows] = useState<WorkflowInfo[]>([]);
  const [runs, setRuns] = useState<WorkflowRunEntry[]>([]);
  const [runNext, setRunNext] = useState("");
  const [pageLoading, setPageLoading] = useState(false);
  const workflowPage = useRef<{ ctrl: AbortController; busy: boolean } | null>(null);
  const [sid, setSid] = useState("");
  const [rid, setRid] = useState("");
  const [workspace, setWorkspace] = useState("");
  const [workflowWorkspace, setWorkflowWorkspace] = useState("");
  const [selection, setSelection] = useState("");
  const [values, setValues] = useState<Record<string, string>>({});
  const [errors, setErrors] = useState({ code: "", workflow: "" });
  const [listEpoch, setListEpoch] = useState(0);
  const [, setRequestVersion] = useState(0);
  const changed = () => setRequestVersion((n) => n + 1);
  const error = (owner: Pane, message: string) => setErrors((old) => ({ ...old, [owner]: message }));
  // Pending creates belong to their resource type and preserve the original
  // key/body through form edits or navigation until success or explicit abandon.
  const codeCreate = useRef<PendingSessionCreate | null>(null);
  const workflowCreate = useRef<PendingWorkflowCreate | null>(null);
  const binding = useRef<AbortController | null>(null);
  useEffect(() => {
    const ctrl = new AbortController();
    binding.current = ctrl;
    return () => ctrl.abort();
  }, [client, pane, sid, rid]);
  useEffect(() => {
    const ctrl = new AbortController();
    if (pane === "code") {
      workflowPage.current = null;
      client.listSessions(ctrl.signal).then((list) => { if (!ctrl.signal.aborted) setSessions(list.sessions); }, (err) => { if (!ctrl.signal.aborted) error("code", errorText(err)); });
    } else {
      const page = { ctrl, busy: true };
      workflowPage.current = page;
      setRuns([]); setRunNext(""); setPageLoading(true);
      client.workflows(ctrl.signal).then((list) => { if (!ctrl.signal.aborted) setWorkflows(list.workflows); }, (err) => { if (!ctrl.signal.aborted) error("workflow", errorText(err)); });
      client.listWorkflowRuns(ctrl.signal).then((list) => {
        if (!ctrl.signal.aborted && workflowPage.current === page) { setRuns(uniqueRuns(list.runs)); setRunNext(list.next ?? ""); }
      }, (err) => { if (!ctrl.signal.aborted) error("workflow", errorText(err)); }).finally(() => {
        page.busy = false; if (!ctrl.signal.aborted && workflowPage.current === page) setPageLoading(false);
      });
    }
    return () => ctrl.abort();
  }, [client, pane, listEpoch]);
  const loadMoreRuns = async () => {
    const page = workflowPage.current;
    if (pane !== "workflow" || !runNext || !page || page.busy || page.ctrl.signal.aborted) return;
    page.busy = true; setPageLoading(true); error("workflow", "");
    try {
      const list = await client.listWorkflowRuns(page.ctrl.signal, { after: runNext });
      if (page.ctrl.signal.aborted || workflowPage.current !== page) return;
      setRuns((old) => uniqueRuns([...old, ...list.runs])); setRunNext(list.next ?? "");
    } catch (err) { if (!page.ctrl.signal.aborted && workflowPage.current === page) error("workflow", errorText(err)); }
    finally { page.busy = false; if (!page.ctrl.signal.aborted && workflowPage.current === page) setPageLoading(false); }
  };

  const workflow = workflows.find((w) => definitionKey(w) === selection);
  const form = workflow ? formFromSchema(workflow.inputSchema) : null;
  const retrySessionCreate = async (request: PendingSessionCreate) => {
    if (request.busy || !binding.current) return;
    const signal = binding.current.signal;
    request.busy = true; changed(); error("code", "");
    try {
      const snap = await client.createSession(request.workspace, request.key, signal);
      if (signal.aborted) return;
      if (typeof snap.sessionId !== "string" || !snap.sessionId) throw new APIError(0, "resource_unavailable");
      codeCreate.current = null;
      setSid(snap.sessionId); setListEpoch((n) => n + 1);
    } catch (err) {
      if (!signal.aborted) { if (refused(err)) codeCreate.current = null; error("code", errorText(err)); }
    } finally { request.busy = false; changed(); }
  };
  const createSession = () => {
    if (codeCreate.current) return;
    const path = workspace.trim();
    if (!absoluteWorkspace(path)) { error("code", "请填写工作区绝对路径。"); return; }
    const request = { key: newIdempotencyKey(), workspace: path, busy: false };
    codeCreate.current = request;
    void retrySessionCreate(request);
  };
  const retryWorkflowCreate = async (request: PendingWorkflowCreate) => {
    if (request.busy || !binding.current) return;
    const signal = binding.current.signal;
    request.busy = true; changed(); error("workflow", "");
    const body = request.body;
    try {
      const raw = await client.createWorkflowRun(body.workspace, body.workflow, body.version, body.input, request.key, signal);
      if (signal.aborted) return;
      if (typeof raw.runId !== "string" || !raw.runId) throw new APIError(0, "resource_unavailable");
      const snap = workflowSnapshotDTO(raw, raw.runId);
      workflowCreate.current = null;
      setRid(snap.runId); setListEpoch((n) => n + 1);
    } catch (err) {
      if (!signal.aborted) { if (refused(err)) workflowCreate.current = null; error("workflow", errorText(err)); }
    } finally { request.busy = false; changed(); }
  };
  const createWorkflow = () => {
    if (workflowCreate.current || !workflow || !form || !("fields" in form)) return;
    const path = workflowWorkspace.trim();
    if (!absoluteWorkspace(path)) { error("workflow", "请填写工作区绝对路径。"); return; }
    const built = buildInput(form.fields, values);
    if ("invalid" in built) { error("workflow", `参数“${built.invalid}”无效或缺失。`); return; }
    const body = Object.freeze({ workspace: path, workflow: workflow.name, version: workflow.version, input: Object.freeze(built.input) });
    const request = { key: newIdempotencyKey(), body, busy: false };
    workflowCreate.current = request;
    void retryWorkflowCreate(request);
  };

  return (
    <div className="grid min-h-screen grid-cols-1 md:grid-cols-[260px_minmax(0,1fr)]">
      <aside className="flex min-w-0 flex-col gap-3 border-b border-line bg-canvas p-3 md:border-r md:border-b-0">
        <nav aria-label="资源类型" className="flex flex-wrap gap-2">
          <button type="button" aria-pressed={pane === "code"} onClick={() => setPane("code")} className={"rounded-control px-2 py-1 text-[12.5px] " + (pane === "code" ? "bg-surface shadow-hairline" : "text-ink-2")}>代码会话</button>
          <button type="button" aria-pressed={pane === "workflow"} onClick={() => setPane("workflow")} className={"rounded-control px-2 py-1 text-[12.5px] " + (pane === "workflow" ? "bg-surface shadow-hairline" : "text-ink-2")}>工作流</button>
        </nav>
        <h1 className="text-[14px] font-semibold">{pane === "code" ? "会话" : "工作流"}</h1>
        {pane === "code" ? <>
          <form aria-label="新建会话" className="flex flex-col gap-1.5" onSubmit={(e) => { e.preventDefault(); createSession(); }}>
            <label className="flex flex-col gap-1 text-[12px] text-ink-2">工作区绝对路径
              <input value={workspace} onChange={(e) => setWorkspace(e.target.value)} spellCheck={false} className={inputClass + " font-mono"} />
            </label>
            <button type="submit" disabled={!!codeCreate.current || !workspace.trim()} className={buttonClass}>新建会话</button>
          </form>
          {codeCreate.current && <PendingCreate label="会话" busy={codeCreate.current.busy} retry={() => void retrySessionCreate(codeCreate.current!)} abandon={() => { codeCreate.current = null; changed(); error("code", ""); }} />}
          <ul aria-label="会话列表" className="flex flex-col gap-1">
            {sessions.map((s) => <li key={s.sessionId}><button type="button" aria-current={s.sessionId === sid} onClick={() => setSid(s.sessionId)} className={`w-full truncate rounded-control px-2 py-1 text-left font-mono text-[12px] ${s.sessionId === sid ? "bg-surface shadow-hairline" : "hover:bg-hover-2"} ${s.available ? "text-ink" : "text-ink-3"}`}>{s.sessionId}</button></li>)}
            {sessions.length === 0 && <li className="text-[12px] text-ink-3">暂无会话</li>}
          </ul>
        </> : <>
          <form aria-label="新建工作流运行" className="flex flex-col gap-2 text-[12px]" onSubmit={(e) => { e.preventDefault(); createWorkflow(); }}>
            <label className="flex flex-col gap-1 text-ink-2">工作流定义
              <select value={selection} onChange={(e) => { setSelection(e.target.value); setValues({}); error("workflow", ""); }} className={inputClass}>
                <option value="">选择定义与版本</option>
                {workflows.map((w) => <option key={definitionKey(w)} value={definitionKey(w)}>{w.name} · {w.version}</option>)}
              </select>
            </label>
            {workflow?.description && <p className="break-words text-ink-2">{workflow.description}</p>}
            <label className="flex flex-col gap-1 text-ink-2">工作区绝对路径
              <input value={workflowWorkspace} onChange={(e) => setWorkflowWorkspace(e.target.value)} spellCheck={false} className={inputClass + " font-mono"} />
            </label>
            {form && "fields" in form && form.fields.map((f) => <label key={f.name} className="flex min-w-0 flex-col gap-1 text-ink-2">
              <span>{f.name}{f.required ? "（必填）" : ""}{f.description ? `：${f.description}` : ""}</span>
              {f.enum || f.kind === "boolean" ? <select value={values[f.name] ?? ""} onChange={(e) => setValues((m) => ({ ...m, [f.name]: e.target.value }))} className={inputClass}>
                <option value="">未选择</option>{(f.enum ?? [true, false]).map((v) => <option key={String(v)} value={String(v)}>{String(v)}</option>)}
              </select> : <input type={f.kind === "string" ? "text" : "number"} step={f.kind === "integer" ? 1 : "any"} value={values[f.name] ?? ""} onChange={(e) => setValues((m) => ({ ...m, [f.name]: e.target.value }))} className={inputClass} />}
            </label>)}
            {form && "unsupported" in form && <p role="alert" className="text-red">该工作流的参数结构无法生成表单。</p>}
            <button type="submit" disabled={!!workflowCreate.current || !workflowWorkspace.trim() || !form || !("fields" in form)} className={buttonClass}>启动工作流</button>
          </form>
          {workflowCreate.current && <PendingCreate label="工作流" busy={workflowCreate.current.busy} retry={() => void retryWorkflowCreate(workflowCreate.current!)} abandon={() => { workflowCreate.current = null; changed(); error("workflow", ""); }} />}
          <ul aria-label="工作流运行列表" className="flex flex-col gap-1">
            {runs.map((run) => <li key={run.runId}><button type="button" aria-current={run.runId === rid} onClick={() => setRid(run.runId)} className={`flex w-full min-w-0 flex-col gap-0.5 rounded-control px-2 py-1 text-left text-[12px] ${run.runId === rid ? "bg-surface shadow-hairline" : "hover:bg-hover-2"} ${run.available ? "text-ink" : "text-ink-3"}`}>
              <span className="max-w-full truncate font-mono">{run.runId}</span><span className="max-w-full truncate text-[11px] text-ink-3">{run.definitionName} · {run.definitionVersion} · {run.state}</span>
            </button></li>)}
            {runs.length === 0 && <li className="text-[12px] text-ink-3">暂无工作流运行</li>}
          </ul>
          {runNext && <button type="button" disabled={pageLoading} onClick={() => void loadMoreRuns()} className="self-start rounded-control px-2 py-1 text-[12px] shadow-btn disabled:opacity-50">加载更多工作流运行</button>}
        </>}
        <button type="button" onClick={() => { error(pane, ""); setListEpoch((n) => n + 1); }} className="self-start text-[12px] text-ink-2 hover:text-ink">刷新列表</button>
        {errors[pane] && <p role="alert" className="text-[12px] text-red">{errors[pane]}</p>}
      </aside>
      <main className="min-w-0 p-4">
        {pane === "code" ? (sid ? <SessionView key={"session:" + sid} client={client} sid={sid} /> : <p className="text-[13px] text-ink-3">请选择或新建一个会话。</p>)
          : (rid ? <WorkflowView key={"workflow:" + rid} client={client} rid={rid} /> : <p className="text-[13px] text-ink-3">请选择历史运行，或选择定义创建一个工作流运行。</p>)}
      </main>
    </div>
  );
}

function PendingCreate({ label, busy, retry, abandon }: { label: string; busy: boolean; retry: () => void; abandon: () => void }) {
  return <section aria-label={"未确认" + label + "创建"} className="flex flex-col gap-2 rounded-card bg-surface p-2 text-[12px] shadow-card">
    <p className="text-ink-2">{busy ? "等待创建回执。" : "创建响应未确认。修改表单不会改变原请求；重试使用原正文，放弃不会撤销可能已创建的对象。"}</p>
    <div className="flex flex-wrap gap-2">
      <button type="button" disabled={busy} onClick={retry} className="rounded-control px-2 py-1 shadow-btn disabled:opacity-50">重试创建{label}</button>
      <button type="button" disabled={busy} onClick={abandon} className="rounded-control px-2 py-1 shadow-btn disabled:opacity-50">放弃创建{label}</button>
    </div>
  </section>;
}
