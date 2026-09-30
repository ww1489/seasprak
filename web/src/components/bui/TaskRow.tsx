/*
 * Adapted from Beautiful UI "Task Rows" (https://www.beautifului.dev/),
 * MIT License, Copyright (c) 2026 Shane Levine. See web/licenses/beautifului-MIT.txt.
 * Local changes: the scripted tick sequence, sample rows and expandable detail
 * list are removed. One row is rendered from a product Task (trace) with the
 * original SpinnerRing / Badge / status pill visuals, Chinese labels and an
 * optional trusted action slot (cancel) supplied by the page, never by the model.
 */
import type { ReactNode } from "react";

function SpinnerRing({ active }: { active?: boolean }) {
  const size = 24,
    stroke = 2;
  const r = (size - stroke) / 2;
  const c = 2 * Math.PI * r;
  return (
    <span className="relative inline-flex shrink-0 items-center justify-center" style={{ width: size, height: size }}>
      <svg width={size} height={size} className="absolute inset-0" style={active ? { animation: "spin 1.1s linear infinite" } : undefined}>
        <circle cx={size / 2} cy={size / 2} r={r} fill="none" stroke="var(--line)" strokeWidth={stroke} />
        {active && (
          <circle
            cx={size / 2}
            cy={size / 2}
            r={r}
            fill="none"
            stroke="var(--ink-3)"
            strokeWidth={stroke}
            strokeLinecap="round"
            strokeDasharray={`${c * 0.28} ${c * 0.72}`}
          />
        )}
      </svg>
    </span>
  );
}

function Badge({ tone, children }: { tone: "red" | "green"; children: ReactNode }) {
  return (
    <span
      className={`flex size-5.5 shrink-0 items-center justify-center rounded-full text-white ${tone === "red" ? "bg-red" : "bg-green"}`}
      style={{ animation: "pop-in 300ms cubic-bezier(0.23,1,0.32,1) both" }}
    >
      {children}
    </span>
  );
}

const XIcon = (
  <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="3.5" strokeLinecap="round">
    <path d="M18 6L6 18M6 6l12 12" />
  </svg>
);
const CheckIcon = (
  <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="3.5" strokeLinecap="round" strokeLinejoin="round">
    <path d="M20 6L9 17l-5-5" />
  </svg>
);

const STATE_LABEL: Record<string, string> = {
  queued: "排队中",
  running: "运行中",
  paused: "已暂停",
  cancelling: "取消中",
  completed: "已完成",
  failed: "失败",
  cancelled: "已取消",
  interrupted: "已中断",
  accepted: "已受理",
  waiting: "等待审批",
};

export function stateLabel(state: string): string {
  return STATE_LABEL[state] ?? state;
}

export default function TaskRow({
  label,
  meta,
  state,
  action,
}: {
  label: string;
  meta: string;
  state: string;
  action?: ReactNode;
}) {
  const badge =
    state === "completed" ? (
      <Badge tone="green">{CheckIcon}</Badge>
    ) : state === "failed" || state === "cancelled" ? (
      <Badge tone="red">{XIcon}</Badge>
    ) : (
      <SpinnerRing active={state === "running" || state === "cancelling"} />
    );
  const pill =
    state === "completed" ? (
      <span className="inline-flex h-5.5 items-center rounded-full bg-green-tint px-2 text-[11.5px] font-medium text-green">{stateLabel(state)}</span>
    ) : state === "failed" || state === "cancelled" ? (
      <span className="inline-flex h-5.5 items-center rounded-full bg-red-tint px-2 text-[11.5px] font-medium text-red">{stateLabel(state)}</span>
    ) : (
      <span className="inline-flex h-5.5 items-center rounded-full bg-field px-2 text-[11.5px] font-medium text-ink-2">{stateLabel(state)}</span>
    );
  return (
    <div
      className="flex h-11 w-full items-center gap-2.5 overflow-hidden rounded-[22px] bg-surface px-2.5 shadow-card"
      style={{ animation: "fade-up 450ms cubic-bezier(0.23,1,0.32,1) both" }}
    >
      <span className="flex size-6 shrink-0 items-center justify-center">{badge}</span>
      <span className="min-w-0 flex-1 truncate text-[13px] font-medium text-ink">{label}</span>
      <span className="truncate text-[12.5px] text-ink-2 tabular-nums">{meta}</span>
      {pill}
      {action}
    </div>
  );
}
