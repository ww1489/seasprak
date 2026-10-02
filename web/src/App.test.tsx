import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import App from "./App";
import type { WorkflowSnapshot } from "./api/client";

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const cursor = (rid: string, seq = "7") => btoa(JSON.stringify([2, "workflow", rid, seq])).replaceAll("+", "-").replaceAll("/", "_").replace(/=+$/, "");
const workflowSnapshot = (rid = "same"): WorkflowSnapshot => ({ runId: rid, definitionName: "review", definitionVersion: "v1", state: "paused", revision: 7, cursor: cursor(rid), durableSeq: "7", instanceId: "workflow-instance", executionStopped: true, canResume: true, workflowNodes: [], interactions: [] });
const definition = { name: "review", version: "v1", description: "受信工作流", inputSchema: { type: "object", required: ["title", "count"], properties: { title: { type: "string" }, count: { type: "integer" }, enabled: { type: "boolean" } } } };
type Request = { path: string; method: string; body: string; key: string; signal?: AbortSignal | null };
function deferred<T>() { let resolve!: (v: T) => void; const promise = new Promise<T>((yes) => { resolve = yes; }); return { promise, resolve }; }
function service() {
  const requests: Request[] = [];
  let post: ((r: Request) => Response | Promise<Response>) | undefined;
  let get: ((r: Request) => Response | Promise<Response> | undefined) | undefined;
  let definitions: unknown[] = [definition, { ...definition, version: "v2" }];
  const streams: { path: string; signal?: AbortSignal | null }[] = [];
  vi.stubGlobal("fetch", vi.fn(async (path: string, opts: RequestInit = {}) => {
    const r: Request = { path, method: opts.method ?? "GET", body: String(opts.body ?? ""), key: new Headers(opts.headers).get("Idempotency-Key") ?? "", signal: opts.signal };
    requests.push(r);
    const custom = r.method === "GET" ? get?.(r) : undefined;
    if (custom !== undefined) return custom;
    if (r.method === "POST") {
      if (post) return post(r);
      if (path === "/v1/workflow-runs") return json(workflowSnapshot("new-run"), 202);
      if (path === "/v1/sessions") return json({ sessionId: "new-session", revision: 1, cursor: "code-cursor:new-session" }, 202);
      if (path.startsWith("/v1/workflow-runs/")) return json({ operationId: "workflow-op", state: "accepted", target: "same", acceptedCommit: 8, scope: "durable" }, 202);
      return json({ operationId: "code-op" }, 202);
    }
    if (path === "/v1/sessions") return json({ sessions: [{ sessionId: "same", available: true }] });
    if (path === "/v1/workflows") return json({ workflows: definitions });
    if (path === "/v1/workflow-runs") return json({ runs: [{ runId: "same", definitionName: "review", definitionVersion: "v1", state: "paused", available: true }] });
    const id = decodeURIComponent(path.split("/")[3] ?? "");
    if (path.startsWith("/v1/workflow-runs/") && path.endsWith("/snapshot")) return json(workflowSnapshot(id));
    if (path.includes("/workflow-runs/") && path.includes("/operations/")) return json({ operationId: "workflow-op", state: "completed", revision: 7 });
    if (path.endsWith("/snapshot")) return json({ sessionId: id, revision: 5, cursor: "code-cursor:" + id, instanceId: "code-instance", traces: [{ traceId: "t1", state: "paused", kind: "prompt", targetAgent: "main", settled: false, hold: false, canResume: true }] });
    if (path.endsWith("/capabilities")) return json({ agents: [], tools: [], unavailable: [] });
    if (path.endsWith("/branches")) return json({ branches: [] });
    if (path.endsWith("/render")) return new Response([
      JSON.stringify({ beginRendering: { surfaceId: "session:" + id, root: "root" } }),
      JSON.stringify({ surfaceUpdate: { surfaceId: "session:" + id, components: [{ id: "root", component: { Text: { value: "code-surface:" + id } } }] } }),
    ].join("\n"), { headers: { "X-Session-Cursor": "code-cursor:" + id } });
    if (path.includes("/events")) {
      const stream = new ReadableStream<Uint8Array>({ start(controller) {
        streams.push({ path, signal: opts.signal });
        controller.enqueue(new TextEncoder().encode(`event: ready\ndata: ${JSON.stringify({ live: true, handoff: path.startsWith("/v1/workflow-runs/") ? cursor(id) : "bound" })}\n\n`));
      }, cancel() {} });
      return new Response(stream);
    }
    throw new Error("unexpected offline App request: " + path);
  }));
  return { requests, streams, setPost(fn: typeof post) { post = fn; }, setGet(fn: typeof get) { get = fn; }, setDefinitions(list: unknown[]) { definitions = list; } };
}
const button = (name: string) => screen.getByRole<HTMLButtonElement>("button", { name });
const creates = (s: ReturnType<typeof service>) => s.requests.filter((r) => r.method === "POST" && r.path === "/v1/workflow-runs");
async function connect() {
  render(<App />);
  fireEvent.change(screen.getByLabelText("令牌"), { target: { value: crypto.randomUUID() } });
  fireEvent.click(button("连接"));
  await screen.findByRole("list", { name: "会话列表" });
}
async function workflows() {
  fireEvent.click(button("工作流"));
  await screen.findByRole("option", { name: "review · v1" });
  fireEvent.change(screen.getByRole("combobox", { name: "工作流定义" }), { target: { value: JSON.stringify(["review", "v1"]) } });
}
function fill(title = "original", workspace = "D:/approved/work") {
  fireEvent.change(screen.getByRole("textbox", { name: "工作区绝对路径" }), { target: { value: workspace } });
  fireEvent.change(screen.getByRole("textbox", { name: "title（必填）" }), { target: { value: title } });
  fireEvent.change(screen.getByRole("spinbutton", { name: "count（必填）" }), { target: { value: "2" } });
  fireEvent.change(screen.getByRole("combobox", { name: "enabled" }), { target: { value: "true" } });
}
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

