import { StrictMode } from "react";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import WorkflowView from "./WorkflowView";
import { Client, type WorkflowSnapshot } from "./api/client";

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const cursor = (rid: string, seq = "9007199254740993") => btoa(JSON.stringify([2, "workflow", rid, seq])).replaceAll("+", "-").replaceAll("/", "_").replace(/=+$/, "");
const initial = (rid: string): WorkflowSnapshot => ({
  runId: rid, definitionName: "review", definitionVersion: "v1", state: "paused", revision: 7,
  cursor: cursor(rid), durableSeq: "9007199254740993", instanceId: "instance-1", executionStopped: true, canResume: true,
  workflowNodes: [{ nodeExecutionId: "node-execution-1", nodeId: "review-node", kind: "tool", state: "waiting" }],
  interactions: [{ interactionId: "approval1", nodeExecutionId: "node-execution-1", question: "工作流审批", options: ["allowed-once", "rejected", "cancelled"], instanceId: "instance-1" }],
});
function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (error: unknown) => void;
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}
type Request = { method: string; path: string; key: string; body: string; signal?: AbortSignal | null };
function service(live = true) {
  const requests: Request[] = [];
  const snapshots = new Map<string, WorkflowSnapshot>();
  const streams: { rid: string; send: (event: string, data: unknown, id?: string) => void; signal?: AbortSignal | null }[] = [];
  let opened = live;
  let autoReady = true;
  let post: ((r: Request) => Response | Promise<Response>) | undefined;
  let get: ((r: Request) => Response | Promise<Response> | undefined) | undefined;
  const snapshot = (rid: string) => { if (!snapshots.has(rid)) snapshots.set(rid, initial(rid)); return snapshots.get(rid)!; };
  vi.stubGlobal("fetch", vi.fn(async (path: string, opts: RequestInit = {}) => {
    const r: Request = { method: opts.method ?? "GET", path, key: new Headers(opts.headers).get("Idempotency-Key") ?? "", body: String(opts.body ?? ""), signal: opts.signal };
    requests.push(r);
    const rid = decodeURIComponent(path.split("/")[3] ?? "");
    const result = r.method === "GET" ? get?.(r) : undefined;
    if (result !== undefined) return result;
    if (r.method === "POST") {
      if (post) return post(r);
      if (path.endsWith("/open")) { opened = true; return json(snapshot(rid)); }
      return json({ operationId: "op1", state: "accepted", target: path.includes("/responses") ? decodeURIComponent(path.split("/")[5]) : rid, acceptedCommit: path.includes("/responses") ? 0 : 8, scope: path.includes("/responses") ? "instance" : "durable", ...(path.includes("/responses") ? { instanceId: "instance-1" } : {}) }, 202);
    }
    if (path.endsWith("/snapshot")) return json(snapshot(rid));
    if (path.includes("/operations/")) return json({ operationId: "op1", state: "completed", revision: snapshot(rid).revision });
    if (path.includes("/events")) {
      const stream = new ReadableStream<Uint8Array>({ start(controller) {
        const send = (event: string, data: unknown, id?: string) => controller.enqueue(new TextEncoder().encode(`event: ${event}\n${id ? `id: ${id}\n` : ""}data: ${JSON.stringify(data)}\n\n`));
        streams.push({ rid, send, signal: opts.signal });
        if (autoReady) send("ready", { handoff: snapshot(rid).cursor, live: opened });
        if (!opened) { send("end", {}); controller.close(); }
      }, cancel() {} });
      return new Response(stream);
    }
    throw new Error("unexpected offline workflow request: " + path);
  }));
  return {
    client: new Client(""), requests, snapshots, streams, snapshot,
    setPost(handler: typeof post) { post = handler; }, setGet(handler: typeof get) { get = handler; },
    setReady(value: boolean) { autoReady = value; }, setOpened(value: boolean) { opened = value; },
    ready() { for (const stream of streams) if (!stream.signal?.aborted) stream.send("ready", { handoff: snapshot(stream.rid).cursor, live: true }); },
    event(rid = "same", type = "workflow.state_changed", scope: unknown = { workflowRunId: rid }) {
      for (const stream of streams) if (stream.rid === rid && !stream.signal?.aborted) stream.send("event", { type, scope, payload: { state: "not-applied-directly" } });
    },
  };
}
const button = (name: string) => screen.getByRole<HTMLButtonElement>("button", { name });
const writes = (s: ReturnType<typeof service>) => s.requests.filter((r) => r.method === "POST" && !r.path.endsWith("/open"));
async function ready(s: ReturnType<typeof service>) {
  const view = render(<WorkflowView client={s.client} rid="same" />);
  await waitFor(() => expect(button("恢复工作流").disabled).toBe(false));
  return view;
}
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

