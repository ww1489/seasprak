import { afterEach, describe, expect, it, vi } from "vitest";
import { APIError, Client, errorText, type Snapshot } from "./client";

afterEach(() => vi.unstubAllGlobals());

describe("independent workflow HTTP", () => {
  const snapshot = {
    runId: "run/with space", definitionName: "review", definitionVersion: "v1", state: "paused",
    revision: 8, cursor: "opaque-workflow-cursor", durableSeq: "9007199254740993", instanceId: "instance-1",
    executionStopped: true, canResume: true, workflowNodes: [], interactions: [],
  };
  const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

  it("queries definitions and persisted runs globally without a session identity", async () => {
    const ctrl = new AbortController();
    const fetch = vi.fn(async (path: string, opts: RequestInit) => {
      expect(opts.method).toBe("GET");
      expect(opts.signal).toBe(ctrl.signal);
      expect(opts.body).toBeUndefined();
      return path === "/v1/workflows" ? json({ workflows: [{ name: "review", version: "v1", inputSchema: { type: "object" } }] }) : json({ runs: [], next: "next-page" });
    });
    vi.stubGlobal("fetch", fetch);
    const client = new Client("");
    await client.workflows(ctrl.signal);
    await client.listWorkflowRuns(ctrl.signal);
    expect(fetch.mock.calls.map(([path]) => path)).toEqual(["/v1/workflows", "/v1/workflow-runs"]);
  });

  it("encodes an opaque workflow history after anchor while preserving the signal-only call", async () => {
    const ctrl = new AbortController();
    const fetch = vi.fn(async (_path: string, opts: RequestInit) => {
      expect(opts.signal).toBe(ctrl.signal); expect(opts.method).toBe("GET"); expect(opts.body).toBeUndefined();
      return json({ runs: [] });
    });
    vi.stubGlobal("fetch", fetch);
    const c = new Client("");
    await c.listWorkflowRuns(ctrl.signal);
    await c.listWorkflowRuns(ctrl.signal, { after: "rid/+ ?" });
    expect(fetch.mock.calls.map(([path]) => path)).toEqual(["/v1/workflow-runs", "/v1/workflow-runs?after=rid%2F%2B%20%3F"]);
  });

  it("creates structured input with exactly the fixed body and original key", async () => {
    const token = crypto.randomUUID();
    const ctrl = new AbortController();
    const fetch = vi.fn(async (path: string, opts: RequestInit) => {
      expect(path).toBe("/v1/workflow-runs");
      expect(opts.method).toBe("POST");
      expect(JSON.parse(String(opts.body))).toEqual({ workspace: "D:/approved/work", workflow: "review", version: "v1", input: { title: "test", count: 2 } });
      const headers = new Headers(opts.headers);
      expect(headers.get("Authorization") === "Bearer " + token).toBe(true);
      expect(headers.get("Idempotency-Key")).toBe("create-key");
      expect(opts.signal).toBe(ctrl.signal);
      expect(opts.credentials).toBe("omit");
      expect(opts.cache).toBe("no-store");
      expect(opts.redirect).toBe("error");
      return json(snapshot, 202);
    });
    vi.stubGlobal("fetch", fetch);
    expect(await new Client(token).createWorkflowRun("D:/approved/work", "review", "v1", { title: "test", count: 2 }, "create-key", ctrl.signal)).toEqual(snapshot);
    expect(fetch).toHaveBeenCalledTimes(1);
  });

  it("encodes run, operation and interaction paths and scopes all controls to the run", async () => {
    const records: { path: string; method?: string; body?: BodyInit | null; key: string | null }[] = [];
    const fetch = vi.fn(async (path: string, opts: RequestInit) => {
      records.push({ path, method: opts.method, body: opts.body, key: new Headers(opts.headers).get("Idempotency-Key") });
      return json(path.endsWith("/snapshot") || path.endsWith("/open") ? snapshot : { operationId: "op/1", state: "accepted", target: snapshot.runId, acceptedCommit: 9, scope: "durable" });
    });
    vi.stubGlobal("fetch", fetch);
    const c = new Client("");
    await c.workflowSnapshot(snapshot.runId);
    await c.openWorkflowRun(snapshot.runId);
    await c.pauseWorkflowRun(snapshot.runId, 8, "pause-key");
    await c.cancelWorkflowRun(snapshot.runId, 8, "cancel-key", "manual");
    await c.resumeWorkflowRun(snapshot.runId, 8, "resume-key");
    await c.respondWorkflowInteraction(snapshot.runId, "approval/1", "allowed-once", 8, "instance-1", "approval-key");
    await c.workflowOperation(snapshot.runId, "op/1");
    expect(records.map((r) => r.path)).toEqual([
      "/v1/workflow-runs/run%2Fwith%20space/snapshot", "/v1/workflow-runs/run%2Fwith%20space/open",
      "/v1/workflow-runs/run%2Fwith%20space/pause", "/v1/workflow-runs/run%2Fwith%20space/cancel",
      "/v1/workflow-runs/run%2Fwith%20space/resume", "/v1/workflow-runs/run%2Fwith%20space/interactions/approval%2F1/responses",
      "/v1/workflow-runs/run%2Fwith%20space/operations/op%2F1",
    ]);
    expect(records.slice(1, 6).map((r) => JSON.parse(String(r.body)))).toEqual([
      {}, { expectedRevision: 8 }, { expectedRevision: 8, reason: "manual" }, { expectedRevision: 8 },
      { decision: "allowed-once", expectedRevision: 8, instanceId: "instance-1" },
    ]);
    expect(records.map((r) => r.key)).toEqual([null, null, "pause-key", "cancel-key", "resume-key", "approval-key", null]);
  });

  it("streams only the encoded workflow resource and keeps its cursor opaque", async () => {
    const ctrl = new AbortController();
    const fetch = vi.fn(async (path: string, opts: RequestInit) => {
      expect(path).toBe("/v1/workflow-runs/run%2Fwith%20space/events?cursor=opaque%2F%2B%20cursor");
      expect(opts.signal).toBe(ctrl.signal);
      return new Response("event: ready\ndata: {\"handoff\":\"opaque\",\"live\":false}\n\nevent: end\ndata: {}\n\n");
    });
    vi.stubGlobal("fetch", fetch);
    const frames: unknown[] = [];
    await new Client("").workflowEvents(snapshot.runId, "opaque/+ cursor", (frame) => frames.push(frame), ctrl.signal);
    expect(frames).toHaveLength(2);
    expect(fetch).toHaveBeenCalledTimes(1);
  });

  for (const n of [-1, 1.5, Number.MAX_SAFE_INTEGER + 1, NaN]) {
    it(`refuses unsafe revision ${String(n)} before any workflow write`, async () => {
      const fetch = vi.fn();
      vi.stubGlobal("fetch", fetch);
      const c = new Client("");
      for (const write of [
        () => c.pauseWorkflowRun("run1", n, "key"), () => c.cancelWorkflowRun("run1", n, "key"),
        () => c.resumeWorkflowRun("run1", n, "key"), () => c.respondWorkflowInteraction("run1", "i1", "rejected", n, "instance-1", "key"),
      ]) await expect(Promise.resolve().then(write)).rejects.toEqual(new APIError(0, "invalid_argument"));
      expect(fetch).not.toHaveBeenCalled();
    });
  }

  it("uses fixed error text even for unknown or prototype-named public codes", () => {
    for (const code of ["private-service-detail", "toString", "constructor", "__proto__"]) expect(errorText(new APIError(500, code))).toBe("服务内部错误。");
  });

  it("keeps public errors fixed and never automatically retries workflow writes", async () => {
    const fetch = vi.fn(async () => json({ error: { code: "permission_denied", message: "private-backend-detail" } }, 403));
    vi.stubGlobal("fetch", fetch);
    try { await new Client("").cancelWorkflowRun("run1", 8, "key"); }
    catch (err) { expect(errorText(err)).toBe("没有权限执行此操作。"); expect(String(err)).not.toContain("private-backend-detail"); }
    expect(fetch).toHaveBeenCalledTimes(1);
  });
});