describe("App dual resource navigation", () => {
  it("loads global workflow definitions/history and browses same-named Code and Workflow identities without writes", async () => {
    const s = service();
    await connect();
    fireEvent.click(within(screen.getByRole("list", { name: "会话列表" })).getByRole("button", { name: "same" }));
    await screen.findByText("code-surface:same");
    await workflows();
    expect(s.requests.some((r) => r.path === "/v1/workflows")).toBe(true);
    expect(s.requests.some((r) => r.path === "/v1/workflow-runs")).toBe(true);
    const history = screen.getByRole("list", { name: "工作流运行列表" });
    fireEvent.click(within(history).getByRole("button", { name: /same/ }));
    await screen.findByRole("region", { name: "工作流运行" });
    await waitFor(() => expect(s.streams).toHaveLength(2));
    expect(s.streams[0].signal?.aborted).toBe(true);
    expect(s.streams.map((stream) => stream.path)).toEqual(["/v1/sessions/same/ui/events?cursor=code-cursor%3Asame", "/v1/workflow-runs/same/events?cursor=" + encodeURIComponent(cursor("same"))]);
    expect(s.requests.filter((r) => r.method === "POST")).toHaveLength(0);
    expect(s.requests.some((r) => r.path.startsWith("/v1/sessions/") && r.path.includes("workflows"))).toBe(false);
    fireEvent.click(button("代码会话"));
    await screen.findByText("code-surface:same");
    expect(screen.queryByRole("region", { name: "工作流运行" })).toBeNull();
    expect(s.requests.filter((r) => r.method === "POST")).toHaveLength(0);
  });

  it("creates only a versioned structured workflow run, never a Session targetAgent input", async () => {
    const s = service();
    await connect(); await workflows(); fill();
    fireEvent.change(screen.getByRole("combobox", { name: "工作流定义" }), { target: { value: JSON.stringify(["review", "v2"]) } });
    fill("versioned");
    fireEvent.click(button("启动工作流"));
    await screen.findByRole("heading", { name: "new-run" });
    expect(creates(s)).toHaveLength(1);
    expect(JSON.parse(creates(s)[0].body)).toEqual({ workspace: "D:/approved/work", workflow: "review", version: "v2", input: { count: 2, enabled: true, title: "versioned" } });
    expect(creates(s)[0].key).not.toBe("");
    expect(s.requests.filter((r) => r.method === "POST").every((r) => r.path === "/v1/workflow-runs")).toBe(true);
  });

  it("rejects schema errors and relative workspaces before any create side effect", async () => {
    const s = service();
    await connect(); await workflows();
    fireEvent.change(screen.getByRole("textbox", { name: "工作区绝对路径" }), { target: { value: "D:/approved/work" } });
    fireEvent.submit(screen.getByRole("form", { name: "新建工作流运行" }));
    await screen.findByText("参数“count”无效或缺失。");
    expect(creates(s)).toHaveLength(0);
    fill("valid", "relative/path");
    fireEvent.submit(screen.getByRole("form", { name: "新建工作流运行" }));
    await screen.findByText("请填写工作区绝对路径。");
    expect(creates(s)).toHaveLength(0);
    fill();
    fireEvent.change(screen.getByRole("spinbutton", { name: "count（必填）" }), { target: { value: "1.5" } });
    fireEvent.submit(screen.getByRole("form", { name: "新建工作流运行" }));
    expect(creates(s)).toHaveLength(0);
  });

  it("shows an unsupported schema instead of guessing input or sending a create", async () => {
    const s = service();
    s.setDefinitions([{ ...definition, inputSchema: { type: "object", properties: { nested: { type: "object" } } } }]);
    await connect(); await workflows();
    await screen.findByText("该工作流的参数结构无法生成表单。");
    expect(button("启动工作流").disabled).toBe(true);
    fireEvent.submit(screen.getByRole("form", { name: "新建工作流运行" }));
    expect(creates(s)).toHaveLength(0);
  });

  it("keeps Code and Workflow unconfirmed controls with the same resource ID in separate scopes", async () => {
    const s = service();
    s.setPost((r) => {
      if (r.path.startsWith("/v1/sessions/")) throw new TypeError("lost Code receipt");
      return json({ operationId: "workflow-op", state: "accepted", target: "same", acceptedCommit: 8, scope: "durable" }, 202);
    });
    await connect();
    fireEvent.click(within(screen.getByRole("list", { name: "会话列表" })).getByRole("button", { name: "same" }));
    await waitFor(() => expect(button("恢复").disabled).toBe(false));
    fireEvent.click(button("恢复"));
    await screen.findByRole("button", { name: "重试恢复" });
    const original = s.requests.find((r) => r.method === "POST")!;
    await workflows();
    fireEvent.click(within(screen.getByRole("list", { name: "工作流运行列表" })).getByRole("button", { name: /same/ }));
    await waitFor(() => expect(button("取消工作流").disabled).toBe(false));
    expect(screen.queryByRole("button", { name: "重试恢复" })).toBeNull();
    fireEvent.click(button("取消工作流"));
    await waitFor(() => expect(s.requests.filter((r) => r.method === "POST")).toHaveLength(2));
    const workflow = s.requests.filter((r) => r.method === "POST")[1];
    expect(workflow.path).toBe("/v1/workflow-runs/same/cancel");
    expect(workflow.key).not.toBe(original.key);
    fireEvent.click(button("代码会话"));
    await waitFor(() => expect(button("重试恢复").disabled).toBe(false));
    fireEvent.click(button("重试恢复"));
    await waitFor(() => expect(s.requests.filter((r) => r.method === "POST")).toHaveLength(3));
    const retried = s.requests.filter((r) => r.method === "POST")[2];
    expect({ path: retried.path, key: retried.key, body: retried.body }).toEqual({ path: original.path, key: original.key, body: original.body });
  });
});

