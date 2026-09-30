import { createServer, request as httpRequest, type IncomingHttpHeaders, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import { request as httpsRequest } from "node:https";
import { readFileSync } from "node:fs";
import type { AddressInfo } from "node:net";
import { once } from "node:events";
import { join } from "node:path";

export type LiveConnection = { model: string; endpoint: string; key: string };
export type ChatRequest = { stream: boolean; text: string; body: unknown };
export type StreamGate = { reached: Promise<void>; open: () => void };
export type StreamShape = { status: number; contentType: string; contentEncoding: string; bytes: number; chunks: number; frames: number; contentFrames: number; ended: boolean };

type PendingGate = StreamGate & { release: Promise<void>; hit: () => void };

/** loadOpenAI reads only the approved local test file and never logs values. */
export function loadOpenAI(repoRoot: string): LiveConnection {
  const env: Record<string, string> = {};
  for (const source of readFileSync(join(repoRoot, ".test_env"), "utf8").split(/\r?\n/)) {
    const line = source.trim();
    if (!line || line.startsWith("#") || !line.includes("=")) continue;
    const at = line.indexOf("=");
    const key = line.slice(0, at).trim().replace(/^export\s+/, "");
    let value = line.slice(at + 1).trim();
    if ((value.startsWith('"') && value.endsWith('"')) || (value.startsWith("'") && value.endsWith("'"))) value = value.slice(1, -1);
    env[key] = value;
  }
  const model = env.OPENAI_MODEL?.trim();
  const key = env.OPENAI_API_KEY?.trim();
  let endpoint = env.OPENAI_BASE_URL?.trim().replace(/\/+$/, "");
  if (!model || !key || !endpoint) throw new Error(".test_env 中 OPENAI live 配置不完整");
  const parsed = new URL(endpoint);
  if (!['http:', 'https:'].includes(parsed.protocol) || !parsed.host || parsed.username || parsed.password || parsed.search || parsed.hash) {
    throw new Error(".test_env 中 OPENAI_BASE_URL 不安全");
  }
  if (!parsed.pathname.endsWith("/v1")) endpoint += "/v1";
  return { model, endpoint, key };
}

/**
 * LiveOpenAIProxy forwards every request to the configured real OpenAI-compatible
 * endpoint. It records request shape and may delay/rechunk bytes, but never
 * fabricates or edits provider response content.
 */
export class LiveOpenAIProxy {
  requests: ChatRequest[] = [];
  statuses: number[] = [];
  shapes: StreamShape[] = [];
  private server: Server | undefined;
  private nextGate: PendingGate | undefined;
  url = "";

  constructor(private readonly live: LiveConnection) {}

  count(pred: (r: ChatRequest) => boolean = () => true) {
    return this.requests.filter(pred).length;
  }

  gateNextContentFrame(): StreamGate {
    if (this.nextGate) throw new Error("stream gate is already pending");
    let hit!: () => void;
    let release!: () => void;
    const reached = new Promise<void>((ok) => (hit = ok));
    const wait = new Promise<void>((ok) => (release = ok));
    const gate: PendingGate = { reached, release: wait, hit, open: release };
    this.nextGate = gate;
    return { reached, open: release };
  }

  async start() {
    this.server = createServer((req, res) => void this.serve(req, res));
    await new Promise<void>((ok) => this.server!.listen(0, "127.0.0.1", ok));
    const address = this.server.address() as AddressInfo;
    this.url = `http://127.0.0.1:${address.port}/v1`;
  }

  async stop() {
    this.nextGate?.open();
    this.server?.closeAllConnections();
    await new Promise<void>((ok) => (this.server ? this.server.close(() => ok()) : ok()));
  }

  private async serve(req: IncomingMessage, res: ServerResponse) {
    try {
      const chunks: Buffer[] = [];
      for await (const chunk of req) chunks.push(Buffer.from(chunk as Uint8Array));
      if (req.method !== "POST" || req.url !== "/v1/chat/completions") {
        res.writeHead(404).end();
        return;
      }
      const raw = Buffer.concat(chunks);
      const body = JSON.parse(raw.toString("utf8")) as { stream?: boolean; messages?: unknown };
      const call: ChatRequest = { stream: body.stream === true, text: JSON.stringify(body.messages ?? []), body };
      this.requests.push(call);
      const gate = call.stream ? this.nextGate : undefined;
      if (gate) this.nextGate = undefined;
      await this.forward(raw, req.headers, res, gate);
    } catch {
      if (!res.headersSent) res.writeHead(502, { "Content-Type": "application/json" });
      if (!res.writableEnded) res.end('{"error":{"message":"live proxy failure","code":"service_unavailable"}}');
    }
  }

  private async forward(raw: Buffer, incoming: IncomingHttpHeaders, res: ServerResponse, gate?: PendingGate) {
    const target = new URL(this.live.endpoint.replace(/\/+$/, "") + "/chat/completions");
    const headers: Record<string, string | string[]> = {};
    for (const [name, value] of Object.entries(incoming)) {
      if (value !== undefined && !["host", "connection", "keep-alive", "transfer-encoding"].includes(name.toLowerCase())) headers[name] = value;
    }
    if (typeof headers.authorization !== "string" || !headers.authorization.startsWith("Bearer ")) {
      throw new Error("cmd/web did not provide the configured credential");
    }
    headers["accept-encoding"] = "identity";
    headers["content-length"] = String(raw.length);
    const request = target.protocol === "https:" ? httpsRequest : httpRequest;
    await new Promise<void>((resolve, reject) => {
      const upstream = request(target, { method: "POST", headers }, async (response) => {
        this.statuses.push(response.statusCode ?? 0);
        const shape: StreamShape = {
          status: response.statusCode ?? 0,
          contentType: String(response.headers["content-type"] ?? ""),
          contentEncoding: String(response.headers["content-encoding"] ?? ""),
          bytes: 0,
          chunks: 0,
          frames: 0,
          contentFrames: 0,
          ended: false,
        };
        this.shapes.push(shape);
        try {
          const responseHeaders = { ...response.headers };
          delete responseHeaders.connection;
          delete responseHeaders["keep-alive"];
          delete responseHeaders["transfer-encoding"];
          delete responseHeaders["content-length"];
          res.writeHead(response.statusCode ?? 502, responseHeaders);
          let pending = "";
          for await (const source of response) {
            const bytes = Buffer.from(source as Uint8Array);
            shape.bytes += bytes.length;
            shape.chunks++;
            for (let offset = 0; offset < bytes.length; offset += 64) {
              const part = bytes.subarray(offset, Math.min(offset + 64, bytes.length));
              await writeRechunked(res, part);
              pending += part.toString("utf8");
              if (gate) {
                const split = pending.split(/\r?\n\r?\n/);
                pending = split.pop() ?? "";
                shape.frames += split.length;
                shape.contentFrames += split.filter(hasContentDelta).length;
                if (shape.contentFrames > 0) {
                  gate.hit();
                  await gate.release;
                  gate = undefined;
                }
              }
            }
          }
          shape.ended = true;
          res.end();
          resolve();
        } catch (err) {
          reject(err);
        }
      });
      upstream.once("error", reject);
      upstream.end(raw);
    });
  }
}

function hasContentDelta(frame: string): boolean {
  for (const line of frame.split(/\r?\n/)) {
    if (!line.startsWith("data:")) continue;
    const data = line.slice(5).trim();
    if (!data || data === "[DONE]") continue;
    try {
      const payload = JSON.parse(data) as { choices?: { delta?: { content?: unknown } }[] };
      if (payload.choices?.some((choice) => typeof choice.delta?.content === "string" && choice.delta.content.length > 0)) return true;
    } catch {
      // Forward malformed provider data unchanged; it simply cannot satisfy the gate.
    }
  }
  return false;
}

async function writeRechunked(res: ServerResponse, bytes: Buffer) {
  let split = 0;
  for (let i = 0; i < bytes.length; i++) {
    if ((bytes[i] & 0xe0) === 0xc0 || (bytes[i] & 0xf0) === 0xe0 || (bytes[i] & 0xf8) === 0xf0) {
      split = i + 1;
      break;
    }
  }
  const parts = split > 0 && split < bytes.length ? [bytes.subarray(0, split), bytes.subarray(split)] : [bytes];
  for (const part of parts) {
    if (!res.write(part)) await once(res, "drain");
    await new Promise((ok) => setTimeout(ok, 1));
  }
}
