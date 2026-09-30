import { readSSE, type SSEFrame } from "./sse";

/** Public error from the service: only the documented code reaches the UI. */
export class APIError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
  ) {
    super(code);
  }
}

export type SessionEntry = { sessionId: string; available: boolean };
export type TraceInfo = { traceId: string; state: string; kind: string; targetAgent: string; settled: boolean; hold: boolean; canResume: boolean };
/**
 * A call whose effect must be reconciled before resuming. Field names match
 * the reconcile request DTO. The service projects it only when its snapshot
 * exposes these identities; absent means nothing is shown.
 */
export type PendingReconciliation = { traceId: string; invocationId: string; toolCallId: string; observationId: string; observationVersion?: number };
export type Snapshot = {
  sessionId: string;
  revision: number;
  cursor: string;
  instanceId?: string;
  traces?: TraceInfo[];
  pendingReconciliations?: PendingReconciliation[];
};
export type WorkflowInfo = { name: string; version: string; description?: string; inputSchema?: unknown };
export type Attachment = { artifactId: string; mimeType: string; size: number; name?: string };
export type OperationStatus = { operationId: string; state: string; revision: number; error?: string };
export type ReconcileBody = { invocationId: string; toolCallId: string; observationId: string; observationVersion?: number; queryId?: string; evidenceRef?: string };
export type ContentBlock = { type: "text"; text: string } | { type: "attachment"; artifactId: string };
export type AgentInfo = { name: string; version: string; description?: string; kind: string };
export type Capabilities = { agents: AgentInfo[]; tools: string[]; unavailable: string[] };
export type Branch = { branchId: string; leafId: string; forkedAt?: string; active: boolean };

/** newIdempotencyKey is generated once per user action and reused on retry. */
export function newIdempotencyKey(): string {
  return crypto.randomUUID();
}

/**
 * Client talks to the same-origin service. The bearer lives only in this
 * object's memory: it is never written to storage, URLs or the DOM, and it is
 * sent via the Authorization header on every request, including SSE.
 */
export class Client {
  #token: string;
  constructor(token: string) {
    this.#token = token;
  }

