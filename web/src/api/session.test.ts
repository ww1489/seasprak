import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { A2UIStore } from "@/a2ui/store";
import { APIError, Client } from "./client";
import { syncSession, type SyncStatus } from "./session";

const encoder = new TextEncoder();
const frame = (event: string, data: unknown, id?: string) => `event: ${event}\n${id === undefined ? "" : `id: ${id}\n`}data: ${JSON.stringify(data)}\n\n`;
const errorResponse = (status: number) => new Response(JSON.stringify({ error: { code: status === 410 ? "resync_required" : "resource_unavailable" } }), { status });
function rendered(cursor = "cursor-7", sid = "s1") {
  const surfaceId = "session:" + sid;
  return new Response([
    { beginRendering: { surfaceId, root: "root" } },
    { surfaceUpdate: { surfaceId, components: [{ id: "root", component: { Text: { dataKey: "answer" } } }] } },
    { dataModelUpdate: { surfaceId, contents: [{ key: "answer", valueString: "final answer:" + cursor }] } },
  ].map((line) => JSON.stringify(line)).join("\n"), { headers: { "X-Session-Cursor": cursor } });
}
const historical = (end = true) => new Response(frame("ready", { live: false }) + (end ? frame("end", {}) : ""));
function stream() {
  let controller!: ReadableStreamDefaultController<Uint8Array>;
  const cancel = vi.fn();
  const body = new ReadableStream<Uint8Array>({ start(c) { controller = c; }, cancel });
  return {
    response: new Response(body), cancel,
    send(event: string, data: unknown, id?: string) { controller.enqueue(encoder.encode(frame(event, data, id))); },
    close() { controller.close(); },
    fail() { controller.error(new TypeError("offline test: stream interrupted")); },
  };
}
function service(responses: (Response | Error | Promise<Response>)[]) {
  const requests: { method: string; path: string; signal?: AbortSignal | null }[] = [];
  vi.stubGlobal("fetch", vi.fn(async (path: string, opts: RequestInit = {}) => {
    requests.push({ method: opts.method ?? "GET", path, signal: opts.signal });
    const response = responses.shift();
    if (response instanceof Error) throw response;
    if (!response) throw new Error("unexpected offline request: " + path);
    return response;
  }));
  return { client: new Client(""), requests };
}
function observe(client: Client, ctrl = new AbortController(), store = new A2UIStore()) {
  const statuses: { status: SyncStatus; error?: unknown }[] = [];
  let done = false;
  const task = syncSession(client, "s1", store, ctrl.signal, (status, error) => statuses.push({ status, error })).then(() => { done = true; });
  return { ctrl, store, statuses, task, get done() { return done; } };
}
const flush = () => vi.advanceTimersByTimeAsync(0);
const onlyGets = (s: ReturnType<typeof service>) => expect(s.requests.every((request) => request.method === "GET")).toBe(true);

