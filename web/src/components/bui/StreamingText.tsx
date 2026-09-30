/*
 * Adapted from Beautiful UI "Streaming Text" (https://www.beautifului.dev/),
 * MIT License, Copyright (c) 2026 Shane Levine. See web/licenses/beautifului-MIT.txt.
 * Local changes: the scripted token timer, inline source chips, remote-looking
 * source links/images, action icons and follow-up prompts are removed. The
 * component now renders caller-supplied plain text as a React text node and
 * shows the original blinking caret while status is "streaming".
 */
export default function StreamingText({ text, streaming }: { text: string; streaming: boolean }) {
  return (
    <p className="text-[13px] leading-relaxed whitespace-pre-wrap break-words text-ink">
      {text}
      {streaming && (
        <span
          aria-hidden
          className="ml-0.5 inline-block h-3 w-0.5 translate-y-0.5 rounded-full bg-ink"
          style={{ animation: "fade-in 150ms ease-out both" }}
        />
      )}
    </p>
  );
}
