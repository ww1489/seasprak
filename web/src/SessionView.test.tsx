import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import SessionView from "./SessionView";
import { Client, type Snapshot, type TraceInfo } from "./api/client";

const encoder = new TextEncoder();
const trace = (traceId: string, canResume = true): TraceInfo => ({ traceId, state: "paused", kind: "prompt", targetAgent: "main", settled: false, hold: false, canResume });
const initialSnapshot = (sid: string): Snapshot => ({
  sessionId: sid, revision: 7, cursor: "cursor-7", instanceId: "instance-1",
  traces: [trace("t1"), { ...trace("held", false), state: "queued", hold: true }],
  pendingReconciliations: [{ traceId: "t1", invocationId: "inv1", toolCallId: "call1", observationId: "obs1", observationVersion: 2 }],
});

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (err: unknown) => void;
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
type RequestRecord = { method: string; path: string; key: string; body: string; raw?: BodyInit | null; contentType: string; signal?: AbortSignal | null };

// Real Client + SessionView + syncSession; only fetch is replaced. The service
// controls ready frames and the moment a business receipt is lost or returned.
function service(live = false, approval = false) {
  const requests: RequestRecord[] = [];
  const snapshots = new Map<string, Snapshot>();
  const streams: { sid: string; controller: ReadableStreamDefaultController<Uint8Array>; signal?: AbortSignal | null }[] = [];
  let opened = live;
  let autoReady = true;
  let posts: ((request: RequestRecord) => Response | Promise<Response>) | undefined;
  let gets: ((request: RequestRecord) => Response | Promise<Response> | undefined) | undefined;
  const getSnapshot = (sid: string) => {
    if (!snapshots.has(sid)) snapshots.set(sid, initialSnapshot(sid));
    return snapshots.get(sid)!;
  };
  const frame = (event: string, data: unknown) => encoder.encode(`event: ${event}\ndata: ${JSON.stringify(data)}\n\n`);
  vi.stubGlobal("fetch", vi.fn(async (path: string, opts: RequestInit = {}) => {
    const r = { method: opts.method ?? "GET", path, key: new Headers(opts.headers).get("Idempotency-Key") ?? "", body: String(opts.body ?? ""), raw: opts.body, contentType: new Headers(opts.headers).get("Content-Type") ?? "", signal: opts.signal };
    requests.push(r);
    const sid = decodeURIComponent(path.split("/")[3] ?? "");
    const getResponse = r.method === "GET" ? gets?.(r) : undefined;
    if (getResponse !== undefined) return getResponse;
    if (r.method === "POST") {
      if (posts) return posts(r);
      if (path.endsWith("/open")) { opened = true; return json(getSnapshot(sid)); }
      return json({ operationId: "op1" }, 202);
    }
    if (path.endsWith("/snapshot")) return json(getSnapshot(sid));
    if (path.endsWith("/capabilities")) return json({ agents: [], tools: [], unavailable: [] });
    if (path.endsWith("/workflows")) return json({ workflows: [] });
    if (path.endsWith("/branches")) return json({ branches: [] });
    if (path.endsWith("/render")) {
      const surfaceId = "session:" + sid;
      const components: unknown[] = [{ id: "label", component: { Text: { value: "surface:" + sid } } }];
      if (approval) components.push({ id: "approval", component: { Approval: { interactionId: "approval1", traceId: "t1", question: "测试审批", options: ["allowed-once", "rejected"], instanceId: "instance-1" } } });
      components.push({ id: "root", component: { Column: { children: ["label", ...(approval ? ["approval"] : [])] } } });
      const lines = [JSON.stringify({ beginRendering: { surfaceId, root: "root" } }), JSON.stringify({ surfaceUpdate: { surfaceId, components } })];
      return new Response(lines.join("\n"), { headers: { "X-Session-Cursor": getSnapshot(sid).cursor } });
    }
    if (path.includes("/ui/events")) {
      const stream = new ReadableStream<Uint8Array>({
        start(controller) {
          streams.push({ sid, controller, signal: opts.signal });
          if (autoReady) controller.enqueue(frame("ready", { live: opened }));
          if (!opened) { controller.enqueue(frame("end", {})); controller.close(); }
          else opts.signal?.addEventListener("abort", () => controller.close(), { once: true });
        },
      });
      return new Response(stream, { headers: { "Content-Type": "text/event-stream" } });
    }
    throw new Error("unexpected test request: " + path);
  }));
  return {
    client: new Client(""), requests, snapshots, streams, getSnapshot,
    setPost(handler: typeof posts) { posts = handler; },
    setGet(handler: typeof gets) { gets = handler; },
    setReady(value: boolean) { autoReady = value; },
    setOpened(value: boolean) { opened = value; },
    ready() {
      for (const stream of streams) if (!stream.signal?.aborted) stream.controller.enqueue(frame("ready", { live: true }));
    },
    refresh(sid = "s1", hideApproval = false) {
      for (const stream of streams) if (stream.sid === sid && !stream.signal?.aborted) {
        const components: unknown[] = [{ id: "label", component: { Text: { value: "surface:" + sid + ":updated" } } }];
        if (hideApproval) components.push({ id: "root", component: { Column: { children: ["label"] } } });
        stream.controller.enqueue(frame("a2ui", { surfaceUpdate: { surfaceId: "session:" + sid, components } }));
      }
    },
  };
}