describe("independent workflow view", () => {
  it("keeps explicit controls usable after React StrictMode's effect cleanup/restart", async () => {
    const s = service();
    render(<StrictMode><WorkflowView client={s.client} rid="same" /></StrictMode>);
    await waitFor(() => expect(button("恢复工作流").disabled).toBe(false));
    fireEvent.click(button("恢复工作流"));
    await waitFor(() => expect(writes(s)).toHaveLength(1));
    expect(writes(s)[0].signal?.aborted).toBe(false);
  });
  it("browses history with zero opens or resumes, and opens control only explicitly without auto-resume", async () => {
    const s = service(false);
    render(<WorkflowView client={s.client} rid="same" />);
    await screen.findByText("只读浏览（无运行中的写入者）");
    expect(button("恢复工作流").disabled).toBe(true);
    expect(button("批准一次").disabled).toBe(true);
    expect(s.requests.every((r) => r.method === "GET")).toBe(true);
    s.setReady(false);
    s.setPost((r) => { expect(r.path).toBe("/v1/workflow-runs/same/open"); s.setOpened(true); return json(s.snapshot("same")); });
    fireEvent.click(button("打开工作流控制"));
    await waitFor(() => expect(s.streams).toHaveLength(2));
    expect(button("恢复工作流").disabled).toBe(true);
    expect(s.requests.filter((r) => r.method === "POST").map((r) => r.body)).toEqual(["{}"]);
    await act(async () => s.ready());
    await waitFor(() => expect(button("恢复工作流").disabled).toBe(false));
    expect(writes(s)).toHaveLength(0);
  });

  it("renders only DTO node identities and selected result as text, never internal node payloads or HTML", async () => {
    const s = service(false);
    const attack = '<img src=x onerror="window.workflowAttack=1"><script>bad</script>';
    Object.assign(s.snapshot("same"), { result: { text: attack }, privateBinding: "hidden-binding" });
    Object.assign(s.snapshot("same").workflowNodes[0], { result: "hidden-node-result", error: "hidden-node-error", arguments: "hidden-args" });
    s.snapshot("same").interactions[0].question = attack;
    const { container } = render(<WorkflowView client={s.client} rid="same" />);
    await screen.findByText("review-node");
    await screen.findByRole("region", { name: "工作流结果" });
    expect(container.textContent).toContain(attack);
    for (const text of ["hidden-binding", "hidden-node-result", "hidden-node-error", "hidden-args"]) expect(container.textContent).not.toContain(text);
    expect(container.querySelector("img,script")).toBeNull();
    expect(screen.queryByRole("textbox", { name: "输入消息" })).toBeNull();
  });

  it("drops old-instance approval controls and never auto-resumes after a decision", async () => {
    const s = service();
    await ready(s);
    fireEvent.click(button("批准一次"));
    await waitFor(() => expect(writes(s)).toHaveLength(1));
    expect(JSON.parse(writes(s)[0].body)).toEqual({ decision: "allowed-once", expectedRevision: 7, instanceId: "instance-1" });
    expect(writes(s).every((r) => r.path.endsWith("/responses"))).toBe(true);
    Object.assign(s.snapshot("same"), { instanceId: "instance-2", revision: 8 });
    await act(async () => s.event());
    await waitFor(() => expect(screen.queryByRole("button", { name: "批准一次" })).toBeNull());
    expect(writes(s)).toHaveLength(1);
  });

  it("discards a same-revision old-instance watcher snapshot after a newer observation while retaining the unknown approval request", async () => {
    const s = service();
    const view = await ready(s);
    s.setPost((r) => {
      if (r.path.endsWith("/responses")) throw new TypeError("offline unknown approval");
      return json({ operationId: "op1", state: "accepted", target: "same", acceptedCommit: 8, scope: "durable" }, 202);
    });
    fireEvent.click(button("批准一次"));
    await screen.findByText("网络连接失败。");
    const original = writes(s)[0];
    const oldSnapshot = structuredClone(s.snapshot("same"));
    const slow = deferred<Response>();
    const operation = deferred<Response>();
    let delayed = false;
    s.setGet((r) => {
      if (r.path.includes("/operations/")) return operation.promise;
      if (r.path.endsWith("/snapshot") && !delayed) { delayed = true; return slow.promise; }
      return undefined;
    });
    fireEvent.click(button("恢复工作流"));
    await waitFor(() => expect(delayed).toBe(true));
    Object.assign(s.snapshot("same"), { instanceId: "instance-2", interactions: [], definitionName: "new-instance-same-revision" });
    fireEvent.click(button("刷新工作流"));
    await screen.findByText("new-instance-same-revision");
    expect(screen.queryByText("工作流审批")).toBeNull();
    expect(button("重试审批").disabled).toBe(false);
    await act(async () => slow.resolve(json(oldSnapshot)));
    expect(screen.queryByText("工作流审批")).toBeNull();
    expect(screen.queryByRole("button", { name: "批准一次" })).toBeNull();
    expect(screen.getByText("new-instance-same-revision")).toBeTruthy();
    expect(writes(s)).toHaveLength(2);
    fireEvent.click(button("重试审批"));
    await waitFor(() => expect(writes(s)).toHaveLength(3));
    expect(writes(s)[2].key).toBe(original.key); expect(writes(s)[2].body).toBe(original.body);
    fireEvent.click(button("放弃审批"));
    expect(screen.queryByRole("button", { name: "重试审批" })).toBeNull();
    view.unmount();
  });

  it("shows cancelling until this run snapshot confirms actual stop", async () => {
    const s = service();
    Object.assign(s.snapshot("same"), { state: "running", executionStopped: false, canResume: false });
    render(<WorkflowView client={s.client} rid="same" />);
    await waitFor(() => expect(button("取消工作流").disabled).toBe(false));
    s.setPost(() => { Object.assign(s.snapshot("same"), { state: "cancelling", revision: 8 }); return json({ operationId: "op1", state: "accepted", target: "same", acceptedCommit: 8, scope: "durable" }, 202); });
    fireEvent.click(button("取消工作流"));
    await screen.findByText("取消中");
    expect(screen.queryByText("已取消")).toBeNull();
    expect(screen.queryByText("执行已停止")).toBeNull();
    expect(writes(s).map((r) => r.path)).toEqual(["/v1/workflow-runs/same/cancel"]);
    expect(s.requests.some((r) => r.path.includes("/operations/op1"))).toBe(true);
    Object.assign(s.snapshot("same"), { state: "cancelled", executionStopped: true, revision: 9 });
    await act(async () => s.event());
    await screen.findByText("已取消");
    await screen.findByText("执行已停止");
  });

  it("rejects cross-resource event types and scopes before refreshing this run", async () => {
    const s = service();
    await ready(s);
    const before = s.requests.filter((r) => r.path.endsWith("/snapshot")).length;
    Object.assign(s.snapshot("same"), { definitionName: "must-not-load" });
    await act(async () => {
      s.event("same", "trace.state_changed", { sessionId: "same" });
      s.event("same", "workflow.state_changed", { workflowRunId: "other" });
      s.event("same", "workflow.state_changed", { workflowRunId: "same", sessionId: "same" });
      await new Promise((resolve) => setTimeout(resolve, 180));
    });
    expect(s.requests.filter((r) => r.path.endsWith("/snapshot"))).toHaveLength(before);
    expect(screen.queryByText("must-not-load")).toBeNull();
    await act(async () => s.event());
    await screen.findByText("must-not-load");
  });

  for (const replacement of ["run", "client"] as const) {
    it(`aborts and ignores late open and GET responses after ${replacement} replacement`, async () => {
      const s = service(false);
      const open = deferred<Response>();
      s.setPost(() => open.promise);
      const view = render(<WorkflowView client={s.client} rid="same" />);
      await screen.findByText("只读浏览（无运行中的写入者）");
      fireEvent.click(button("打开工作流控制"));
      const old = s.requests.find((r) => r.method === "POST")!;
      view.rerender(<WorkflowView client={replacement === "client" ? new Client("") : s.client} rid={replacement === "run" ? "other" : "same"} />);
      await waitFor(() => expect(s.streams).toHaveLength(2));
      await waitFor(() => expect(button("打开工作流控制").disabled).toBe(false));
      await act(async () => open.resolve(json({ ...initial("same"), definitionName: "late-old-open", revision: 99 })));
      expect(old.signal?.aborted).toBe(true);
      expect(screen.queryByText("late-old-open")).toBeNull();
      expect(writes(s)).toHaveLength(0);
    });
  }

  for (const handoff of [btoa(JSON.stringify([1, "same", "9007199254740993"])).replace(/=+$/, ""), cursor("other"), cursor("same", "7")]) {
    it(`keeps controls unwritable for wrong ready ${handoff} with zero writes and extra GETs`, async () => {
      const s = service(); s.setReady(false);
      render(<WorkflowView client={s.client} rid="same" />);
      await waitFor(() => expect(s.streams).toHaveLength(1));
      const before = s.requests.length;
      await act(async () => s.streams[0].send("ready", { live: true, handoff }));
      await screen.findByText("请求参数无效。");
      expect(button("恢复工作流").disabled).toBe(true);
      expect(button("批准一次").disabled).toBe(true);
      fireEvent.click(button("恢复工作流")); fireEvent.click(button("批准一次"));
      expect(writes(s)).toHaveLength(0); expect(s.requests).toHaveLength(before);
    });
  }
  it("ignores wrong durable event ids without a new snapshot or state application", async () => {
    const s = service(); await ready(s);
    const before = s.requests.length;
    Object.assign(s.snapshot("same"), { definitionName: "must-not-apply-wrong-id" });
    await act(async () => {
      s.streams[0].send("event", { type: "workflow.state_changed", scope: { workflowRunId: "same" } }, cursor("other", "9007199254740994"));
      await new Promise((resolve) => setTimeout(resolve, 180));
    });
    expect(s.requests).toHaveLength(before); expect(writes(s)).toHaveLength(0);
    expect(screen.queryByText("must-not-apply-wrong-id")).toBeNull();
  });

  it("refuses an unsafe snapshot revision and mismatched run identity without any write", async () => {
    const s = service();
    s.setGet((r) => r.path.endsWith("/snapshot") ? json({ ...initial("other"), revision: Number.MAX_SAFE_INTEGER + 1 }) : undefined);
    render(<WorkflowView client={s.client} rid="same" />);
    await screen.findByText("请求参数无效。");
    expect(writes(s)).toHaveLength(0);
    expect(screen.queryByRole("button", { name: "批准一次" })).toBeNull();
  });
});

