import { afterEach, describe, expect, it, vi } from "vitest";
import { Client, type WorkflowSnapshot } from "./client";
import { syncWorkflow, workflowSnapshotDTO } from "./workflow";
import type { SSEFrame } from "./sse";

const encode = (raw: unknown) => btoa(String.fromCharCode(...new TextEncoder().encode(JSON.stringify(raw)))).replaceAll("+", "-").replaceAll("/", "_").replace(/=+$/, "");
const cursor = (rid = "run1", seq = "7") => encode([2, "workflow", rid, seq]);
const initial = (rid = "run1", revision = 7): WorkflowSnapshot => ({ runId: rid, definitionName: "review", definitionVersion: "v1", state: "running", revision, cursor: cursor(rid, String(revision)), durableSeq: String(revision), instanceId: "instance-1", executionStopped: false, canResume: false, workflowNodes: [], interactions: [] });
const json = (body: unknown) => new Response(JSON.stringify(body));
function deferred<T>() { let resolve!: (value: T) => void; const promise = new Promise<T>((yes) => { resolve = yes; }); return { promise, resolve }; }
function service() {
  const paths: string[] = [];
  const streams: { signal?: AbortSignal | null; send: (event: string, data: unknown, id?: string) => void; close: () => void }[] = [];
  let snapshot: () => Response | Promise<Response> = () => json(initial());
  vi.stubGlobal("fetch", vi.fn(async (path: string, opts: RequestInit) => {
    paths.push(path);
    if (path.endsWith("/snapshot")) return snapshot();
    const stream = new ReadableStream<Uint8Array>({ start(controller) {
      streams.push({ signal: opts.signal, send: (event, data, id) => controller.enqueue(new TextEncoder().encode(`event: ${event}\n${id !== undefined ? `id: ${id}\n` : ""}data: ${JSON.stringify(data)}\n\n`)), close: () => controller.close() });
    }, cancel() {} });
    return new Response(stream);
  }));
  return { paths, streams, setSnapshot(fn: typeof snapshot) { snapshot = fn; } };
}
const fact = { type: "workflow.state_changed", scope: { workflowRunId: "run1" }, payload: { state: "never-applied-as-state" } };
afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals(); });

