export type SSEFrame = { event: string; data: string; id?: string };

/**
 * SSEParser implements the WHATWG event-stream framing over arbitrary byte
 * chunks: CR, LF and CRLF line endings (including CRLF split across chunks),
 * multi-line data joined with "\n", comments, and UTF-8 sequences split across
 * chunks (TextDecoder with stream:true). Only frames with an explicit event
 * type or data are emitted; the id is reported only when the frame had one.
 */
export class SSEParser {
  private decoder = new TextDecoder("utf-8");
  private buf = "";
  private pendingCR = false;
  private event = "";
  private data: string[] = [];
  private id: string | undefined;
  private hasData = false;

  constructor(private readonly onFrame: (f: SSEFrame) => void) {}

  push(chunk: Uint8Array) {
    this.feed(this.decoder.decode(chunk, { stream: true }));
  }

  /** end flushes the decoder; an unterminated trailing frame is discarded. */
  end() {
    this.feed(this.decoder.decode());
  }

  private feed(text: string) {
    let s = text;
    if (this.pendingCR) {
      this.pendingCR = false;
      if (s.startsWith("\n")) s = s.slice(1);
    }
    this.buf += s;
    let start = 0;
    for (let i = 0; i < this.buf.length; i++) {
      const ch = this.buf[i];
      if (ch !== "\n" && ch !== "\r") continue;
      this.line(this.buf.slice(start, i));
      if (ch === "\r") {
        if (i + 1 < this.buf.length) {
          if (this.buf[i + 1] === "\n") i++;
        } else {
          this.pendingCR = true;
        }
      }
      start = i + 1;
    }
    this.buf = this.buf.slice(start);
  }

  private line(l: string) {
    if (l === "") {
      if (this.hasData || this.event) {
        const frame: SSEFrame = { event: this.event || "message", data: this.data.join("\n") };
        if (this.id !== undefined) frame.id = this.id;
        this.onFrame(frame);
      }
      this.event = "";
      this.data = [];
      this.id = undefined;
      this.hasData = false;
      return;
    }
    if (l.startsWith(":")) return;
    const colon = l.indexOf(":");
    const field = colon < 0 ? l : l.slice(0, colon);
    let value = colon < 0 ? "" : l.slice(colon + 1);
    if (value.startsWith(" ")) value = value.slice(1);
    switch (field) {
      case "event":
        this.event = value;
        break;
      case "data":
        this.data.push(value);
        this.hasData = true;
        break;
      case "id":
        if (!value.includes("\0")) this.id = value;
        break;
    }
  }
}

/** readSSE pumps a fetch body through the parser until it ends or aborts. */
export async function readSSE(body: ReadableStream<Uint8Array>, onFrame: (f: SSEFrame) => void, signal: AbortSignal) {
  const parser = new SSEParser(onFrame);
  const reader = body.getReader();
  const stop = () => void reader.cancel().catch(() => undefined);
  signal.addEventListener("abort", stop, { once: true });
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done || signal.aborted) break;
      parser.push(value);
    }
    if (!signal.aborted) parser.end();
  } finally {
    signal.removeEventListener("abort", stop);
  }
}