beforeEach(() => vi.useFakeTimers());
afterEach(() => { vi.useRealTimers(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

describe("session observation failures", () => {
  for (const kind of ["network", "503", "body"] as const) {
    it(`retries render ${kind} instead of ending before subscription`, async () => {
      const failure = kind === "network" ? new TypeError("offline test: render unavailable") : kind === "503" ? errorResponse(503) : new Response(new ReadableStream<Uint8Array>({ start(c) { c.error(new TypeError("offline test: render body interrupted")); } }));
      const s = service([failure, rendered("cursor-9"), historical()]);
      const o = observe(s.client);
      try {
        await flush();
        expect(o.done).toBe(false);
        expect(o.statuses.map((entry) => entry.status)).toEqual(["loading", "reconnecting"]);
        expect(o.statuses.at(-1)?.error).toBeInstanceOf(kind === "503" ? APIError : TypeError);
        expect(s.requests.map((request) => request.path)).toEqual(["/v1/sessions/s1/render"]);
        await vi.advanceTimersByTimeAsync(499);
        expect(s.requests).toHaveLength(1);
        await vi.advanceTimersByTimeAsync(1);
        expect(o.done).toBe(true);
        expect(o.statuses.at(-1)?.status).toBe("ended");
        expect(s.requests.map((request) => request.path)).toEqual(["/v1/sessions/s1/render", "/v1/sessions/s1/render", "/v1/sessions/s1/ui/events?cursor=cursor-9"]);
        expect(o.store.snapshot.cursor).toBe("cursor-9");
        expect(o.store.snapshot.data.get("answer")).toBe("final answer:cursor-9");
        onlyGets(s);
      } finally { o.ctrl.abort(); await o.task; }
    });
  }

  for (const kind of ["network", "503", "body"] as const) {
    it(`retries SSE ${kind} before ready with a fresh render cursor`, async () => {
      const broken = stream();
      const failure = kind === "network" ? new TypeError("offline test: subscription unavailable") : kind === "503" ? errorResponse(503) : broken.response;
      const s = service([rendered(), failure, rendered("cursor-9"), historical()]);
      const o = observe(s.client);
      try {
        await flush();
        if (kind === "body") { broken.fail(); await flush(); }
        expect(o.done).toBe(false);
        expect(o.statuses.at(-1)?.status).toBe("reconnecting");
        expect(o.statuses.at(-1)?.error).toBeInstanceOf(kind === "503" ? APIError : TypeError);
        expect(s.requests).toHaveLength(2);
        await vi.advanceTimersByTimeAsync(499);
        expect(s.requests).toHaveLength(2);
        await vi.advanceTimersByTimeAsync(1);
        expect(o.done).toBe(true);
        expect(o.statuses.at(-1)?.status).toBe("ended");
        expect(s.requests.filter((request) => request.path.includes("/ui/events")).map((request) => request.path)).toEqual(["/v1/sessions/s1/ui/events?cursor=cursor-7", "/v1/sessions/s1/ui/events?cursor=cursor-9"]);
        expect(o.store.snapshot.cursor).toBe("cursor-9");
        onlyGets(s);
      } finally { o.ctrl.abort(); await o.task; }
    });
  }

  for (const location of ["render", "SSE"] as const) {
    it(`immediately resynchronizes ${location} HTTP 410 without the 500ms backoff`, async () => {
      const s = service([...(location === "SSE" ? [rendered()] : []), errorResponse(410), rendered("cursor-9"), historical()]);
      const o = observe(s.client);
      try {
        await flush();
        expect(o.done).toBe(true);
        expect(s.requests.filter((request) => request.path.endsWith("/render"))).toHaveLength(2);
        expect(s.requests.at(-1)?.path).toBe("/v1/sessions/s1/ui/events?cursor=cursor-9");
        expect(o.store.snapshot.cursor).toBe("cursor-9");
        expect(o.statuses.filter((entry) => entry.error).map((entry) => entry.error)).toEqual([expect.objectContaining({ status: 410, code: "resync_required" })]);
        expect(o.statuses.at(-1)?.status).toBe("ended");
        expect(vi.getTimerCount()).toBe(0);
        onlyGets(s);
      } finally { o.ctrl.abort(); await o.task; }
    });

    for (const status of [401, 403, 404, 409, 429]) {
      it(`ends ${location} HTTP ${status} as a permanent public error without retry`, async () => {
        const s = service([...(location === "SSE" ? [rendered()] : []), errorResponse(status)]);
        const o = observe(s.client);
        await flush();
        await o.task;
        expect(o.statuses.at(-1)).toEqual({ status: "error", error: expect.objectContaining({ status, code: "resource_unavailable" }) });
        expect(o.statuses.map((entry) => entry.status)).not.toContain("ended");
        await vi.advanceTimersByTimeAsync(5000);
        expect(s.requests).toHaveLength(location === "SSE" ? 2 : 1);
        expect(o.store.snapshot.cursor).toBe(location === "SSE" ? "cursor-7" : "");
        onlyGets(s);
      });
    }
  }

  it("retains the existing 500/1000/2000/5000ms backoff for successive failures", async () => {
    const unavailable = () => new TypeError("offline test: render unavailable");
    const s = service([unavailable(), unavailable(), unavailable(), unavailable(), unavailable(), rendered("cursor-9"), historical()]);
    const o = observe(s.client);
    try {
      await flush();
      for (const [index, delay] of [500, 1000, 2000, 5000, 5000].entries()) {
        expect(o.done).toBe(false);
        await vi.advanceTimersByTimeAsync(delay - 1);
        expect(s.requests).toHaveLength(index + 1);
        await vi.advanceTimersByTimeAsync(1);
        expect(s.requests.filter((request) => request.path.endsWith("/render"))).toHaveLength(index + 2);
      }
      expect(o.done).toBe(true);
      expect(o.statuses.at(-1)?.status).toBe("ended");
      onlyGets(s);
    } finally { o.ctrl.abort(); await o.task; }
  });

  it("keeps a failed non-live observation retryable even after ready", async () => {
    const broken = stream();
    const s = service([rendered(), broken.response, rendered("cursor-9"), historical()]);
    const o = observe(s.client);
    try {
      await flush();
      broken.send("ready", { live: false }); await flush();
      broken.fail(); await flush();
      expect(o.statuses.at(-1)?.status).toBe("reconnecting");
      expect(o.done).toBe(false);
      await vi.advanceTimersByTimeAsync(500);
      expect(o.done).toBe(true);
      expect(o.store.snapshot.cursor).toBe("cursor-9");
      expect(s.requests).toHaveLength(4);
      onlyGets(s);
    } finally { o.ctrl.abort(); await o.task; }
  });
});

describe("normal session observation and cancellation", () => {
  for (const end of [false, true]) {
    it(`ends failure-free historical observation ${end ? "with end" : "at EOF"} without reconnecting`, async () => {
      const s = service([rendered(), historical(end)]);
      const o = observe(s.client);
      await flush(); await o.task;
      expect(o.statuses.map((entry) => entry.status)).toEqual(["loading", "loading", "ended"]);
      expect(o.store.snapshot.cursor).toBe("cursor-7");
      expect(o.store.snapshot.data.get("answer")).toBe("final answer:cursor-7");
      expect(s.requests).toHaveLength(2);
      expect(vi.getTimerCount()).toBe(0);
      onlyGets(s);
    });
  }

  it("ends on an explicit end after a live ready", async () => {
    const s = service([rendered(), new Response(frame("ready", { live: true }) + frame("end", {}))]);
    const o = observe(s.client);
    await flush(); await o.task;
    expect(o.statuses.map((entry) => entry.status)).toEqual(["loading", "live", "ended"]);
    expect(s.requests).toHaveLength(2);
    onlyGets(s);
  });

  it("immediately repeats render/subscribe on resync with the new cursor", async () => {
    const s = service([rendered(), new Response(frame("ready", { live: false }) + frame("resync", {})), rendered("cursor-9"), historical()]);
    const o = observe(s.client);
    try {
      await flush();
      expect(o.done).toBe(true);
      expect(s.requests).toHaveLength(4);
      expect(s.requests.at(-1)?.path).toBe("/v1/sessions/s1/ui/events?cursor=cursor-9");
      expect(o.store.snapshot.cursor).toBe("cursor-9");
      onlyGets(s);
    } finally { o.ctrl.abort(); await o.task; }
  });

  it("cleans up each completed retry wait and cancels the remaining wait without another GET", async () => {
    const liveEOF = () => new Response(frame("ready", { live: true }));
    const s = service([rendered(), liveEOF(), rendered("cursor-9"), liveEOF()]);
    const ctrl = new AbortController();
    const added = vi.spyOn(ctrl.signal, "addEventListener");
    const removed = vi.spyOn(ctrl.signal, "removeEventListener");
    const pending = () => added.mock.calls.filter(([event, listener]) => event === "abort" && !removed.mock.calls.some(([removedEvent, removedListener]) => removedEvent === event && removedListener === listener));
    const o = observe(s.client, ctrl);
    try {
      await flush();
      expect(pending()).toHaveLength(1);
      await vi.advanceTimersByTimeAsync(500);
      expect(s.requests).toHaveLength(4);
      expect(pending()).toHaveLength(1);
      ctrl.abort(); await o.task;
      expect(pending()).toHaveLength(0);
      expect(vi.getTimerCount()).toBe(0);
      await vi.advanceTimersByTimeAsync(5000);
      expect(s.requests).toHaveLength(4);
      expect(o.statuses.map((entry) => entry.status)).not.toContain("ended");
      onlyGets(s);
    } finally { ctrl.abort(); await o.task; }
  });

  it("stops if the retry status callback aborts before its wait is registered", async () => {
    const s = service([new TypeError("offline test: render unavailable")]);
    const ctrl = new AbortController();
    const statuses: SyncStatus[] = [];
    let done = false;
    const task = syncSession(s.client, "s1", new A2UIStore(), ctrl.signal, (status) => {
      statuses.push(status);
      if (status === "reconnecting") ctrl.abort();
    }).then(() => { done = true; });
    try {
      await flush();
      expect(statuses).toEqual(["loading", "reconnecting"]);
      expect(ctrl.signal.aborted).toBe(true);
      expect(done).toBe(true);
      expect(vi.getTimerCount()).toBe(0);
      expect(s.requests).toHaveLength(1);
      onlyGets(s);
    } finally { ctrl.abort(); await vi.runAllTimersAsync(); await task; }
  });

  it("leaves a partial group cursor unacknowledged and applies a full replay once after reconnect", async () => {
    const first = stream();
    const second = stream();
    const s = service([rendered(), first.response, rendered("cursor-9"), second.response]);
    const o = observe(s.client);
    const update = { surfaceUpdate: { surfaceId: "session:s1", components: [{ id: "root", component: { ChatMessage: { messageId: "assistant-1", role: "assistant", status: "final", dataKey: "answer" } } }] } };
    try {
      await flush();
      first.send("ready", { live: true });
      first.send("a2ui", update);
      await flush();
      expect(o.store.snapshot.cursor).toBe("cursor-7");
      expect(o.store.snapshot.data.get("answer")).toBe("final answer:cursor-7");
      first.fail(); await flush();
      await vi.advanceTimersByTimeAsync(500);
      expect(s.requests.filter((request) => request.path.includes("/ui/events")).map((request) => request.path)).toEqual(["/v1/sessions/s1/ui/events?cursor=cursor-7", "/v1/sessions/s1/ui/events?cursor=cursor-9"]);
      second.send("ready", { live: false });
      for (let replay = 0; replay < 2; replay++) {
        second.send("a2ui", update);
        second.send("a2ui", { dataModelUpdate: { surfaceId: "session:s1", contents: [{ key: "answer", valueString: "complete replayed answer" }] } }, "cursor-10");
      }
      second.send("end", {}); second.close();
      await flush(); await o.task;
      expect(o.store.snapshot.cursor).toBe("cursor-10");
      expect(o.store.snapshot.components.size).toBe(1);
      expect(o.store.snapshot.components.get("root")?.component).toEqual({ ChatMessage: { messageId: "assistant-1", role: "assistant", status: "final", dataKey: "answer" } });
      expect(o.store.snapshot.data.get("answer")).toBe("complete replayed answer");
      expect(o.statuses.at(-1)?.status).toBe("ended");
      onlyGets(s);
    } finally { o.ctrl.abort(); await o.task; }
  });

  it("rejects old binding frames and cursors even for the same surface identity", async () => {
    const old = stream();
    const s = service([rendered(), old.response]);
    const o = observe(s.client);
    try {
      await flush();
      old.send("ready", { live: true }); await flush();
      const current = o.store.reset("session:s1", "current-cursor");
      o.store.apply(current, JSON.stringify({ dataModelUpdate: { surfaceId: "session:s1", contents: [{ key: "answer", valueString: "current binding answer" }] } }));
      const version = o.store.getVersion();
      old.send("a2ui", { dataModelUpdate: { surfaceId: "session:s1", contents: [{ key: "answer", valueString: "late old answer" }] } }, "old-cursor-99");
      old.send("cursor", {}, "old-cursor-100");
      await flush();
      expect(o.store.snapshot.cursor).toBe("current-cursor");
      expect(o.store.snapshot.data.get("answer")).toBe("current binding answer");
      expect(o.store.getVersion()).toBe(version);
      expect(s.requests).toHaveLength(2);
      onlyGets(s);
    } finally { o.ctrl.abort(); await o.task; }
  });

  it("does not subscribe or apply a late render after cancellation and rebinding", async () => {
    let resolve!: (value: Response) => void;
    const late = new Promise<Response>((yes) => { resolve = yes; });
    const s = service([late, historical()]);
    const store = new A2UIStore();
    const o = observe(s.client, new AbortController(), store);
    await flush();
    o.ctrl.abort();
    store.reset("session:s2", "new-session-cursor");
    resolve(rendered("cursor-99"));
    await flush(); await o.task;
    expect(s.requests).toHaveLength(1);
    expect(store.snapshot.surfaceId).toBe("session:s2");
    expect(store.snapshot.cursor).toBe("new-session-cursor");
    expect(store.snapshot.data.size).toBe(0);
    expect(o.statuses.map((entry) => entry.status)).toEqual(["loading"]);
    onlyGets(s);
  });
});
