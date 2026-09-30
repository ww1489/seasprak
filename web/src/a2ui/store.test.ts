import { describe, expect, it } from "vitest";
import { A2UIStore } from "./store";

const S = "session:s1";
const begin = JSON.stringify({ beginRendering: { surfaceId: S, root: "root" } });
const root = (...children: string[]) => ({ id: "root", component: { Column: { children } } });
const msg = (id: string, status = "final") => ({ id: "msg:" + id, component: { ChatMessage: { messageId: id, role: "assistant", status, dataKey: "text:" + id } } });
const su = (...components: unknown[]) => JSON.stringify({ surfaceUpdate: { surfaceId: S, components } });
const dm = (key: string, valueString: string) => JSON.stringify({ dataModelUpdate: { surfaceId: S, contents: [{ key, valueString }] } });

function state(store: A2UIStore) {
  const s = store.snapshot;
  return { root: s.root, components: Object.fromEntries(s.components), data: Object.fromEntries(s.data) };
}

describe("A2UIStore", () => {
  it("replaying a half group and duplicates is idempotent; cursor moves only on id frames", () => {
    const store = new A2UIStore(S);
    const t = store.reset(S, "c0");
    store.apply(t, begin);
    store.apply(t, su(msg("m1"), root("msg:m1")));
    expect(store.snapshot.cursor).toBe("c0");
    store.apply(t, dm("text:m1", "hello"), "c1");
    expect(store.snapshot.cursor).toBe("c1");
    const once = state(store);
    // Reconnect from c0 replays the whole group, including the first half again.
    store.apply(t, su(msg("m1"), root("msg:m1")));
    expect(store.snapshot.cursor).toBe("c1");
    store.apply(t, dm("text:m1", "hello"), "c1");
    store.apply(t, dm("text:m1", "hello"), "c1");
    expect(state(store)).toEqual(once);
    expect(store.snapshot.components.size).toBe(2);
  });

  it("streaming then final overwrites the same component and key", () => {
    const store = new A2UIStore(S);
    const t = store.reset(S);
    store.apply(t, begin);
    store.apply(t, su(msg("m1", "streaming"), root("msg:m1")));
    store.apply(t, dm("text:m1", "par"));
    store.apply(t, su(msg("m1"), root("msg:m1")));
    store.apply(t, dm("text:m1", "partial done"), "c9");
    expect(store.snapshot.data.get("text:m1")).toBe("partial done");
    const c = store.snapshot.components.get("msg:m1")!.component;
    expect("ChatMessage" in c && c.ChatMessage.status).toBe("final");
  });

  it("ignores late frames from a previous binding and foreign surfaces", () => {
    const store = new A2UIStore(S);
    const old = store.reset(S);
    store.apply(old, begin);
    const current = store.reset(S, "c5");
    expect(store.apply(old, su(msg("ghost"), root("msg:ghost")), "c99")).toBe(false);
    expect(store.snapshot.components.size).toBe(0);
    expect(store.snapshot.cursor).toBe("c5");
    store.apply(current, begin);
    expect(store.apply(current, JSON.stringify({ surfaceUpdate: { surfaceId: "session:other", components: [msg("x")] } }))).toBe(false);
    expect(store.apply(current, "not json")).toBe(false);
    expect(store.apply(current, JSON.stringify({ beginRendering: { surfaceId: S, root: "r" }, deleteSurface: { surfaceId: S } }))).toBe(false);
  });

  it("drops approvals from an older service instance", () => {
    const store = new A2UIStore(S);
    const t = store.reset(S);
    store.apply(t, begin);
    const appr = (id: string, instanceId: string) => ({ id: "approval:" + id, component: { Approval: { interactionId: id, traceId: "t", question: "q", options: ["allowed-once"], instanceId } } });
    store.apply(t, su(appr("i1", "inst-a")));
    store.apply(t, su(appr("i2", "inst-b")));
    expect([...store.snapshot.components.keys()]).toEqual(["approval:i2"]);
  });
});