const button = (name: string) => screen.getByRole<HTMLButtonElement>("button", { name });
const businessPosts = (s: ReturnType<typeof service>) => s.requests.filter((r) => r.method === "POST" && !r.path.endsWith("/open"));

describe("code-only targets", () => {
  it("does not fetch workflow definitions or offer workflow capability targets in a session", async () => {
    const s = service(true);
    s.setGet((r) => r.path.endsWith("/capabilities") ? json({ agents: [
      { name: "normal", version: "v1", kind: "agent" }, { name: "legacy-flow", version: "v1", kind: "workflow" },
    ], tools: [], unavailable: [] }) : undefined);
    render(<SessionView client={s.client} sid="s1" />);
    await screen.findByRole("option", { name: "normal" });
    expect(screen.queryByRole("option", { name: "legacy-flow" })).toBeNull();
    expect(screen.queryByRole("form", { name: "工作流参数" })).toBeNull();
    expect(s.requests.some((r) => r.path.includes("workflows"))).toBe(false);
    fireEvent.change(screen.getByRole("combobox", { name: "目标 Agent" }), { target: { value: "normal" } });
    fireEvent.change(screen.getByRole("textbox", { name: "输入消息" }), { target: { value: "ordinary prompt" } });
    await waitFor(() => expect(button("发送").disabled).toBe(false));
    fireEvent.click(button("发送"));
    await waitFor(() => expect(businessPosts(s)).toHaveLength(1));
    expect(JSON.parse(businessPosts(s)[0].body)).toEqual({ kind: "prompt", targetAgent: "normal", content: [{ type: "text", text: "ordinary prompt" }] });
  });
});

async function readyPage(s: ReturnType<typeof service>) {
  render(<SessionView client={s.client} sid="s1" />);
  await waitFor(() => expect(button("恢复").disabled).toBe(false));
}