describe("openSession", () => {
  it("sends an authenticated strict empty POST with an encoded session identity", async () => {
    const token = crypto.randomUUID();
    const controller = new AbortController();
    const snapshot: Snapshot = { sessionId: "session/with space", revision: 8, cursor: "cursor-8", instanceId: "instance-1" };
    const fetch = vi.fn(async (path: string, opts: RequestInit) => {
      expect(path).toBe("/v1/sessions/session%2Fwith%20space/open");
      expect(opts.method).toBe("POST");
      expect(opts.body).toBe("{}");
      const headers = new Headers(opts.headers);
      // Boolean assertion avoids putting even a generated bearer in output.
      expect(headers.get("Authorization") === "Bearer " + token).toBe(true);
      expect(headers.get("Content-Type")).toBe("application/json");
      expect(headers.has("Idempotency-Key")).toBe(false);
      expect(opts.signal).toBe(controller.signal);
      expect(opts.credentials).toBe("omit");
      expect(opts.redirect).toBe("error");
      expect(opts.cache).toBe("no-store");
      return new Response(JSON.stringify(snapshot), { headers: { "Content-Type": "application/json" } });
    });
    vi.stubGlobal("fetch", fetch);
    expect(await new Client(token).openSession(snapshot.sessionId, controller.signal)).toEqual(snapshot);
    expect(fetch).toHaveBeenCalledTimes(1);
  });

  it("returns a definite public error without automatically retrying", async () => {
    const fetch = vi.fn(async () => new Response(JSON.stringify({ error: { code: "not_found" } }), { status: 404 }));
    vi.stubGlobal("fetch", fetch);
    await expect(new Client("").openSession("missing")).rejects.toEqual(new APIError(404, "not_found"));
    expect(fetch).toHaveBeenCalledTimes(1);
  });
});