describe("typed workflow cursors", () => {
  for (const bad of [encode([1, "run1", "7"]), cursor("other"), "opaque", cursor() + "=", encode([2, "workflow", "run1", "07"]), cursor("run1", "18446744073709551616"), cursor("run1", "8"), encode([2, "workflow", "run1", 7]), "A".repeat(513), btoa('[2, "workflow", "run1", "7"]').replace(/=+$/, ""), "_w", cursor().slice(0, -1) + "R"]) {
    it(`rejects snapshot cursor ${bad}`, () => {
      expect(() => workflowSnapshotDTO({ ...initial(), cursor: bad }, "run1")).toThrow("invalid_argument");
    });
  }
  for (const seq of ["0", "9007199254740993", "18446744073709551615"]) {
    it(`preserves uint64 string position ${seq} and UTF8 identity`, () => {
      const rid = "运行/审查";
      expect(workflowSnapshotDTO({ ...initial(rid), durableSeq: seq, cursor: cursor(rid, seq) }, rid).durableSeq).toBe(seq);
    });
  }
  for (const handoff of [encode([1, "run1", "7"]), cursor("other"), cursor("run1", "6"), cursor() + "="]) {
    it(`refuses wrong ready handoff ${handoff} without extra queries or live`, async () => {
      const s = service();
      const ctrl = new AbortController();
      const statuses: string[] = [];
      const errors: unknown[] = [];
      const snapshots: WorkflowSnapshot[] = [];
      const task = syncWorkflow(new Client(""), "run1", ctrl.signal, (snap) => snapshots.push(snap), (status, err) => { statuses.push(status); if (err) errors.push(err); });
      try {
        await vi.waitFor(() => expect(s.streams).toHaveLength(1));
        s.streams[0].send("ready", { handoff, live: true });
        s.streams[0].send("end", {}); s.streams[0].close();
        await task;
        expect(statuses).not.toContain("live");
        expect(statuses.at(-1)).toBe("error");
        expect(errors).toHaveLength(1);
        expect(s.paths).toHaveLength(2);
        expect(snapshots).toHaveLength(1);
      } finally { ctrl.abort(); }
    });
  }
  for (const ready of [null, [], { live: true }, { handoff: cursor(), live: "true" }]) {
    it(`refuses malformed ready ${JSON.stringify(ready)}`, async () => {
      const s = service(); const ctrl = new AbortController(); const statuses: string[] = [];
      const task = syncWorkflow(new Client(""), "run1", ctrl.signal, () => {}, (status) => statuses.push(status));
      try {
        await vi.waitFor(() => expect(s.streams).toHaveLength(1));
        s.streams[0].send("ready", ready);
        s.streams[0].send("end", {}); s.streams[0].close();
        await task;
        expect(statuses.at(-1)).toBe("error"); expect(statuses).not.toContain("live"); expect(s.paths).toHaveLength(2);
      } finally { ctrl.abort(); }
    });
  }
  it("retains the fixed ready identity error when aborting its transport rejects the stream", async () => {
    const client = new Client(""); const ctrl = new AbortController();
    vi.spyOn(client, "workflowSnapshot").mockResolvedValue(initial());
    vi.spyOn(client, "workflowEvents").mockImplementation(async (_rid, _cursor, callback) => {
      callback({ event: "ready", data: JSON.stringify({ live: true, handoff: cursor("other") }) });
      throw new DOMException("transport aborted", "AbortError");
    });
    const statuses: string[] = []; const errors: unknown[] = [];
    await syncWorkflow(client, "run1", ctrl.signal, () => {}, (status, err) => {
      statuses.push(status); if (err) errors.push(err);
      if (status === "reconnecting") ctrl.abort();
    });
    expect(statuses).toEqual(["loading", "error"]);
    expect(errors).toEqual([expect.objectContaining({ code: "invalid_argument" })]);
    expect(client.workflowSnapshot).toHaveBeenCalledTimes(1);
  });
  it("ignores wrong roots, noncanonical and nonmonotonic durable frame ids and mismatched payload cursors", async () => {
    const s = service(); const ctrl = new AbortController(); const snapshots: WorkflowSnapshot[] = [];
    const task = syncWorkflow(new Client(""), "run1", ctrl.signal, (snap) => snapshots.push(snap), () => {});
    try {
      await vi.waitFor(() => expect(s.streams).toHaveLength(1));
      s.streams[0].send("ready", { live: true, handoff: cursor() });
      s.setSnapshot(() => json(initial("run1", 8)));
      for (const id of [encode([1, "run1", "8"]), cursor("other", "8"), cursor() + "=", cursor(), cursor("run1", "6"), cursor("run1", "18446744073709551616")]) s.streams[0].send("event", fact, id);
      s.streams[0].send("event", { ...fact, cursor: cursor("run1", "9") }, cursor("run1", "8"));
      s.streams[0].send("event", { ...fact, runId: "other" }, cursor("run1", "8"));
      await new Promise((resolve) => setTimeout(resolve, 180));
      expect(s.paths).toHaveLength(2); expect(snapshots).toHaveLength(1);
      s.streams[0].send("event", { ...fact, cursor: cursor("run1", "9007199254740993") }, cursor("run1", "9007199254740993"));
      await vi.waitFor(() => expect(snapshots.at(-1)?.revision).toBe(8));
      s.streams[0].send("event", fact, cursor("run1", "9007199254740993"));
      s.streams[0].send("event", fact, cursor("run1", "8"));
      await new Promise((resolve) => setTimeout(resolve, 180));
      expect(s.paths).toHaveLength(3);
    } finally { ctrl.abort(); await task; }
  });
});