describe("workflow read-only history paging", () => {
  const entry = (runId: string) => ({ runId, definitionName: "review", definitionVersion: "v1", state: "paused", available: true });
  it("loads one next page in flight, deduplicates runs, and selects the second page without opening or resuming", async () => {
    const s = service(); const page = deferred<Response>();
    s.setGet((r) => r.path === "/v1/workflow-runs" ? json({ runs: [entry("same")], next: "rid/+ ?" }) : r.path.startsWith("/v1/workflow-runs?") ? page.promise : undefined);
    await connect(); await workflows();
    const more = await screen.findByRole("button", { name: "加载更多工作流运行" });
    fireEvent.click(more); fireEvent.click(more);
    await waitFor(() => expect(s.requests.filter((r) => r.path.includes("?after="))).toHaveLength(1));
    expect(s.requests.find((r) => r.path.includes("?after="))?.path).toBe("/v1/workflow-runs?after=rid%2F%2B%20%3F");
    await act(async () => page.resolve(json({ runs: [entry("same"), entry("second-page") ] })));
    const list = screen.getByRole("list", { name: "工作流运行列表" });
    expect(within(list).getAllByRole("button")).toHaveLength(2);
    fireEvent.click(within(list).getByRole("button", { name: /second-page/ }));
    await screen.findByRole("heading", { name: "second-page" });
    expect(s.requests.filter((r) => r.method === "POST")).toHaveLength(0);
    expect(screen.queryByRole("button", { name: "加载更多工作流运行" })).toBeNull();
  });
  it("clears old pages when the workflow history list is refreshed", async () => {
    const s = service();
    s.setGet((r) => r.path === "/v1/workflow-runs" ? json({ runs: [entry("same")], next: "same" }) : r.path.startsWith("/v1/workflow-runs?") ? json({ runs: [entry("old-page")] }) : undefined);
    await connect(); await workflows();
    fireEvent.click(await screen.findByRole("button", { name: "加载更多工作流运行" }));
    await screen.findByRole("button", { name: /old-page/ });
    fireEvent.click(button("刷新列表"));
    await screen.findByRole("button", { name: "加载更多工作流运行" });
    expect(screen.queryByRole("button", { name: /old-page/ })).toBeNull();
    expect(s.requests.filter((r) => r.method === "POST")).toHaveLength(0);
  });
  it("aborts a late next page across pane navigation and does not apply it after returning", async () => {
    const s = service(); const page = deferred<Response>();
    s.setGet((r) => r.path === "/v1/workflow-runs" ? json({ runs: [entry("same")], next: "same" }) : r.path.startsWith("/v1/workflow-runs?") ? page.promise : undefined);
    await connect(); await workflows();
    fireEvent.click(await screen.findByRole("button", { name: "加载更多工作流运行" }));
    await waitFor(() => expect(s.requests.filter((r) => r.path.includes("?after="))).toHaveLength(1));
    const old = s.requests.find((r) => r.path.includes("?after="))!;
    fireEvent.click(button("代码会话"));
    expect(old.signal?.aborted).toBe(true);
    await workflows();
    await act(async () => page.resolve(json({ runs: [entry("late-page")] })));
    expect(screen.queryByRole("button", { name: /late-page/ })).toBeNull();
    expect(button("加载更多工作流运行").disabled).toBe(false);
    expect(s.requests.filter((r) => r.method === "POST")).toHaveLength(0);
  });
});

