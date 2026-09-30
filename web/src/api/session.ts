import { APIError, type Client } from "./client";
import type { A2UIStore } from "@/a2ui/store";

export type SyncStatus = "idle" | "loading" | "live" | "ended" | "reconnecting" | "error";

const RETRY_MS = [500, 1000, 2000, 5000];

/**
 * syncSession keeps store in step with one session until signal aborts:
 * GET render (full surface + cursor), then subscribe ui/events after that
 * cursor. Every reconnect repeats render→subscribe under a new store binding,
 * so frames from a superseded stream can never land in the current view.
 */
export async function syncSession(client: Client, sid: string, store: A2UIStore, signal: AbortSignal, onStatus: (s: SyncStatus, err?: unknown) => void) {
  let attempt = 0;
  while (!signal.aborted) {
    onStatus(attempt === 0 ? "loading" : "reconnecting");
    const token = store.reset("session:" + sid);
    let live = false;
    let ended = false;
    let resync = false;
    try {
      const { lines, cursor } = await client.render(sid, signal);
      for (const line of lines) store.apply(token, line);
      store.setCursor(token, cursor);
      await client.uiEvents(
        sid,
        cursor,
        (f) => {
          if (signal.aborted) return;
          switch (f.event) {
            case "ready":
              try {
                live = (JSON.parse(f.data) as { live?: unknown }).live === true;
              } catch {
                live = false;
              }
              onStatus(live ? "live" : "loading");
              attempt = 0;
              break;
            case "a2ui":
              store.apply(token, f.data, f.id);
              break;
            case "cursor":
              if (f.id) store.setCursor(token, f.id);
              break;
            case "resync":
              resync = true;
              break;
            case "end":
              ended = true;
              break;
          }
        },
        signal,
      );
    } catch (err) {
      if (signal.aborted) return;
      if (err instanceof APIError && err.status >= 400 && err.status < 500 && err.status !== 410) {
        onStatus("error", err);
        return;
      }
    }
    if (signal.aborted) return;
    if (ended || (!live && !resync)) {
      onStatus("ended");
      return;
    }
    const delay = resync ? 0 : RETRY_MS[Math.min(attempt, RETRY_MS.length - 1)];
    attempt++;
    await sleep(delay, signal);
  }
}

function sleep(ms: number, signal: AbortSignal) {
  return new Promise<void>((resolve) => {
    const t = setTimeout(resolve, ms);
    signal.addEventListener(
      "abort",
      () => {
        clearTimeout(t);
        resolve();
      },
      { once: true },
    );
  });
}
