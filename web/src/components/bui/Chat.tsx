/*
 * Adapted from Beautiful UI "Chat" (https://www.beautifului.dev/),
 * MIT License, Copyright (c) 2026 Shane Levine. See web/licenses/beautifului-MIT.txt.
 * Local changes: split into the right-aligned user bubble, the assistant
 * section and the composer; the scripted reply phases, header tabs and icon
 * actions are removed. Text is caller-supplied and rendered as React text.
 * The composer is also used in place of "Prompt Bar", whose dependencies
 * (@/components/primitives/GlideMenu, glimm) could not be obtained.
 */
import { useRef, type ReactNode } from "react";

export function UserBubble({ children }: { children: ReactNode }) {
  return (
    <div className="flex justify-end pl-14">
      <div className="rounded-xl bg-field px-3 py-1.5 text-[13px] leading-[1.4] whitespace-pre-wrap break-words text-ink">{children}</div>
    </div>
  );
}

export function AgentSection({ label, sub, children }: { label: string; sub?: string; children: ReactNode }) {
  return (
    <div className="flex w-full flex-col gap-1.5" style={{ animation: "fade-up 400ms cubic-bezier(0.23,1,0.32,1) both" }}>
      <div className="flex items-center gap-1 text-[12px] leading-[1.3]">
        <span className="font-medium text-ink">{label}</span>
        {sub && <span className="text-ink-2">{sub}</span>}
      </div>
      {children}
    </div>
  );
}

export function Composer({
  value,
  onChange,
  onSend,
  disabled,
  placeholder,
  extra,
}: {
  value: string;
  onChange: (v: string) => void;
  onSend: () => void;
  disabled?: boolean;
  placeholder: string;
  extra?: ReactNode;
}) {
  const inputRef = useRef<HTMLTextAreaElement>(null);
  const canSend = !disabled && value.trim().length > 0;
  return (
    <div
      role="presentation"
      onClick={() => inputRef.current?.focus()}
      className="flex cursor-text flex-col gap-2 rounded-control border border-line bg-field p-2.5 shadow-[0_1px_2px_rgba(0,0,0,0.035)] focus-within:border-line-strong"
    >
      <textarea
        ref={inputRef}
        rows={2}
        value={value}
        onChange={(event) => onChange(event.target.value)}
        onKeyDown={(event) => {
          if (event.key === "Enter" && !event.shiftKey && !event.nativeEvent.isComposing) {
            event.preventDefault();
            if (canSend) onSend();
          }
        }}
        placeholder={placeholder}
        aria-label="输入消息"
        className="min-h-9 resize-none bg-transparent text-[13px] leading-[1.4] text-ink outline-none placeholder:text-ink-3"
      />
      <div className="flex items-center justify-between gap-2">
        <div className="flex min-w-0 items-center gap-2">{extra}</div>
        <button
          type="button"
          aria-label="发送"
          disabled={!canSend}
          onClick={onSend}
          className="flex size-7 items-center justify-center rounded-[8px] transition-[background-color,color,transform] duration-200 enabled:active:scale-[0.96]"
          style={{ background: canSend ? "var(--ink)" : "var(--line-strong)", color: canSend ? "var(--surface)" : "var(--ink-2)" }}
        >
          <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.4" strokeLinecap="round" strokeLinejoin="round">
            <path d="M12 19V5M5 12l7-7 7 7" />
          </svg>
        </button>
      </div>
    </div>
  );
}
