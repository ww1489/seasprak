/*
 * Adapted from Beautiful UI "Tool Chips" (https://www.beautifului.dev/),
 * MIT License, Copyright (c) 2026 Shane Levine. See web/licenses/beautifului-MIT.txt.
 * Local changes: the scripted step timer, sample rows, file-diff chips and the
 * body-portal diff preview are removed. One row renders a product ToolCall:
 * the original row + chip visuals show the tool name and status, and the
 * expandable detail shows the bounded plain-text output preview.
 */
import { useState } from "react";

const STATUS: Record<string, string> = {
  requested: "已请求",
  running: "执行中",
  succeeded: "成功",
  failed: "失败",
  denied: "已拒绝",
  cancelled: "已取消",
};

export default function ToolChip({ name, status, output }: { name: string; status: string; output?: string }) {
  const [open, setOpen] = useState(false);
  const hasOutput = output !== undefined && output !== "";
  return (
    <div style={{ animation: "fade-up 300ms cubic-bezier(0.23,1,0.32,1) both" }}>
      <button
        type="button"
        aria-expanded={open}
        disabled={!hasOutput}
        onClick={() => setOpen((v) => !v)}
        className="group/row flex h-7 w-full min-w-0 items-center gap-2 rounded-control px-[3px] text-left transition-colors duration-100 enabled:hover:bg-hover-2"
      >
        <span className="relative flex size-4 shrink-0 items-center justify-center text-ink-3">
          <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
            <path d="M4 17l6-5-6-5M12 19h8" />
          </svg>
        </span>
        <span className="shrink-0 text-[12.5px] font-medium text-ink">工具调用</span>
        <span className="inline-flex h-5.5 min-w-0 flex-1 items-center truncate rounded-chip bg-field px-1.5 font-mono text-[11.5px] text-ink-2 shadow-hairline">
          {name}
        </span>
        <span className="shrink-0 text-[11.5px] text-ink-3">{STATUS[status] ?? status}</span>
      </button>
      {hasOutput && (
        <div
          className="grid transition-[grid-template-rows,opacity] duration-300"
          style={{ gridTemplateRows: open ? "1fr" : "0fr", opacity: open ? 1 : 0 }}
        >
          <div className="min-h-0 overflow-hidden">
            <pre className="mt-0.5 mb-1 ml-2 max-h-60 overflow-auto border-l border-line py-0.5 pl-3.5 font-mono text-[11.5px] leading-[1.6] whitespace-pre-wrap break-words text-ink-2">
              {output}
            </pre>
          </div>
        </div>
      )}
    </div>
  );
}