afterEach(() => { cleanup(); vi.useRealTimers(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

describe("explicit session control", () => {
  it("keeps ordinary browsing read-only and opens control only on a user click", async () => {
    const s = service(false, true);
    render(<SessionView client={s.client} sid="s1" />);
    await screen.findByText("只读浏览（无运行中的写入者）");
    await screen.findByRole("button", { name: "恢复" });
    expect(s.requests.every((r) => r.method === "GET")).toBe(true);
    expect(button("恢复").disabled).toBe(true);
    expect(button("继续队列").disabled).toBe(true);
    expect(button("批准一次").disabled).toBe(true);
    fireEvent.change(screen.getByRole("textbox", { name: "输入消息" }), { target: { value: "hello" } });
    expect(button("发送").disabled).toBe(true);

    s.setReady(false);
    s.getSnapshot("s1").revision = 9;
    s.setPost((r) => { expect(r.path).toBe("/v1/sessions/s1/open"); s.setOpened(true); return json(s.getSnapshot("s1")); });
    fireEvent.click(button("打开会话控制"));
    await waitFor(() => expect(s.requests.filter((r) => r.path.endsWith("/render"))).toHaveLength(2));
    expect(s.requests.find((r) => r.method === "POST")?.body).toBe("{}");
    expect(button("恢复").disabled).toBe(true);
    expect(button("继续队列").disabled).toBe(true);
    expect(businessPosts(s)).toHaveLength(0);
    await act(async () => s.ready());
    await waitFor(() => expect(button("恢复").disabled).toBe(false));
    expect(businessPosts(s)).toHaveLength(0);

    s.setPost(() => json({ operationId: "op1" }, 202));
    fireEvent.click(button("恢复"));
    await waitFor(() => expect(businessPosts(s)).toHaveLength(1));
    expect(JSON.parse(businessPosts(s)[0].body)).toEqual({ expectedRevision: 9 });
  });

  it("locks opening while pending and keeps browsing read-only after refusal", async () => {
    const s = service(false, true);
    const open = deferred<Response>();
    s.setPost(() => open.promise);
    render(<SessionView client={s.client} sid="s1" />);
    await screen.findByText("只读浏览（无运行中的写入者）");
    await screen.findByRole("button", { name: "恢复" });
    await act(async () => { fireEvent.click(button("打开会话控制")); fireEvent.click(button("打开会话控制")); });
    expect(s.requests.filter((r) => r.method === "POST")).toHaveLength(1);
    expect(button("打开会话控制").disabled).toBe(true);
    await act(async () => open.resolve(json({ error: { code: "state_conflict" } }, 409)));
    await screen.findByText("状态已变化，请刷新后重试。");
    expect(button("打开会话控制").disabled).toBe(false);
    expect(button("恢复").disabled).toBe(true);
    expect(button("批准一次").disabled).toBe(true);
    expect(businessPosts(s)).toHaveLength(0);
  });

  for (const outcome of ["snapshot", "error"] as const) {
    it(`resets a pending open after replacing Client for the same sid and isolates its late ${outcome}`, async () => {
      const s = service(false, true);
      const oldOpen = deferred<Response>();
      s.setPost(() => oldOpen.promise);
      const view = render(<SessionView client={s.client} sid="s1" />);
      await screen.findByText("只读浏览（无运行中的写入者）");
      await screen.findByRole("button", { name: "恢复" });
      fireEvent.click(button("打开会话控制"));
      const oldRequest = s.requests.find((r) => r.method === "POST")!;
      const newClient = new Client("");
      view.rerender(<SessionView client={newClient} sid="s1" />);
      await waitFor(() => expect(s.requests.filter((r) => r.path.endsWith("/render"))).toHaveLength(2));
      await waitFor(() => expect(button("打开会话控制").disabled).toBe(false));
      await screen.findByRole("button", { name: "恢复" });
      expect(oldRequest.signal?.aborted).toBe(true);
      expect(button("恢复").disabled).toBe(true);
      expect(button("批准一次").disabled).toBe(true);
      expect(s.requests.filter((r) => r.method === "POST")).toHaveLength(1);
      expect(businessPosts(s)).toHaveLength(0);

      s.setReady(false);
      s.getSnapshot("s1").revision = 9;
      s.setPost(() => { s.setOpened(true); return json(s.getSnapshot("s1")); });
      fireEvent.click(button("打开会话控制"));
      await waitFor(() => expect(s.requests.filter((r) => r.path.endsWith("/render"))).toHaveLength(3));
      expect(button("恢复").disabled).toBe(true);
      await act(async () => s.ready());
      await waitFor(() => expect(button("恢复").disabled).toBe(false));
      await act(async () => {
        if (outcome === "snapshot") oldOpen.resolve(json({ ...initialSnapshot("s1"), revision: 99, traces: [trace("late-old-client")] }));
        else oldOpen.reject(new TypeError("offline test: old Client open failed late"));
      });
      expect(screen.queryByText("late-old-client")).toBeNull();
      expect(screen.queryByRole("alert")).toBeNull();
      expect(button("恢复").disabled).toBe(false);
      expect(businessPosts(s)).toHaveLength(0);
      s.setPost(() => json({ operationId: "new-op" }, 202));
      fireEvent.click(button("恢复"));
      await waitFor(() => expect(businessPosts(s)).toHaveLength(1));
      expect(JSON.parse(businessPosts(s)[0].body)).toEqual({ expectedRevision: 9 });
    });
  }

  it("isolates late GET state after replacing Client for the same sid", async () => {
    const s = service();
    const oldCapabilities = deferred<Response>();
    let capabilities = 0;
    s.setGet((r) => r.path.endsWith("/capabilities") && ++capabilities === 1 ? oldCapabilities.promise : undefined);
    const view = render(<SessionView client={s.client} sid="s1" />);
    await screen.findByText("只读浏览（无运行中的写入者）");
    const oldRequest = s.requests.find((r) => r.path.endsWith("/capabilities"))!;
    view.rerender(<SessionView client={new Client("")} sid="s1" />);
    await waitFor(() => expect(capabilities).toBe(2));
    await screen.findByRole("region", { name: "能力" });
    await act(async () => oldCapabilities.resolve(json({ agents: [{ name: "late-old-agent", version: "v1", kind: "agent" }], tools: [], unavailable: [] })));
    expect(oldRequest.signal?.aborted).toBe(true);
    expect(screen.queryAllByText("late-old-agent")).toHaveLength(0);
    expect(button("打开会话控制").disabled).toBe(false);
    expect(s.requests.every((r) => r.method === "GET")).toBe(true);
  });

  it("isolates a late open response after switching sessions", async () => {
    const s = service();
    const open = deferred<Response>();
    s.setPost(() => open.promise);
    const view = render(<SessionView client={s.client} sid="s1" />);
    await screen.findByText("只读浏览（无运行中的写入者）");
    fireEvent.click(button("打开会话控制"));
    await waitFor(() => expect(s.requests.filter((r) => r.method === "POST")).toHaveLength(1));
    const oldOpen = s.requests.find((r) => r.method === "POST")!;
    view.rerender(<SessionView client={s.client} sid="s2" />);
    await screen.findByText("surface:s2");
    await act(async () => open.resolve(json({ ...initialSnapshot("s1"), revision: 99, traces: [trace("late")] })));
    expect(oldOpen.signal?.aborted).toBe(true);
    expect(screen.queryByText("late")).toBeNull();
    expect(s.requests.filter((r) => r.path.startsWith("/v1/sessions/s2/render"))).toHaveLength(1);
    expect(businessPosts(s)).toHaveLength(0);
  });
});

function loseFirstReceipt(s: ReturnType<typeof service>, failure?: () => Response) {
  const receipts = new Map<string, { body: string; operationId: string }>();
  let accepted = 0;
  s.setPost((r) => {
    const old = receipts.get(r.key);
    if (old) return old.body === r.body ? json({ operationId: old.operationId }, 202) : json({ error: { code: "idempotency_conflict" } }, 409);
    accepted++;
    receipts.set(r.key, { body: r.body, operationId: "op" + accepted });
    if (failure) return failure();
    throw new TypeError("offline test: accepted response lost");
  });
  return { get accepted() { return accepted; } };
}

const controlCases = [
  { kind: "resume", label: "恢复", first: "恢复" },
  { kind: "reconcile", label: "核对", first: "提交核对" },
  { kind: "approval", label: "审批", first: "批准一次" },
] as const;

async function beginControl(s: ReturnType<typeof service>, kind: typeof controlCases[number]["kind"], first: string) {
  await readyPage(s);
  if (kind === "reconcile") fireEvent.change(screen.getByRole("textbox", { name: "核对证据引用" }), { target: { value: "  evidence-original  " } });
  fireEvent.click(button(first));
  await screen.findByText("网络连接失败。");
  expect(businessPosts(s)).toHaveLength(1);
}

async function refreshSnapshot(s: ReturnType<typeof service>, hideApproval = false) {
  const before = s.requests.filter((r) => r.path.endsWith("/snapshot")).length;
  await act(async () => s.refresh("s1", hideApproval));
  await waitFor(() => expect(s.requests.filter((r) => r.path.endsWith("/snapshot")).length).toBeGreaterThan(before));
}

const receiptFailures = [
  { name: "503", message: "存储暂不可用。", response: () => json({ error: { code: "storage_unavailable" } }, 503) },
  { name: "202 malformed JSON", message: "网络连接失败。", response: () => new Response("{", { status: 202, headers: { "Content-Type": "application/json" } }) },
  { name: "202 body read failure", message: "网络连接失败。", response: () => new Response(new ReadableStream<Uint8Array>({
    start(controller) { controller.error(new TypeError("offline test: accepted receipt body lost")); },
  }), { status: 202, headers: { "Content-Type": "application/json" } }) },
];

describe("unconfirmed control receipts", () => {
  for (const c of controlCases) {
    for (const failure of receiptFailures) {
      it(`retries accepted ${c.kind} after ${failure.name} with its original request despite lost eligibility`, async () => {
        const s = service(true, c.kind === "approval");
        const server = loseFirstReceipt(s, failure.response);
        await readyPage(s);
        if (c.kind === "reconcile") fireEvent.change(screen.getByRole("textbox", { name: "核对证据引用" }), { target: { value: "  evidence-original  " } });
        fireEvent.click(button(c.first));
        await screen.findByText(failure.message);
        expect(businessPosts(s)).toHaveLength(1);
        expect(server.accepted).toBe(1);
        const first = businessPosts(s)[0];
        expect(first.key).not.toBe("");
        expect(JSON.parse(first.body)).toEqual(c.kind === "resume" ? { expectedRevision: 7 } : c.kind === "approval"
          ? { decision: "allowed-once", expectedRevision: 7, instanceId: "instance-1" }
          : { invocationId: "inv1", toolCallId: "call1", observationId: "obs1", observationVersion: 2, evidenceRef: "evidence-original", expectedRevision: 7 });
        Object.assign(s.getSnapshot("s1"), { revision: 8, traces: [], pendingReconciliations: [] });
        await refreshSnapshot(s, true);
        await waitFor(() => expect(screen.queryByRole("button", { name: c.first })).toBeNull());
        expect(button("重试" + c.label).disabled).toBe(false);
        expect(button("放弃" + c.label).disabled).toBe(false);
        expect(businessPosts(s)).toHaveLength(1);
        fireEvent.click(button("重试" + c.label));
        await waitFor(() => expect(businessPosts(s)).toHaveLength(2));
        const second = businessPosts(s)[1];
        expect({ path: second.path, key: second.key, body: second.body }).toEqual({ path: first.path, key: first.key, body: first.body });
        expect(server.accepted).toBe(1);
        await waitFor(() => expect(screen.queryByRole("button", { name: "重试" + c.label })).toBeNull());
      });
    }

    it(`retries ${c.kind} with its original key and full body after revision changes`, async () => {
      const s = service(true, c.kind === "approval");
      const server = loseFirstReceipt(s);
      await beginControl(s, c.kind, c.first);
      const first = businessPosts(s)[0];
      expect(JSON.parse(first.body)).toEqual(c.kind === "resume" ? { expectedRevision: 7 } : c.kind === "approval"
        ? { decision: "allowed-once", expectedRevision: 7, instanceId: "instance-1" }
        : { invocationId: "inv1", toolCallId: "call1", observationId: "obs1", observationVersion: 2, evidenceRef: "evidence-original", expectedRevision: 7 });
      s.getSnapshot("s1").revision = 8;
      await refreshSnapshot(s);
      fireEvent.click(button(c.kind === "resume" ? c.first : "重试" + c.label));
      await waitFor(() => expect(businessPosts(s)).toHaveLength(2));
      const second = businessPosts(s)[1];
      expect({ key: second.key, body: second.body }).toEqual({ key: first.key, body: first.body });
      expect(server.accepted).toBe(1);
      expect(screen.queryByRole("button", { name: "重试" + c.label })).toBeNull();
    });

    it(`keeps explicit retry and abandon for ${c.kind} when eligibility disappears`, async () => {
      const s = service(true, c.kind === "approval");
      const server = loseFirstReceipt(s);
      await beginControl(s, c.kind, c.first);
      const first = businessPosts(s)[0];
      Object.assign(s.getSnapshot("s1"), { revision: 8, traces: [], pendingReconciliations: [] });
      await refreshSnapshot(s, true);
      expect(screen.queryByRole("button", { name: c.first })).toBeNull();
      expect(button("放弃" + c.label).disabled).toBe(false);
      expect(businessPosts(s)).toHaveLength(1);
      fireEvent.click(button("重试" + c.label));
      await waitFor(() => expect(businessPosts(s)).toHaveLength(2));
      expect(businessPosts(s)[1].key).toBe(first.key);
      expect(businessPosts(s)[1].body).toBe(first.body);
      expect(server.accepted).toBe(1);
    });
  }

  it("isolates a late control failure after replacing Client for the same sid and retains its original request", async () => {
    const s = service(true);
    const oldReceipt = deferred<Response>();
    const newReceipt = deferred<Response>();
    s.setPost(() => oldReceipt.promise);
    const view = render(<SessionView client={s.client} sid="s1" />);
    await waitFor(() => expect(button("恢复").disabled).toBe(false));
    fireEvent.click(button("恢复"));
    await screen.findByRole("button", { name: "重试恢复" });
    const oldRequest = businessPosts(s)[0];
    view.rerender(<SessionView client={new Client("")} sid="s1" />);
    await waitFor(() => expect(s.requests.filter((r) => r.path.endsWith("/render"))).toHaveLength(2));
    await waitFor(() => expect(button("恢复").disabled).toBe(false));
    expect(screen.queryByRole("button", { name: "重试恢复" })).toBeNull();
    expect(businessPosts(s)).toHaveLength(1);
    s.setPost(() => newReceipt.promise);
    fireEvent.click(button("恢复"));
    await screen.findByRole("button", { name: "重试恢复" });
    expect(businessPosts(s)).toHaveLength(2);
    expect(businessPosts(s)[1].key).not.toBe(oldRequest.key);
    await act(async () => oldReceipt.reject(new TypeError("offline test: old Client receipt failed late")));
    expect(screen.queryByRole("alert")).toBeNull();
    expect(button("重试恢复").disabled).toBe(true);
    await act(async () => newReceipt.resolve(json({ operationId: "new-op" }, 202)));
    expect(screen.queryByRole("button", { name: "重试恢复" })).toBeNull();
    view.rerender(<SessionView client={s.client} sid="s1" />);
    await waitFor(() => expect(button("重试恢复").disabled).toBe(false));
    expect(businessPosts(s)).toHaveLength(2);
    s.setPost(() => json({ operationId: "old-op" }, 202));
    fireEvent.click(button("重试恢复"));
    await waitFor(() => expect(businessPosts(s)).toHaveLength(3));
    expect(businessPosts(s)[2].key).toBe(oldRequest.key);
    expect(businessPosts(s)[2].body).toBe(oldRequest.body);
  });

  it("retains the original request when browsing another session and returning", async () => {
    const s = service(true);
    const server = loseFirstReceipt(s);
    const view = render(<SessionView client={s.client} sid="s1" />);
    await waitFor(() => expect(button("恢复").disabled).toBe(false));
    fireEvent.click(button("恢复"));
    await screen.findByText("网络连接失败。");
    const first = businessPosts(s)[0];
    Object.assign(s.getSnapshot("s1"), { revision: 8, traces: [], pendingReconciliations: [] });
    view.rerender(<SessionView client={s.client} sid="s2" />);
    await screen.findByText("surface:s2");
    expect(screen.queryByRole("button", { name: "重试恢复" })).toBeNull();
    view.rerender(<SessionView client={s.client} sid="s1" />);
    await screen.findByText("surface:s1");
    await waitFor(() => expect(button("重试恢复").disabled).toBe(false));
    fireEvent.click(button("重试恢复"));
    await waitFor(() => expect(businessPosts(s)).toHaveLength(2));
    expect(businessPosts(s)[1].key).toBe(first.key);
    expect(businessPosts(s)[1].body).toBe(first.body);
    expect(server.accepted).toBe(1);
  });

  it("applies a late receipt to its original session while preserving its busy lock", async () => {
    const s = service(true);
    const receipt = deferred<Response>();
    s.setPost(() => receipt.promise);
    const view = render(<SessionView client={s.client} sid="s1" />);
    await waitFor(() => expect(button("恢复").disabled).toBe(false));
    fireEvent.click(button("恢复"));
    await screen.findByRole("button", { name: "重试恢复" });
    view.rerender(<SessionView client={s.client} sid="s2" />);
    await screen.findByText("surface:s2");
    expect(screen.queryByRole("button", { name: "重试恢复" })).toBeNull();
    view.rerender(<SessionView client={s.client} sid="s1" />);
    await screen.findByText("surface:s1");
    expect(button("重试恢复").disabled).toBe(true);
    fireEvent.click(button("重试恢复"));
    expect(businessPosts(s)).toHaveLength(1);
    await act(async () => receipt.resolve(json({ operationId: "op1" }, 202)));
    expect(screen.queryByRole("button", { name: "重试恢复" })).toBeNull();
  });

  for (const c of controlCases) {
    it(`serializes simultaneous clicks for one unconfirmed ${c.kind}`, async () => {
      const s = service(true, c.kind === "approval");
      const receipt = deferred<Response>();
      let accepted = 0;
      s.setPost(() => { accepted++; return receipt.promise; });
      await readyPage(s);
      if (c.kind === "reconcile") fireEvent.change(screen.getByRole("textbox", { name: "核对证据引用" }), { target: { value: "evidence-original" } });
      await act(async () => { fireEvent.click(button(c.first)); fireEvent.click(button(c.first)); });
      expect(businessPosts(s)).toHaveLength(1);
      expect(accepted).toBe(1);
      expect(button("重试" + c.label).disabled).toBe(true);
      expect(button("放弃" + c.label).disabled).toBe(true);
      await act(async () => receipt.resolve(json({ operationId: "op1" }, 202)));
    });
  }

  it("allows an explicit new request after a definite 409 refusal", async () => {
    const s = service(true);
    let count = 0;
    s.setPost(() => ++count === 1 ? json({ error: { code: "state_conflict" } }, 409) : json({ operationId: "op1" }, 202));
    await readyPage(s);
    fireEvent.click(button("恢复"));
    await screen.findByText("状态已变化，请刷新后重试。");
    expect(businessPosts(s)).toHaveLength(1);
    const first = businessPosts(s)[0];
    fireEvent.click(button("恢复"));
    await waitFor(() => expect(businessPosts(s)).toHaveLength(2));
    expect(businessPosts(s)[1].key).not.toBe(first.key);
    expect(JSON.parse(businessPosts(s)[1].body)).toEqual({ expectedRevision: 7 });
  });

  it("requires abandoning unconfirmed evidence before sending a different body", async () => {
    const s = service(true);
    loseFirstReceipt(s);
    await beginControl(s, "reconcile", "提交核对");
    const first = businessPosts(s)[0];
    fireEvent.change(screen.getByRole("textbox", { name: "核对证据引用" }), { target: { value: "evidence-new" } });
    fireEvent.click(button("提交核对"));
    expect(businessPosts(s)).toHaveLength(1);
    fireEvent.click(button("重试核对"));
    await waitFor(() => expect(businessPosts(s)).toHaveLength(2));
    expect(businessPosts(s)[1].body).toBe(first.body);
    // Another lost response leaves the original request available to abandon.
    s.setPost(() => { throw new TypeError("offline test: response lost again"); });
    fireEvent.change(screen.getByRole("textbox", { name: "核对证据引用" }), { target: { value: "evidence-original" } });
    fireEvent.click(button("提交核对"));
    await screen.findByRole("button", { name: "放弃核对" });
    fireEvent.click(button("放弃核对"));
    fireEvent.change(screen.getByRole("textbox", { name: "核对证据引用" }), { target: { value: "evidence-new" } });
    fireEvent.click(button("提交核对"));
    await waitFor(() => expect(businessPosts(s)).toHaveLength(4));
    expect(businessPosts(s)[3].key).not.toBe(businessPosts(s)[2].key);
    expect(JSON.parse(businessPosts(s)[3].body).evidenceRef).toBe("evidence-new");
  });

  it("keeps the first approval decision until retry or explicit abandon", async () => {
    const s = service(true, true);
    loseFirstReceipt(s);
    await beginControl(s, "approval", "批准一次");
    expect(button("拒绝").disabled).toBe(true);
    fireEvent.click(button("拒绝"));
    expect(businessPosts(s)).toHaveLength(1);
    fireEvent.click(button("放弃审批"));
    expect(button("拒绝").disabled).toBe(false);
    fireEvent.click(button("拒绝"));
    await waitFor(() => expect(businessPosts(s)).toHaveLength(2));
    expect(businessPosts(s)[1].key).not.toBe(businessPosts(s)[0].key);
    expect(JSON.parse(businessPosts(s)[1].body).decision).toBe("rejected");
  });
});

describe("existing request identity equivalence", () => {
  for (const kind of ["input", "continue"] as const) {
    it(`keeps the existing ${kind} key and body when only snapshot revision changes`, async () => {
      const s = service(true);
      const server = loseFirstReceipt(s);
      await readyPage(s);
      if (kind === "input") fireEvent.change(screen.getByRole("textbox", { name: "输入消息" }), { target: { value: "  same prompt  " } });
      const label = kind === "input" ? "发送" : "继续队列";
      fireEvent.click(button(label));
      await screen.findByText("网络连接失败。");
      const first = businessPosts(s)[0];
      expect(JSON.parse(first.body)).toEqual(kind === "input" ? { kind: "prompt", content: [{ type: "text", text: "same prompt" }] } : { traceIds: ["held"] });
      s.getSnapshot("s1").revision = 8;
      await refreshSnapshot(s);
      fireEvent.click(button(label));
      await waitFor(() => expect(businessPosts(s)).toHaveLength(2));
      expect(businessPosts(s)[1].key).toBe(first.key);
      expect(businessPosts(s)[1].body).toBe(first.body);
      expect(server.accepted).toBe(1);
    });
  }

  it("retries an upload with the same immutable file, media type, name and key", async () => {
    const s = service(true);
    const file = new File(["original bytes"], "original.md", { type: "text/markdown" });
    let storedKey = "";
    let accepted = 0;
    s.setPost((r) => {
      expect(r.path).toBe("/v1/sessions/s1/attachments?name=original.md");
      expect(r.raw).toBe(file);
      expect(r.contentType).toBe("text/markdown");
      if (!storedKey) {
        storedKey = r.key;
        accepted++;
        throw new TypeError("offline test: upload receipt lost");
      }
      expect(r.key).toBe(storedKey);
      return json({ artifactId: "artifact1", mimeType: "text/markdown", name: "original.md", size: file.size }, 201);
    });
    await readyPage(s);
    fireEvent.change(screen.getByLabelText("添加附件"), { target: { files: [file] } });
    await screen.findByRole("button", { name: "重试上传" });
    fireEvent.click(button("重试上传"));
    await waitFor(() => expect(businessPosts(s)).toHaveLength(2));
    expect(accepted).toBe(1);
    await waitFor(() => expect(screen.queryByRole("button", { name: "重试上传" })).toBeNull());
    expect(screen.getByRole("list", { name: "待发送附件" }).textContent).toContain("original.md");
  });
});

const finalReply = (sid: string) => `离线助手回复 ${sid}：结果已完成并核验。`;
function messageLines(sid: string, final: boolean) {
  const surfaceId = "session:" + sid;
  return [
    { beginRendering: { surfaceId, root: "root" } },
    { surfaceUpdate: { surfaceId, components: [
      { id: "user", component: { ChatMessage: { messageId: "user-" + sid, role: "user", status: "final", dataKey: "user-text" } } },
      { id: "assistant", component: { ChatMessage: { messageId: "assistant-" + sid, role: "assistant", status: final ? "final" : "streaming", dataKey: "assistant-text" } } },
      { id: "root", component: { Column: { children: ["user", "assistant"] } } },
    ] } },
    { dataModelUpdate: { surfaceId, contents: [
      { key: "user-text", valueString: "请完整返回：" + finalReply(sid) },
      { key: "assistant-text", valueString: final ? finalReply(sid) : "" },
    ] } },
  ];
}
function messageRender(sid: string, final: boolean, cursor: string) {
  return new Response(messageLines(sid, final).map((line) => JSON.stringify(line)).join("\n"), { headers: { "X-Session-Cursor": cursor } });
}

// Exercise the real fetch client, synchronization, A2UI store and renderer.
// Only transport timing and bytes are controlled; no model or writer is opened.
function messageService() {
  const s = service(true);
  let final = false;
  let renderCount = 0;
  let eventCount = 0;
  let onRender: ((sid: string, count: number) => Response | Promise<Response> | undefined) | undefined;
  let onEvents: ((sid: string, count: number) => Response | Promise<Response> | undefined) | undefined;
  const streams: { sid: string; send: (event: string, data: unknown, id?: string) => void; close: () => void; fail: () => void }[] = [];
  s.setGet((r) => {
    const sid = decodeURIComponent(r.path.split("/")[3]);
    if (r.path.endsWith("/render")) {
      renderCount++;
      return onRender?.(sid, renderCount) ?? messageRender(sid, final, "cursor-" + renderCount);
    }
    if (r.path.includes("/ui/events")) {
      eventCount++;
      const response = onEvents?.(sid, eventCount);
      if (response !== undefined) return response;
      const body = new ReadableStream<Uint8Array>({ start(controller) {
        streams.push({
          sid,
          send: (event, data, id) => controller.enqueue(encoder.encode(`event: ${event}\n${id === undefined ? "" : `id: ${id}\n`}data: ${JSON.stringify(data)}\n\n`)),
          close: () => controller.close(), fail: () => controller.error(new TypeError("offline test: stream interrupted")),
        });
      } });
      return new Response(body);
    }
    return undefined;
  });
  return {
    ...s, streams,
    setFinal() { final = true; },
    setRender(handler: typeof onRender) { onRender = handler; },
    setEvents(handler: typeof onEvents) { onEvents = handler; },
  };
}
const flushObservation = () => act(async () => { await vi.advanceTimersByTimeAsync(0); });
function assertFinalAssistant(sid: string) {
  const assistants = screen.getAllByRole("article", { name: "助手消息" });
  expect(assistants).toHaveLength(1);
  expect(assistants[0].getAttribute("data-message-id")).toBe("assistant-" + sid);
  expect(within(assistants[0]).getByText(finalReply(sid), { exact: true })).toBeTruthy();
  expect(within(assistants[0]).queryByText("生成中", { exact: true })).toBeNull();
  expect(screen.getByRole("article", { name: "用户消息" }).textContent).toBe("请完整返回：" + finalReply(sid));
}

describe("read-only reload observation", () => {
  beforeEach(() => vi.useFakeTimers());

  for (const arrival of ["before render", "between render and subscribe", "after ready"] as const) {
    it(`projects one complete assistant when the final message arrives ${arrival}`, async () => {
      const s = messageService();
      if (arrival === "before render") s.setFinal();
      if (arrival === "between render and subscribe") s.setEvents(() => { s.setFinal(); return undefined; });
      render(<SessionView client={s.client} sid="s1" />);
      await flushObservation();
      expect(s.streams).toHaveLength(1);
      await act(async () => { s.streams[0].send("ready", { live: arrival === "after ready" }); });
      if (arrival === "after ready") {
        expect(screen.getByRole("article", { name: "助手消息" }).getAttribute("data-message-id")).toBe("assistant-s1");
        expect(screen.getByText("生成中", { exact: true })).toBeTruthy();
        s.setFinal();
      }
      await act(async () => {
        if (arrival !== "before render") {
          s.streams[0].send("a2ui", messageLines("s1", true)[1]);
          s.streams[0].send("a2ui", messageLines("s1", true)[2], "cursor-final");
        }
        s.streams[0].send("end", {}); s.streams[0].close();
      });
      await flushObservation();
      assertFinalAssistant("s1");
      expect(screen.getByText("只读浏览（无运行中的写入者）")).toBeTruthy();
      expect(s.requests.filter((r) => r.path.endsWith("/render"))).toHaveLength(1);
      expect(s.requests.filter((r) => r.path.includes("/ui/events")).map((r) => r.path)).toEqual(["/v1/sessions/s1/ui/events?cursor=cursor-1"]);
      expect(s.requests.every((r) => r.method === "GET")).toBe(true);
    });
  }

  for (const location of ["render", "SSE"] as const) {
    for (const failure of ["network", "503"] as const) {
      it(`keeps the view reconnecting through ${location} ${failure} and receives the final assistant without control POSTs`, async () => {
        const s = messageService();
        const first = (_sid: string, count: number) => {
          if (count !== 1) return undefined;
          if (failure === "network") throw new TypeError("offline test: observation unavailable");
          return json({ error: { code: "resource_unavailable" } }, 503);
        };
        if (location === "render") s.setRender(first); else s.setEvents(first);
        render(<SessionView client={s.client} sid="s1" />);
        await flushObservation();
        expect(screen.getByText("重新连接中", { exact: true })).toBeTruthy();
        expect(screen.getByRole("alert").textContent).toBe(failure === "network" ? "网络连接失败。" : "服务资源暂不可用。");
        expect(screen.queryByText("只读浏览（无运行中的写入者）")).toBeNull();
        expect(button("打开会话控制").disabled).toBe(true);
        await act(async () => { await vi.advanceTimersByTimeAsync(499); });
        expect(s.requests.filter((r) => r.path.endsWith("/render"))).toHaveLength(1);
        s.setFinal();
        await act(async () => { await vi.advanceTimersByTimeAsync(1); });
        expect(s.streams).toHaveLength(1);
        await act(async () => { s.streams[0].send("ready", { live: false }); s.streams[0].send("end", {}); s.streams[0].close(); });
        await flushObservation();
        assertFinalAssistant("s1");
        expect(screen.getByText("只读浏览（无运行中的写入者）")).toBeTruthy();
        expect(screen.queryByRole("alert")).toBeNull();
        expect(s.requests.filter((r) => r.path.endsWith("/render"))).toHaveLength(2);
        expect(s.requests.filter((r) => r.path.includes("/ui/events")).map((r) => r.path)).toEqual(location === "render"
          ? ["/v1/sessions/s1/ui/events?cursor=cursor-2"]
          : ["/v1/sessions/s1/ui/events?cursor=cursor-1", "/v1/sessions/s1/ui/events?cursor=cursor-2"]);
        expect(s.requests.every((r) => r.method === "GET")).toBe(true);
      });
    }
  }

  it("replays a half-delivered final group after reconnect without duplicating the assistant", async () => {
    const s = messageService();
    render(<SessionView client={s.client} sid="s1" />);
    await flushObservation();
    await act(async () => {
      s.streams[0].send("ready", { live: true });
      s.streams[0].send("a2ui", messageLines("s1", true)[1]);
    });
    expect(screen.getAllByRole("article", { name: "助手消息" })).toHaveLength(1);
    expect(screen.queryByText(finalReply("s1"), { exact: true })).toBeNull();
    s.setFinal();
    await act(async () => { s.streams[0].fail(); });
    await act(async () => { await vi.advanceTimersByTimeAsync(500); });
    expect(s.streams).toHaveLength(2);
    await act(async () => {
      s.streams[1].send("ready", { live: false });
      for (let replay = 0; replay < 2; replay++) {
        s.streams[1].send("a2ui", messageLines("s1", true)[1]);
        s.streams[1].send("a2ui", messageLines("s1", true)[2], "cursor-final");
      }
      s.streams[1].send("end", {}); s.streams[1].close();
    });
    await flushObservation();
    assertFinalAssistant("s1");
    expect(s.requests.filter((r) => r.path.includes("/ui/events")).map((r) => r.path)).toEqual(["/v1/sessions/s1/ui/events?cursor=cursor-1", "/v1/sessions/s1/ui/events?cursor=cursor-2"]);
    expect(s.requests.every((r) => r.method === "GET")).toBe(true);
  });

  for (const switchTo of ["new client", "new session"] as const) {
    for (const lateAt of ["render", "SSE"] as const) {
      it(`isolates a late ${lateAt} response after switching to a ${switchTo}`, async () => {
        const s = messageService();
        const late = deferred<Response>();
        const delayFirst = (_sid: string, count: number) => count === 1 ? late.promise : undefined;
        if (lateAt === "render") s.setRender(delayFirst); else s.setEvents(delayFirst);
        const view = render(<SessionView client={s.client} sid="s1" />);
        await flushObservation();
        const oldRequest = s.requests.find((r) => lateAt === "render" ? r.path.endsWith("/render") : r.path.includes("/ui/events"))!;
        s.setFinal();
        const sid = switchTo === "new client" ? "s1" : "s2";
        view.rerender(<SessionView client={switchTo === "new client" ? new Client("") : s.client} sid={sid} />);
        await flushObservation();
        expect(oldRequest.signal?.aborted).toBe(true);
        expect(s.streams).toHaveLength(1);
        await act(async () => { s.streams[0].send("ready", { live: true }); });
        await act(async () => {
          if (lateAt === "render") late.resolve(messageRender("s1", true, "cursor-old-99"));
          else late.resolve(new Response(`event: ready\ndata: {"live":false}\n\nevent: a2ui\nid: cursor-old-99\ndata: ${JSON.stringify({ dataModelUpdate: { surfaceId: "session:s1", contents: [{ key: "assistant-text", valueString: "late old assistant" }] } })}\n\nevent: end\ndata: {}\n\n`));
        });
        await flushObservation();
        assertFinalAssistant(sid);
        expect(screen.queryByText("late old assistant")).toBeNull();
        expect(screen.getByText("实时", { exact: true })).toBeTruthy();
        expect(screen.queryByText("只读浏览（无运行中的写入者）")).toBeNull();
        expect(s.requests.filter((r) => r.path.endsWith("/render"))).toHaveLength(2);
        expect(s.requests.filter((r) => r.path.includes("/ui/events")).map((r) => r.path)).toEqual(lateAt === "render"
          ? [`/v1/sessions/${sid}/ui/events?cursor=cursor-2`]
          : ["/v1/sessions/s1/ui/events?cursor=cursor-1", `/v1/sessions/${sid}/ui/events?cursor=cursor-2`]);
        expect(s.requests.every((r) => r.method === "GET")).toBe(true);
      });
    }
  }
});
