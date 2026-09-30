import { describe, expect, it } from "vitest";
import { SSEParser, type SSEFrame } from "./sse";

const enc = new TextEncoder();

function parse(chunks: (string | Uint8Array)[]): SSEFrame[] {
  const out: SSEFrame[] = [];
  const p = new SSEParser((f) => out.push(f));
  for (const c of chunks) p.push(typeof c === "string" ? enc.encode(c) : c);
  p.end();
  return out;
}

describe("SSEParser", () => {
  it("joins multi-line data and keeps the id only when present", () => {
    const frames = parse(["event: a2ui\nid: c1\ndata: line1\ndata: line2\n\n", "event: a2ui\ndata: x\n\n"]);
    expect(frames).toEqual([
      { event: "a2ui", id: "c1", data: "line1\nline2" },
      { event: "a2ui", data: "x" },
    ]);
  });

  it("handles CRLF, including a CR/LF pair split across chunks", () => {
    const frames = parse(["event: cursor\r", "\nid: c2\r\ndata: {}\r\n\r", "\n", "data: tail\r\n\r\n"]);
    expect(frames).toEqual([
      { event: "cursor", id: "c2", data: "{}" },
      { event: "message", data: "tail" },
    ]);
  });

  it("decodes UTF-8 characters split across chunks", () => {
    const bytes = enc.encode("event: a2ui\ndata: 你好🙂\n\n");
    const cut = bytes.indexOf(0xe4) + 1; // split inside the first CJK code point
    const emoji = bytes.indexOf(0xf0) + 2; // split inside the 4-byte emoji
    const frames = parse([bytes.slice(0, cut), bytes.slice(cut, emoji), bytes.slice(emoji)]);
    expect(frames).toEqual([{ event: "a2ui", data: "你好🙂" }]);
  });

  it("ignores comments/heartbeats and drops an unterminated trailing frame", () => {
    const frames = parse([": heartbeat\n\n", "event: ready\ndata: {}\n\n", "event: a2ui\ndata: half"]);
    expect(frames).toEqual([{ event: "ready", data: "{}" }]);
  });

  it("accepts a lone CR as a line terminator", () => {
    expect(parse(["event: end\rdata: {}\r\r"])).toEqual([{ event: "end", data: "{}" }]);
  });
});