const cases = [
  { kind: "pause", label: "暂停", first: "暂停工作流" }, { kind: "cancel", label: "取消", first: "取消工作流" },
  { kind: "resume", label: "恢复", first: "恢复工作流" }, { kind: "approval", label: "审批", first: "批准一次" },
] as const;
describe("workflow unconfirmed receipts", () => {
  for (const c of cases) for (const failure of ["network", "503", "malformed"] as const) {
    it(`keeps original ${c.kind} key/body/revision after ${failure} despite eligibility loss`, async () => {
      const s = service();
      if (c.kind === "pause" || c.kind === "cancel") Object.assign(s.snapshot("same"), { state: "running", executionStopped: false });
      let first: Request | undefined;
      let accepted = 0;
      s.setPost((r) => {
        if (first) {
          expect({ path: r.path, key: r.key, body: r.body }).toEqual({ path: first.path, key: first.key, body: first.body });
          return json({ operationId: "op1", state: "accepted", target: c.kind === "approval" ? "approval1" : "same", acceptedCommit: c.kind === "approval" ? 0 : 8, scope: c.kind === "approval" ? "instance" : "durable", ...(c.kind === "approval" ? { instanceId: "instance-1" } : {}) }, 202);
        }
        first = r; accepted++;
        if (failure === "network") throw new TypeError("offline accepted receipt lost");
        if (failure === "malformed") return new Response("{", { status: 202 });
        return json({ error: { code: "storage_unavailable", message: "hidden-error" } }, 503);
      });
      const view = render(<WorkflowView client={s.client} rid="same" />);
      await waitFor(() => expect(button(c.first).disabled).toBe(false));
      await act(async () => { fireEvent.click(button(c.first)); fireEvent.click(button(c.first)); });
      await screen.findByText(failure === "503" ? "存储暂不可用。" : "网络连接失败。");
      expect(writes(s)).toHaveLength(1);
      expect(JSON.parse(first!.body)).toEqual(c.kind === "approval" ? { decision: "allowed-once", expectedRevision: 7, instanceId: "instance-1" } : { expectedRevision: 7 });
      Object.assign(s.snapshot("same"), { revision: 9, state: "completed", canResume: false, interactions: [] });
      await act(async () => s.event());
      await screen.findByText("已完成");
      expect(button("重试" + c.label).disabled).toBe(false);
      view.rerender(<WorkflowView client={s.client} rid="other" />);
      await screen.findByRole("heading", { name: "other" });
      expect(screen.queryByRole("button", { name: "重试" + c.label })).toBeNull();
      view.rerender(<WorkflowView client={s.client} rid="same" />);
      await waitFor(() => expect(button("重试" + c.label).disabled).toBe(false));
      fireEvent.click(button("重试" + c.label));
      await waitFor(() => expect(writes(s)).toHaveLength(2));
      expect(accepted).toBe(1);
      await waitFor(() => expect(screen.queryByRole("button", { name: "重试" + c.label })).toBeNull());
    });
  }

  for (const c of cases) for (const defect of ["503-invalid", "incomplete", "operation-type", "operation-empty", "state-type", "state-invalid", "commit-type", "commit-negative", "commit-unsafe", "scope", "target", "commit-scope", "instance"] as const) {
    it(`retains original ${c.kind} request for uncertain ${defect} receipt`, async () => {
      const s = service();
      if (c.kind === "pause" || c.kind === "cancel") Object.assign(s.snapshot("same"), { state: "running", executionStopped: false });
      const valid = { operationId: "op1", state: "accepted", target: c.kind === "approval" ? "approval1" : "same", acceptedCommit: c.kind === "approval" ? 0 : 8, scope: c.kind === "approval" ? "instance" : "durable", ...(c.kind === "approval" ? { instanceId: "instance-1" } : {}) };
      let count = 0;
      s.setPost(() => {
        if (++count > 1) return json(valid, 202);
        if (defect === "503-invalid") return json({ error: { code: "invalid_argument" } }, 503);
        const broken: Record<string, unknown> = { ...valid };
        if (defect === "incomplete") { delete broken.state; delete broken.acceptedCommit; }
        if (defect === "operation-type") broken.operationId = 42;
        if (defect === "operation-empty") broken.operationId = "";
        if (defect === "state-type") broken.state = true;
        if (defect === "state-invalid") broken.state = "arbitrary-state";
        if (defect === "commit-type") broken.acceptedCommit = "8";
        if (defect === "commit-negative") broken.acceptedCommit = -1;
        if (defect === "commit-unsafe") broken.acceptedCommit = Number.MAX_SAFE_INTEGER + 1;
        if (defect === "scope") broken.scope = "wrong";
        if (defect === "target") broken.target = "other";
        if (defect === "commit-scope") broken.acceptedCommit = c.kind === "approval" ? 8 : 0;
        if (defect === "instance") broken.instanceId = "other-instance";
        return json(broken, 202);
      });
      render(<WorkflowView client={s.client} rid="same" />);
      await waitFor(() => expect(button(c.first).disabled).toBe(false));
      fireEvent.click(button(c.first));
      await screen.findByText(defect === "503-invalid" ? "请求参数无效。" : "服务资源暂不可用。");
      expect(writes(s)).toHaveLength(1);
      const original = writes(s)[0];
      expect(button("重试" + c.label).disabled).toBe(false);
      expect(s.requests.filter((r) => r.path.includes("/operations/"))).toHaveLength(0);
      fireEvent.click(button("重试" + c.label));
      await waitFor(() => expect(writes(s)).toHaveLength(2));
      expect(writes(s)[1].key).toBe(original.key); expect(writes(s)[1].body).toBe(original.body);
      await waitFor(() => expect(screen.queryByRole("button", { name: "重试" + c.label })).toBeNull());
    });
  }

  it("keeps the first decision until explicit abandon and clears it after a definite refusal", async () => {
    const s = service();
    let count = 0;
    s.setPost(() => { if (++count === 1) throw new TypeError("offline lost"); return json({ error: { code: "state_conflict" } }, 409); });
    await ready(s);
    fireEvent.click(button("批准一次"));
    await screen.findByRole("button", { name: "放弃审批" });
    expect(button("拒绝").disabled).toBe(true);
    fireEvent.click(button("拒绝"));
    expect(writes(s)).toHaveLength(1);
    fireEvent.click(button("放弃审批"));
    fireEvent.click(button("拒绝"));
    await screen.findByText("状态已变化，请刷新后重试。");
    expect(writes(s)).toHaveLength(2);
    expect(writes(s)[1].key).not.toBe(writes(s)[0].key);
    expect(JSON.parse(writes(s)[1].body).decision).toBe("rejected");
    expect(screen.queryByRole("button", { name: "重试审批" })).toBeNull();
  });

  it("retains an old Client receipt separately and does not apply its late failure to a replacement Client", async () => {
    const s = service();
    const receipt = deferred<Response>();
    s.setPost(() => receipt.promise);
    const view = await ready(s);
    fireEvent.click(button("恢复工作流"));
    await screen.findByRole("button", { name: "重试恢复" });
    const old = writes(s)[0];
    view.rerender(<WorkflowView client={new Client("")} rid="same" />);
    await waitFor(() => expect(button("恢复工作流").disabled).toBe(false));
    expect(screen.queryByRole("button", { name: "重试恢复" })).toBeNull();
    await act(async () => receipt.reject(new TypeError("late private failure")));
    expect(screen.queryByRole("alert")).toBeNull();
    view.rerender(<WorkflowView client={s.client} rid="same" />);
    await waitFor(() => expect(button("重试恢复").disabled).toBe(false));
    s.setPost(() => json({ operationId: "op1", state: "accepted", target: "same", acceptedCommit: 8, scope: "durable" }, 202));
    fireEvent.click(button("重试恢复"));
    await waitFor(() => expect(writes(s)).toHaveLength(2));
    expect(writes(s)[1].key).toBe(old.key);
    expect(writes(s)[1].body).toBe(old.body);
  });
});