  private async request(method: string, path: string, opts: { body?: unknown; raw?: { data: Blob; type: string }; key?: string; signal?: AbortSignal } = {}): Promise<Response> {
    const headers: Record<string, string> = { Authorization: "Bearer " + this.#token };
    if (opts.body !== undefined) headers["Content-Type"] = "application/json";
    if (opts.raw) headers["Content-Type"] = opts.raw.type;
    if (opts.key) headers["Idempotency-Key"] = opts.key;
    const resp = await fetch(path, {
      method,
      headers,
      body: opts.raw ? opts.raw.data : opts.body === undefined ? undefined : JSON.stringify(opts.body),
      signal: opts.signal,
      credentials: "omit",
      cache: "no-store",
      redirect: "error",
    });
    if (!resp.ok) {
      let code = "internal_error";
      try {
        const body = (await resp.json()) as { error?: { code?: unknown } };
        if (typeof body.error?.code === "string") code = body.error.code;
      } catch {
        /* non-JSON error body: keep the generic code */
      }
      throw new APIError(resp.status, code);
    }
    return resp;
  }

  private async json<T>(method: string, path: string, opts?: { body?: unknown; raw?: { data: Blob; type: string }; key?: string; signal?: AbortSignal }): Promise<T> {
    return (await (await this.request(method, path, opts)).json()) as T;
  }

  listSessions(signal?: AbortSignal) {
    return this.json<{ sessions: SessionEntry[]; next?: string }>("GET", "/v1/sessions", { signal });
  }
  createSession(workspace: string, key: string) {
    return this.json<Snapshot>("POST", "/v1/sessions", { body: { workspace, model: "default" }, key });
  }
  snapshot(sid: string, signal?: AbortSignal) {
    return this.json<Snapshot>("GET", `/v1/sessions/${enc(sid)}/snapshot`, { signal });
  }
  capabilities(sid: string, signal?: AbortSignal) {
    return this.json<Capabilities>("GET", `/v1/sessions/${enc(sid)}/capabilities`, { signal });
  }
  submitPrompt(sid: string, content: ContentBlock[], targetAgent: string, key: string) {
    const body: Record<string, unknown> = { kind: "prompt", content };
    if (targetAgent) body.targetAgent = targetAgent;
    return this.json<{ inputId: string; traceId: string }>("POST", `/v1/sessions/${enc(sid)}/inputs`, { body, key });
  }
  cancelTrace(sid: string, tid: string, key: string) {
    return this.json<{ operationId: string }>("POST", `/v1/sessions/${enc(sid)}/traces/${enc(tid)}/cancel`, { body: {}, key });
  }
  resumeTrace(sid: string, tid: string, expectedRevision: number, key: string) {
    safeRevision(expectedRevision);
    return this.json<{ operationId: string }>("POST", `/v1/sessions/${enc(sid)}/traces/${enc(tid)}/resume`, { body: { expectedRevision }, key });
  }
  continueQueue(sid: string, traceIds: string[], key: string) {
    return this.json<{ operationId: string }>("POST", `/v1/sessions/${enc(sid)}/queue/continue`, { body: { traceIds }, key });
  }
  reconcile(sid: string, tid: string, body: ReconcileBody, expectedRevision: number, key: string) {
    safeRevision(expectedRevision);
    return this.json<{ operationId: string }>("POST", `/v1/sessions/${enc(sid)}/traces/${enc(tid)}/reconcile`, { body: { ...body, expectedRevision }, key });
  }
  operation(sid: string, oid: string) {
    return this.json<OperationStatus>("GET", `/v1/sessions/${enc(sid)}/operations/${enc(oid)}`);
  }
  workflows(sid: string, signal?: AbortSignal) {
    return this.json<{ workflows: WorkflowInfo[] }>("GET", `/v1/sessions/${enc(sid)}/workflows`, { signal });
  }
  /** uploadAttachment saves raw bytes only; it never submits input. */
  uploadAttachment(sid: string, data: Blob, mimeType: string, name: string, key: string) {
    const q = name ? `?name=${encodeURIComponent(name)}` : "";
    return this.json<Attachment>("POST", `/v1/sessions/${enc(sid)}/attachments${q}`, { raw: { data, type: mimeType }, key });
  }
  respond(sid: string, iid: string, decision: string, expectedRevision: number, instanceId: string, key: string) {
    safeRevision(expectedRevision);
    return this.json<{ operationId: string }>("POST", `/v1/sessions/${enc(sid)}/interactions/${enc(iid)}/responses`, {
      body: { decision, expectedRevision, instanceId },
      key,
    });
  }
  branches(sid: string, signal?: AbortSignal) {
    return this.json<{ branches: Branch[] }>("GET", `/v1/sessions/${enc(sid)}/branches`, { signal });
  }
  /** fork sends "summarize" only when requested, so older services see the original DTO. */
  fork(sid: string, branchId: string, fromEntryId: string, summarize = false) {
    const body: Record<string, unknown> = { branchId, fromEntryId };
    if (summarize) body.summarize = true;
    return this.json<{ branchId: string }>("POST", `/v1/sessions/${enc(sid)}/branches`, { body });
  }
  activate(sid: string, bid: string) {
    return this.json<{ branchId: string }>("POST", `/v1/sessions/${enc(sid)}/branches/${enc(bid)}/activate`, { body: {} });
  }
  compact(sid: string, key: string) {
    return this.json<{ operationId: string }>("POST", `/v1/sessions/${enc(sid)}/compactions`, { body: { reason: "manual" }, key });
  }

  /** render returns the current surface as JSONL lines and its cursor. */
  async render(sid: string, signal: AbortSignal): Promise<{ lines: string[]; cursor: string }> {
    const resp = await this.request("GET", `/v1/sessions/${enc(sid)}/render`, { signal });
    const text = await resp.text();
    return { lines: text.split("\n").filter((l) => l !== ""), cursor: resp.headers.get("X-Session-Cursor") ?? "" };
  }

  /** uiEvents streams SSE frames after cursor until the stream ends or aborts. */
  async uiEvents(sid: string, cursor: string, onFrame: (f: SSEFrame) => void, signal: AbortSignal) {
    const q = cursor ? `?cursor=${encodeURIComponent(cursor)}` : "";
    const resp = await this.request("GET", `/v1/sessions/${enc(sid)}/ui/events${q}`, { signal });
    if (!resp.body) throw new APIError(0, "resource_unavailable");
    await readSSE(resp.body, onFrame, signal);
  }
}

const enc = encodeURIComponent;

/** Revisions beyond the safe integer range are refused rather than rounded. */
function safeRevision(n: number) {
  if (!Number.isSafeInteger(n) || n < 0) throw new APIError(0, "invalid_argument");
}

/** errorText maps public error codes to fixed Chinese messages. */
export function errorText(err: unknown): string {
  const code = err instanceof APIError ? err.code : "network";
  const map: Record<string, string> = {
    unauthenticated: "令牌无效或已过期，请重新输入。",
    permission_denied: "没有权限执行此操作。",
    not_found: "对象不存在。",
    invalid_argument: "请求参数无效。",
    state_conflict: "状态已变化，请刷新后重试。",
    idempotency_conflict: "该操作与之前的请求冲突。",
    resync_required: "需要重新同步。",
    unsupported_capability: "当前服务不支持此能力。",
    budget_exhausted: "预算已耗尽。",
    storage_unavailable: "存储暂不可用。",
    resource_unavailable: "服务资源暂不可用。",
    network: "网络连接失败。",
  };
  return map[code] ?? "服务内部错误。";
}
