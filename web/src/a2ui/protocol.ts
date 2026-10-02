// A2UI presentation protocol of docs/pi-eino-dev-plan/13-p3-web-contract.md
// §7.1. Frames are parsed defensively: anything outside this whitelist is
// rejected rather than rendered.

export type TextComp = { value?: string; dataKey?: string; usageHint?: string };
export type Children = { children: string[] };
export type ChatMessageComp = {
  messageId: string;
  role: "user" | "assistant" | "tool" | "summary";
  status: "final" | "streaming";
  dataKey: string;
};
export type ToolCallComp = { callId: string; name: string; status: string };
export type TaskComp = { traceId: string; state: string; targetAgent: string; settled: boolean };
export type ApprovalComp = {
  interactionId: string;
  traceId: string;
  question: string;
  options: string[];
  instanceId: string;
};

export type InvocationComp = { invocationId: string; parentCallId: string; agent: string; state: string };

export type ComponentValue =
  | { Text: TextComp }
  | { Column: Children }
  | { Card: Children }
  | { Row: Children }
  | { ChatMessage: ChatMessageComp }
  | { ToolCall: ToolCallComp }
  | { Task: TaskComp }
  | { Approval: ApprovalComp }
  | { Invocation: InvocationComp }
  | { Unknown: string };

export type Component = { id: string; component: ComponentValue };

export type A2UIMessage =
  | { beginRendering: { surfaceId: string; root: string } }
  | { surfaceUpdate: { surfaceId: string; components: Component[] } }
  | { dataModelUpdate: { surfaceId: string; contents: { key: string; valueString: string }[] } }
  | { deleteSurface: { surfaceId: string } };

const KNOWN = new Set(["Text", "Column", "Card", "Row", "ChatMessage", "ToolCall", "Task", "Approval", "Invocation"]);

const isObj = (v: unknown): v is Record<string, unknown> => typeof v === "object" && v !== null && !Array.isArray(v);
const str = (v: unknown): string => (typeof v === "string" ? v : "");
const strs = (v: unknown): string[] => (Array.isArray(v) ? v.filter((x): x is string => typeof x === "string") : []);

function parseComponent(raw: unknown): Component | null {
  if (!isObj(raw) || typeof raw.id !== "string" || !isObj(raw.component)) return null;
  const keys = Object.keys(raw.component);
  if (keys.length !== 1) return null;
  const kind = keys[0];
  const v = raw.component[kind];
  if (!KNOWN.has(kind) || !isObj(v)) return { id: raw.id, component: { Unknown: kind } };
  switch (kind) {
    case "Text":
      return { id: raw.id, component: { Text: { value: str(v.value), dataKey: str(v.dataKey), usageHint: str(v.usageHint) } } };
    case "Column":
    case "Card":
    case "Row":
      return { id: raw.id, component: { [kind]: { children: strs(v.children) } } as ComponentValue };
    case "ChatMessage": {
      const role = str(v.role);
      if (!["user", "assistant", "tool", "summary"].includes(role)) return { id: raw.id, component: { Unknown: kind } };
      return {
        id: raw.id,
        component: {
          ChatMessage: {
            messageId: str(v.messageId),
            role: role as ChatMessageComp["role"],
            status: v.status === "streaming" ? "streaming" : "final",
            dataKey: str(v.dataKey),
          },
        },
      };
    }
    case "ToolCall":
      return { id: raw.id, component: { ToolCall: { callId: str(v.callId), name: str(v.name), status: str(v.status) } } };
    case "Task":
      return {
        id: raw.id,
        component: { Task: { traceId: str(v.traceId), state: str(v.state), targetAgent: str(v.targetAgent), settled: v.settled === true } },
      };
    case "Invocation":
      return {
        id: raw.id,
        component: { Invocation: { invocationId: str(v.invocationId), parentCallId: str(v.parentCallId), agent: str(v.agent), state: str(v.state) } },
      };
    default:
      return {
        id: raw.id,
        component: {
          Approval: {
            interactionId: str(v.interactionId),
            traceId: str(v.traceId),
            question: str(v.question),
            options: strs(v.options),
            instanceId: str(v.instanceId),
          },
        },
      };
  }
}

/** parseMessage returns null for anything that is not exactly one known envelope. */
export function parseMessage(text: string): A2UIMessage | null {
  let raw: unknown;
  try {
    raw = JSON.parse(text);
  } catch {
    return null;
  }
  if (!isObj(raw) || Object.keys(raw).length !== 1) return null;
  const [kind] = Object.keys(raw);
  const v = raw[kind];
  if (!isObj(v) || typeof v.surfaceId !== "string") return null;
  switch (kind) {
    case "beginRendering":
      return typeof v.root === "string" ? { beginRendering: { surfaceId: v.surfaceId, root: v.root } } : null;
    case "surfaceUpdate": {
      if (!Array.isArray(v.components)) return null;
      const components = v.components.map(parseComponent).filter((c): c is Component => c !== null);
      return { surfaceUpdate: { surfaceId: v.surfaceId, components } };
    }
    case "dataModelUpdate": {
      if (!Array.isArray(v.contents)) return null;
      const contents = v.contents
        .filter(isObj)
        .filter((c) => typeof c.key === "string")
        .map((c) => ({ key: c.key as string, valueString: str(c.valueString) }));
      return { dataModelUpdate: { surfaceId: v.surfaceId, contents } };
    }
    case "deleteSurface":
      return { deleteSurface: { surfaceId: v.surfaceId } };
  }
  return null;
}