describe("App immutable creation receipts", () => {
  for (const failure of ["network", "503", "malformed"] as const) {
    it(`retains the original workflow create body/key after ${failure}, edits and switching panes`, async () => {
      const s = service();
      let accepted = 0;
      let original: Request | undefined;
      s.setPost((r) => {
        if (original) { expect({ key: r.key, body: r.body }).toEqual({ key: original.key, body: original.body }); return json(workflowSnapshot("new-run"), 202); }
        original = r; accepted++;
        if (failure === "network") throw new TypeError("offline receipt lost");
        if (failure === "malformed") return new Response("{", { status: 202 });
        return json({ error: { code: "storage_unavailable", message: "private-output" } }, 503);
      });
      await connect(); await workflows(); fill();
      await act(async () => { fireEvent.click(button("启动工作流")); fireEvent.click(button("启动工作流")); });
      await screen.findByRole("button", { name: "重试创建工作流" });
      expect(creates(s)).toHaveLength(1);
      fill("edited", "D:/different/work");
      fireEvent.submit(screen.getByRole("form", { name: "新建工作流运行" }));
      expect(creates(s)).toHaveLength(1);
      fireEvent.click(button("代码会话"));
      expect(screen.queryByRole("button", { name: "重试创建工作流" })).toBeNull();
      await workflows();
      fireEvent.click(button("重试创建工作流"));
      await screen.findByRole("heading", { name: "new-run" });
      expect(creates(s)).toHaveLength(2);
      expect(accepted).toBe(1);
    });
  }

  it("requires explicit abandon before using edited input and a new creation key", async () => {
    const s = service();
    s.setPost(() => { throw new TypeError("offline lost"); });
    await connect(); await workflows(); fill();
    fireEvent.click(button("启动工作流"));
    await screen.findByRole("button", { name: "放弃创建工作流" });
    fill("edited");
    fireEvent.click(button("放弃创建工作流"));
    fireEvent.click(button("启动工作流"));
    await waitFor(() => expect(creates(s)).toHaveLength(2));
    expect(creates(s)[1].key).not.toBe(creates(s)[0].key);
    expect(JSON.parse(creates(s)[1].body).input.title).toBe("edited");
  });

  it("ignores a late create receipt after switching to Code and offers only the original explicit retry", async () => {
    const s = service();
    const receipt = deferred<Response>();
    s.setPost(() => receipt.promise);
    await connect(); await workflows(); fill();
    fireEvent.click(button("启动工作流"));
    await waitFor(() => expect(creates(s)).toHaveLength(1));
    const original = creates(s)[0];
    fireEvent.click(button("代码会话"));
    await act(async () => receipt.resolve(json(workflowSnapshot("late-run"), 202)));
    expect(original.signal?.aborted).toBe(true);
    expect(screen.queryByRole("heading", { name: "late-run" })).toBeNull();
    await workflows();
    expect(screen.queryByRole("heading", { name: "late-run" })).toBeNull();
    expect(button("重试创建工作流").disabled).toBe(false);
    s.setPost(() => json(workflowSnapshot("late-run"), 202));
    fireEvent.click(button("重试创建工作流"));
    await screen.findByRole("heading", { name: "late-run" });
    expect(creates(s)[1].key).toBe(original.key);
    expect(creates(s)[1].body).toBe(original.body);
  });

  it("keeps Code session creation immutable too instead of changing its key after editing the workspace", async () => {
    const s = service();
    let accepted = false;
    s.setPost(() => { if (!accepted) { accepted = true; throw new TypeError("offline lost"); } return json({ sessionId: "new-session", revision: 1, cursor: "code-cursor:new-session" }, 202); });
    await connect();
    fireEvent.change(screen.getByRole("textbox", { name: "工作区绝对路径" }), { target: { value: "D:/approved/work" } });
    fireEvent.click(button("新建会话"));
    await screen.findByRole("button", { name: "重试创建会话" });
    fireEvent.change(screen.getByRole("textbox", { name: "工作区绝对路径" }), { target: { value: "D:/edited/work" } });
    fireEvent.click(button("重试创建会话"));
    await screen.findByText("code-surface:new-session");
    const writes = s.requests.filter((r) => r.method === "POST");
    expect(writes).toHaveLength(2);
    expect(writes[1].key).toBe(writes[0].key);
    expect(writes[1].body).toBe(writes[0].body);
  });
});
