import { useRef, useState } from "react";
import { Client, errorText, newIdempotencyKey, type SessionEntry } from "./api/client";
import SessionView from "./SessionView";

/** Pending action keys are kept until success so a retry reuses the same key. */
function useActionKey() {
  const ref = useRef<string | null>(null);
  return {
    get: () => (ref.current ??= newIdempotencyKey()),
    done: () => {
      ref.current = null;
    },
  };
}

function TokenForm({ onToken }: { onToken: (t: string) => void }) {
  const [value, setValue] = useState("");
  return (
    <form
      aria-label="访问令牌"
      className="mx-auto mt-24 flex w-full max-w-md flex-col gap-3 rounded-card bg-surface p-5 shadow-card"
      onSubmit={(e) => {
        e.preventDefault();
        const t = value.trim();
        if (t) {
          setValue("");
          onToken(t);
        }
      }}
    >
      <h1 className="text-[15px] font-semibold">连接本地服务</h1>
      <p className="text-[12.5px] leading-normal text-ink-2">
        粘贴启动时写入“Bearer file”中的令牌。令牌只保存在当前页面内存中，刷新页面后需要重新输入。
      </p>
      <label className="flex flex-col gap-1 text-[12.5px] text-ink-2">
        令牌
        <input
          type="password"
          autoComplete="off"
          spellCheck={false}
          value={value}
          onChange={(e) => setValue(e.target.value)}
          className="rounded-control border border-line bg-field px-2.5 py-1.5 font-mono text-[12.5px] text-ink outline-none focus:border-line-strong"
        />
      </label>
      <button type="submit" disabled={!value.trim()} className="self-start rounded-control bg-ink px-3 py-1.5 text-[12.5px] font-medium text-surface disabled:opacity-50">
        连接
      </button>
    </form>
  );
}

export default function App() {
  const [client, setClient] = useState<Client | null>(null);
  const [sessions, setSessions] = useState<SessionEntry[]>([]);
  const [selected, setSelected] = useState("");
  const [workspace, setWorkspace] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const createKey = useActionKey();

  const refresh = async (c: Client) => {
    try {
      const list = await c.listSessions();
      setSessions(list.sessions);
      setError("");
      return true;
    } catch (err) {
      setError(errorText(err));
      return false;
    }
  };

  if (!client) {
    return (
      <main className="min-h-screen px-4">
        <TokenForm
          onToken={async (t) => {
            const c = new Client(t);
            if (await refresh(c)) setClient(c);
          }}
        />
        {error && (
          <p role="alert" className="mx-auto mt-3 max-w-md text-[12.5px] text-red">
            {error}
          </p>
        )}
      </main>
    );
  }

  const create = async () => {
    setBusy(true);
    try {
      const snap = await client.createSession(workspace.trim(), createKey.get());
      createKey.done();
      await refresh(client);
      setSelected(snap.sessionId);
    } catch (err) {
      setError(errorText(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="grid min-h-screen grid-cols-1 md:grid-cols-[260px_minmax(0,1fr)]">
      <aside className="flex min-w-0 flex-col gap-3 border-b border-line bg-canvas p-3 md:border-r md:border-b-0">
        <h1 className="text-[14px] font-semibold">会话</h1>
        <form
          aria-label="新建会话"
          className="flex flex-col gap-1.5"
          onSubmit={(e) => {
            e.preventDefault();
            void create();
          }}
        >
          <label className="flex flex-col gap-1 text-[12px] text-ink-2">
            工作区绝对路径
            <input
              value={workspace}
              onChange={(e) => {
                setWorkspace(e.target.value);
                createKey.done();
              }}
              spellCheck={false}
              className="rounded-control border border-line bg-surface px-2 py-1 font-mono text-[12px] text-ink outline-none"
            />
          </label>
          <button type="submit" disabled={busy || !workspace.trim()} className="rounded-control bg-ink px-2.5 py-1 text-[12.5px] font-medium text-surface disabled:opacity-50">
            新建会话
          </button>
        </form>
        <button type="button" onClick={() => void refresh(client)} className="self-start text-[12px] text-ink-2 hover:text-ink">
          刷新列表
        </button>
        {error && (
          <p role="alert" className="text-[12px] text-red">
            {error}
          </p>
        )}
        <ul aria-label="会话列表" className="flex flex-col gap-1">
          {sessions.map((s) => (
            <li key={s.sessionId}>
              <button
                type="button"
                aria-current={s.sessionId === selected}
                onClick={() => setSelected(s.sessionId)}
                className={`w-full truncate rounded-control px-2 py-1 text-left font-mono text-[12px] ${s.sessionId === selected ? "bg-surface shadow-hairline" : "hover:bg-hover-2"} ${s.available ? "text-ink" : "text-ink-3"}`}
              >
                {s.sessionId}
              </button>
            </li>
          ))}
          {sessions.length === 0 && <li className="text-[12px] text-ink-3">暂无会话</li>}
        </ul>
      </aside>
      <main className="min-w-0 p-4">{selected ? <SessionView key={selected} client={client} sid={selected} /> : <p className="text-[13px] text-ink-3">请选择或新建一个会话。</p>}</main>
    </div>
  );
}