describe("workflow synchronization", () => {
  it("coalesces an event burst into one bounded snapshot refresh without applying event payload state", async () => {
    const s = service();
    const ctrl = new AbortController();
    const snapshots: WorkflowSnapshot[] = [];
    const task = syncWorkflow(new Client(""), "run1", ctrl.signal, (snap) => snapshots.push(snap), () => {});
    try {
      await vi.waitFor(() => expect(s.streams).toHaveLength(1));
      s.streams[0].send("ready", { live: true, handoff: cursor() });
      s.setSnapshot(() => json(initial("run1", 8)));
      for (let i = 0; i < 100; i++) s.streams[0].send("event", fact);
      await vi.waitFor(() => expect(snapshots.at(-1)?.revision).toBe(8));
      expect(s.paths.filter((p) => p.endsWith("/snapshot"))).toHaveLength(2);
      expect(snapshots.at(-1)?.state).toBe("running");
      expect(s.paths.every((p) => p.startsWith("/v1/workflow-runs/run1/"))).toBe(true);
    } finally { ctrl.abort(); await task; }
  });

  it("flushes facts arriving during an in-flight refresh before ending historical observation", async () => {
    const s = service();
    const ctrl = new AbortController();
    const snapshots: WorkflowSnapshot[] = [];
    const statuses: string[] = [];
    const slow = deferred<Response>();
    let count = 0;
    s.setSnapshot(() => ++count === 1 ? json(initial()) : count === 2 ? slow.promise : json(initial("run1", 9)));
    const task = syncWorkflow(new Client(""), "run1", ctrl.signal, (snap) => snapshots.push(snap), (status) => statuses.push(status));
    try {
      await vi.waitFor(() => expect(s.streams).toHaveLength(1));
      s.streams[0].send("ready", { live: false, handoff: cursor() });
      s.streams[0].send("event", fact);
      await vi.waitFor(() => expect(count).toBe(2));
      s.streams[0].send("event", fact);
      s.streams[0].send("end", {});
      s.streams[0].close();
      slow.resolve(json(initial("run1", 8)));
      await task;
      expect(snapshots.at(-1)?.revision).toBe(9);
      expect(count).toBe(3);
      expect(statuses.at(-1)).toBe("ended");
    } finally { ctrl.abort(); await task; }
  });

  it("resyncs by reloading this run and subscribes using only its fresh opaque cursor", async () => {
    const s = service();
    const ctrl = new AbortController();
    const snapshots: WorkflowSnapshot[] = [];
    const task = syncWorkflow(new Client(""), "run1", ctrl.signal, (snap) => snapshots.push(snap), () => {});
    try {
      await vi.waitFor(() => expect(s.streams).toHaveLength(1));
      s.streams[0].send("ready", { live: true, handoff: cursor() });
      s.setSnapshot(() => json(initial("run1", 8)));
      s.streams[0].send("resync", {});
      await vi.waitFor(() => expect(s.streams).toHaveLength(2));
      expect(s.streams[0].signal?.aborted).toBe(true);
      expect(s.paths.filter((p) => p.includes("/events"))).toEqual(["/v1/workflow-runs/run1/events?cursor=" + cursor(), "/v1/workflow-runs/run1/events?cursor=" + cursor("run1", "8")]);
      expect(snapshots.at(-1)?.revision).toBe(8);
    } finally { ctrl.abort(); await task; }
  });

  it("reconnects live EOF with a fresh snapshot but performs no writer or execution control request", async () => {
    const s = service();
    const ctrl = new AbortController();
    const task = syncWorkflow(new Client(""), "run1", ctrl.signal, () => {}, () => {});
    try {
      await vi.waitFor(() => expect(s.streams).toHaveLength(1));
      s.streams[0].send("ready", { live: true, handoff: cursor() });
      s.streams[0].close();
      await vi.waitFor(() => expect(s.streams).toHaveLength(2), { timeout: 1500 });
      expect(s.paths.filter((p) => p.endsWith("/snapshot"))).toHaveLength(2);
      expect(s.paths.some((p) => /open|resume|cancel|pause/.test(p))).toBe(false);
    } finally { ctrl.abort(); await task; }
  });

  it("discards a late snapshot when the observation signal was aborted", async () => {
    const s = service();
    const late = deferred<Response>();
    s.setSnapshot(() => late.promise);
    const ctrl = new AbortController();
    const snapshots: WorkflowSnapshot[] = [];
    const task = syncWorkflow(new Client(""), "run1", ctrl.signal, (snap) => snapshots.push(snap), () => {});
    await vi.waitFor(() => expect(s.paths).toHaveLength(1));
    ctrl.abort(); late.resolve(json(initial()));
    await task;
    expect(snapshots).toHaveLength(0);
    expect(s.streams).toHaveLength(0);
  });

  it("rejects a late frame even when a superseded subscription ignores its AbortSignal", async () => {
    const client = new Client("");
    vi.spyOn(client, "workflowSnapshot").mockResolvedValue(initial());
    const stream = deferred<void>();
    let onFrame: ((frame: SSEFrame) => void) | undefined;
    vi.spyOn(client, "workflowEvents").mockImplementation(async (_rid, _cursor, callback) => { onFrame = callback; await stream.promise; });
    const ctrl = new AbortController();
    const statuses: string[] = [];
    const task = syncWorkflow(client, "run1", ctrl.signal, () => {}, (status) => statuses.push(status));
    await vi.waitFor(() => expect(onFrame).toBeDefined());
    ctrl.abort();
    onFrame!({ event: "ready", data: '{"live":true}' });
    onFrame!({ event: "event", data: JSON.stringify(fact) });
    stream.resolve(); await task;
    expect(statuses).toEqual(["loading"]);
    expect(client.workflowSnapshot).toHaveBeenCalledTimes(1);
  });

  for (const change of [{ runId: "other" }, { revision: Number.MAX_SAFE_INTEGER + 1 }, { revision: -1 }]) {
    it(`refuses unsafe or cross-resource DTO ${JSON.stringify(change)}`, () => {
      expect(() => workflowSnapshotDTO({ ...initial(), ...change }, "run1")).toThrow("invalid_argument");
    });
  }
});
