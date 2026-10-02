import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render } from "@testing-library/react";
import { A2UIStore } from "./store";
import { Surface } from "./renderer";

const S = "session:s1";
const XSS = `<img src=x onerror="window.__pwned=1"><script>window.__pwned=2</script>`;

function build(components: unknown[], data: Record<string, string> = {}) {
  const store = new A2UIStore(S);
  const t = store.reset(S);
  store.apply(t, JSON.stringify({ beginRendering: { surfaceId: S, root: "root" } }));
  store.apply(t, JSON.stringify({ surfaceUpdate: { surfaceId: S, components } }));
  store.apply(t, JSON.stringify({ dataModelUpdate: { surfaceId: S, contents: Object.entries(data).map(([key, valueString]) => ({ key, valueString })) } }));
  return store;
}

afterEach(cleanup);

describe("Surface renderer", () => {
  it("renders hostile strings as text, never as markup", () => {
    const store = build(
      [
        { id: "msg:u", component: { ChatMessage: { messageId: "u", role: "user", status: "final", dataKey: "text:u" } } },
        { id: "msg:a", component: { ChatMessage: { messageId: "a", role: "assistant", status: "final", dataKey: "text:a" } } },
        { id: "call:c", component: { ToolCall: { callId: "c", name: XSS, status: "succeeded" } } },
        { id: "t", component: { Text: { value: XSS } } },
        { id: "root", component: { Column: { children: ["msg:u", "msg:a", "call:c", "t"] } } },
      ],
      { "text:u": XSS, "text:a": XSS },
    );
    const { container } = render(<Surface state={store.snapshot} />);
    expect(container.querySelector("img")).toBeNull();
    expect(container.querySelector("script")).toBeNull();
    expect(container.textContent).toContain(XSS);
    expect((window as unknown as { __pwned?: number }).__pwned).toBeUndefined();
  });

  it("shows a fixed error for unknown components without their content", () => {
    const store = build([
      { id: "x", component: { Html: { value: XSS } } },
      { id: "root", component: { Column: { children: ["x"] } } },
    ]);
    const { container, getByRole } = render(<Surface state={store.snapshot} />);
    expect(getByRole("alert").textContent).toBe("无法显示的组件");
    expect(container.textContent).not.toContain("onerror");
  });

  it("keeps Code invocation progress and rejects legacy session workflow nodes", () => {
    const store = build([
      { id: "inv:i1", component: { Invocation: { invocationId: "i1", parentCallId: "c1", agent: XSS, state: "running" } } },
      { id: "node:n1", component: { WorkflowNode: { nodeExecutionId: "n1", traceId: "t1", nodeId: "fetch", kind: "tool", state: "waiting" } } },
      { id: "root", component: { Column: { children: ["inv:i1", "node:n1"] } } },
    ]);
    const { container, getByRole } = render(<Surface state={store.snapshot} />);
    expect(container.textContent).toContain("子 Agent 调用");
    expect(container.textContent).toContain(XSS);
    expect(container.textContent).toContain("运行中");
    expect(container.textContent).not.toContain("工作流工具节点");
    expect(container.textContent).not.toContain("fetch");
    expect(container.querySelector("img")).toBeNull();
    expect(getByRole("alert").textContent).toBe("无法显示的组件");
  });

  it("exposes each message identity and projected role independently of shared content", () => {
    const roles = ["user", "assistant", "tool", "summary"];
    const store = build([
      ...roles.map((role) => ({ id: "msg:" + role, component: { ChatMessage: { messageId: "message-" + role, role, status: "final", dataKey: "text:" + role } } })),
      { id: "root", component: { Column: { children: roles.map((role) => "msg:" + role) } } },
    ], Object.fromEntries(roles.map((role) => ["text:" + role, "same visible content"])));
    const { getByRole, getAllByRole } = render(<Surface state={store.snapshot} />);
    expect(getAllByRole("article")).toHaveLength(4);
    for (const [role, label] of [["user", "用户"], ["assistant", "助手"], ["tool", "工具结果"], ["summary", "摘要"]]) {
      const message = getByRole("article", { name: label + "消息" });
      expect(message.getAttribute("data-message-id")).toBe("message-" + role);
      expect(message.textContent).toContain("same visible content");
    }
  });

  it("survives cycles in the component graph", () => {
    const store = build([
      { id: "a", component: { Column: { children: ["root"] } } },
      { id: "root", component: { Column: { children: ["a"] } } },
    ]);
    expect(() => render(<Surface state={store.snapshot} />)).not.toThrow();
  });
});
