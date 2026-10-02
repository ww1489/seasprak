import type { ReactNode } from "react";
import type { ApprovalComp } from "./protocol";
import type { SurfaceState } from "./store";
import StreamingText from "@/components/bui/StreamingText";
import TaskRow from "@/components/bui/TaskRow";
import ToolChip from "@/components/bui/ToolChip";
import { AgentSection, UserBubble } from "@/components/bui/Chat";

/** Trusted page controls injected into product components. */
export type RenderControls = {
  taskAction?: (traceId: string, state: string) => ReactNode;
  approval?: (c: ApprovalComp) => ReactNode;
};

const ROLE_LABEL: Record<string, string> = { assistant: "助手", tool: "工具结果", summary: "摘要" };
const MAX_DEPTH = 32;

/**
 * Renders a surface by whitelist. Every string reaches the DOM as a React text
 * node; there is no HTML injection path. Unknown components show a fixed
 * error text instead of their content.
 */
export function Surface({ state, controls = {} }: { state: SurfaceState; controls?: RenderControls }) {
  if (!state.root) return null;
  return <div className="flex flex-col gap-2.5">{renderNode(state, state.root, controls, 0, new Set())}</div>;
}

function renderNode(s: SurfaceState, id: string, controls: RenderControls, depth: number, seen: Set<string>): ReactNode {
  const c = s.components.get(id);
  if (!c || depth > MAX_DEPTH || seen.has(id)) return null;
  const path = new Set(seen).add(id);
  const kids = (children: string[]) => children.map((child) => <Keyed key={child}>{renderNode(s, child, controls, depth + 1, path)}</Keyed>);
  const v = c.component;
  if ("Column" in v) return <div className="flex flex-col gap-2.5">{kids(v.Column.children)}</div>;
  if ("Row" in v) return <div className="flex flex-row flex-wrap gap-2">{kids(v.Row.children)}</div>;
  if ("Card" in v) return <div className="rounded-card bg-surface p-3 shadow-card">{kids(v.Card.children)}</div>;
  if ("Text" in v) {
    const text = v.Text.dataKey ? (s.data.get(v.Text.dataKey) ?? "") : (v.Text.value ?? "");
    const cls = v.Text.usageHint === "title" ? "text-[14px] font-semibold" : v.Text.usageHint === "caption" ? "text-[12px] text-ink-3" : "text-[13px]";
    return <p className={`${cls} whitespace-pre-wrap break-words`}>{text}</p>;
  }
  if ("ChatMessage" in v) {
    const m = v.ChatMessage;
    const text = s.data.get(m.dataKey) ?? "";
    return (
      <article aria-label={(m.role === "user" ? "用户" : ROLE_LABEL[m.role]) + "消息"} data-message-id={m.messageId}>
        {m.role === "user" ? <UserBubble>{text}</UserBubble> : (
          <AgentSection label={ROLE_LABEL[m.role] ?? m.role} sub={m.status === "streaming" ? "生成中" : undefined}>
            <StreamingText text={text} streaming={m.status === "streaming"} />
          </AgentSection>
        )}
      </article>
    );
  }
  if ("ToolCall" in v) {
    const t = v.ToolCall;
    return <ToolChip name={t.name} status={t.status} output={s.data.get("output:" + t.callId)} />;
  }
  if ("Task" in v) {
    const t = v.Task;
    return <TaskRow label="任务" meta={t.targetAgent || t.traceId} state={t.state} action={controls.taskAction?.(t.traceId, t.state)} />;
  }
  if ("Approval" in v) return controls.approval ? controls.approval(v.Approval) : null;
  if ("Invocation" in v) {
    const i = v.Invocation;
    return <TaskRow label="子 Agent 调用" meta={i.agent || i.invocationId} state={i.state} />;
  }
  return (
    <p role="alert" className="text-[12px] text-red">
      无法显示的组件
    </p>
  );
}

function Keyed({ children }: { children: ReactNode }) {
  return <>{children}</>;
}
